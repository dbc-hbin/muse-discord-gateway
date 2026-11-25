package bridge

import "errors"

// ReplyCancellation distinguishes deliberate non-delivery from successful send.
type ReplyCancellation struct {
	At   float64 `json:"at"`
	Code string  `json:"code"`
}

func replyCancelled(db *storeConn, id string) (bool, error) {
	var n int
	err := db.QueryRow("SELECT count(*) FROM reply_cancellations WHERE reply_id=?", id).Scan(&n)
	return n != 0, err
}

// cancelReplyIfUnsent must run inside the caller's transaction. False means the
// reply cannot safely be abandoned (sending/uncertain, or already fully sent).
// True also covers an existing cancellation, without changing its original code.
func cancelReplyIfUnsent(db *storeConn, id, code string) (bool, error) {
	if code == "" || symbolicCode(code) != code {
		return false, errors.New("invalid cancellation code")
	}
	d, err := delivery(db, id)
	if err != nil {
		return false, err
	}
	if d.State == "cancelled" {
		return true, nil
	}
	unsent := false
	for _, chunk := range d.Chunks {
		switch chunk.State {
		case "sending", "uncertain":
			return false, nil
		case "pending", "failed":
			unsent = true
		case "sent", "cancelled":
		default:
			return false, errors.New("unknown chunk state cannot be cancelled")
		}
	}
	if !unsent {
		return false, nil
	}
	if _, err = db.Exec("INSERT INTO reply_cancellations(reply_id,cancelled_at,code) VALUES(?,?,?)", id, epoch(), code); err != nil {
		return false, err
	}
	if _, err = db.Exec("UPDATE chunks SET state='cancelled',code=? WHERE reply_id=? AND state='pending'", code, id); err != nil {
		return false, err
	}
	return true, nil
}

// pendingBehindSQL matches when a pending chunk sits at or behind the
// outer reply (aliases r/i): the tail of the same reply, or any later
// reply in the same conversation. Such a pending chunk is exactly what a
// failed or uncertain head keeps from being sent.
const pendingBehindSQL = `EXISTS(SELECT 1 FROM chunks p JOIN replies r2 ON r2.id=p.reply_id JOIN inbound i2 ON i2.id=r2.inbound_id WHERE p.state='pending' AND i2.platform=i.platform AND json_extract(i2.envelope,'$.conversation_id')=json_extract(i.envelope,'$.conversation_id') AND (r2.created>r.created OR (r2.created=r.created AND r2.rowid>=r.rowid)))`

// FailedHeadExpirySeconds bounds how long a failed reply head may block
// its conversation. A chat reply that has been undeliverable for an hour
// is stale; expiring it (below) unblocks the conversation while keeping
// the failed chunk as evidence, exactly like an operator cancellation.
const FailedHeadExpirySeconds = 3600

// ExpireBlockedFailedReplies cancels, with code failed_head_expired,
// every reply that has a failed chunk, no sending or uncertain chunk,
// and pending work stuck behind it, once the reply is older than
// maxAge. Replies with an uncertain chunk are never expired: that POST
// may have been delivered, so only an operator may resolve them.
// Replies blocking nothing are left alone for a possible manual retry.
func (s *Store) ExpireBlockedFailedReplies(maxAge float64) (int, error) {
	v, err := s.call(func(db *storeConn) (any, error) {
		return transact(db, func(db *storeConn) (any, error) {
			rows, err := db.Query(`SELECT r.id FROM replies r JOIN inbound i ON i.id=r.inbound_id WHERE r.created<=? AND EXISTS(SELECT 1 FROM chunks c WHERE c.reply_id=r.id AND c.state='failed') AND NOT EXISTS(SELECT 1 FROM chunks c WHERE c.reply_id=r.id AND c.state IN ('sending','uncertain')) AND NOT EXISTS(SELECT 1 FROM reply_cancellations x WHERE x.reply_id=r.id) AND `+pendingBehindSQL+` ORDER BY r.created,r.rowid`, epoch()-maxAge)
			if err != nil {
				return nil, err
			}
			var ids []string
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					rows.Close()
					return nil, err
				}
				ids = append(ids, id)
			}
			if err = rows.Err(); err != nil {
				rows.Close()
				return nil, err
			}
			if err = rows.Close(); err != nil {
				return nil, err
			}
			n := 0
			for _, id := range ids {
				ok, err := cancelReplyIfUnsent(db, id, "failed_head_expired")
				if err != nil {
					return nil, err
				}
				if ok {
					n++
				}
			}
			return n, nil
		})
	})
	if err != nil {
		return 0, err
	}
	return v.(int), nil
}

// queueBlockage summarizes conversations whose pending chunks are held
// back by a failed or uncertain head, so a wedged queue is visible in
// Status instead of only being discovered when a reply goes missing.
func queueBlockage(db *storeConn) (map[string]any, error) {
	rows, err := db.Query(`SELECT min(r.created),max(CASE WHEN EXISTS(SELECT 1 FROM chunks u WHERE u.reply_id=r.id AND u.state='uncertain') THEN 1 ELSE 0 END) FROM chunks c JOIN replies r ON r.id=c.reply_id JOIN inbound i ON i.id=r.inbound_id WHERE c.state IN ('failed','uncertain') AND NOT EXISTS(SELECT 1 FROM reply_cancellations x WHERE x.reply_id=r.id) AND `+pendingBehindSQL+` GROUP BY i.platform,json_extract(i.envelope,'$.conversation_id')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	blocked, uncertain := 0, 0
	oldest := 0.0
	now := epoch()
	for rows.Next() {
		var created float64
		var hasUncertain int
		if err = rows.Scan(&created, &hasUncertain); err != nil {
			return nil, err
		}
		blocked++
		if hasUncertain != 0 {
			uncertain++
		}
		if age := now - created; age > oldest {
			oldest = age
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"blocked_conversations": blocked, "uncertain_blocked_conversations": uncertain, "oldest_block_seconds": int64(oldest)}, nil
}

// CancelReply durably abandons the unsent remainder of a reply. It preserves
// failed and sent chunks as evidence, including their attempts, codes and IDs.
// The cancellation and pending-chunk transitions commit atomically with queue
// advancement. It never deletes data, marks a failure sent, or retries a POST.
//
// Sending or uncertain chunks must be reconciled first; cancellation must not
// release later replies while an earlier POST may still be in flight or unacked.
// A cancelled reply can never be requeued, including after restart.
func (s *Store) CancelReply(id string) (Delivery, error) {
	v, err := s.call(func(db *storeConn) (any, error) {
		return transact(db, func(db *storeConn) (any, error) {
			cancelled, err := cancelReplyIfUnsent(db, id, "operator_cancelled")
			if err != nil {
				return nil, err
			}
			if !cancelled {
				return nil, errors.New("only unsent replies without sending or uncertain chunks can be cancelled")
			}
			return delivery(db, id)
		})
	})
	if err != nil {
		return Delivery{}, err
	}
	return v.(Delivery), nil
}
