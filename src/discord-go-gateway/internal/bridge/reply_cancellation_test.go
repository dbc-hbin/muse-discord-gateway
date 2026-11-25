package bridge

import (
	"reflect"
	"strings"
	"testing"
)

func TestCancelFailedReplyAdvancesConversationDurably(t *testing.T) {
	s, path := testStore(t)
	ingestLedger(t, s, "deleted-original", "same")
	first := replyLedger(t, s, claimLedger(t, s), strings.Repeat("a", 3801))
	ingestLedger(t, s, "later", "same")
	second := replyLedger(t, s, claimLedger(t, s), "next answer")
	chunk, err := s.NextChunk()
	if err != nil || chunk == nil || chunk.ReplyID != first {
		t.Fatal(chunk, err)
	}
	if err := s.RecordResult(*chunk, SendResult{State: "failed", Code: "http_400"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Delivery(first)
	if err != nil {
		t.Fatal(err)
	}
	if next, err := s.NextChunk(); err != nil || next != nil {
		t.Fatal("failed head must remain blocked until explicit cancellation", next, err)
	}
	d, err := s.CancelReply(first)
	if err != nil || d.State != "cancelled" || !reflect.DeepEqual(d.Chunks[0], before.Chunks[0]) {
		t.Fatal("cancellation changed failed evidence", d, before, err)
	}
	for _, c := range d.Chunks[1:] {
		if c.State != "cancelled" || c.Attempts != 0 || c.MessageID != nil {
			t.Fatal("unsent suffix was not cancelled", c)
		}
	}
	if again, err := s.CancelReply(first); err != nil || !reflect.DeepEqual(again, d) {
		t.Fatal("cancellation is not idempotent", again, err)
	}
	if d.Cancellation == nil || d.Cancellation.Code != "operator_cancelled" || d.Cancellation.At <= 0 {
		t.Fatal("missing durable cancellation reason", d)
	}
	if status, err := s.Status(); err != nil || status["cancelled_replies"] != 1 {
		t.Fatal("status hides cancellation", status, err)
	}
	if n, err := s.RetryFailed(first); err == nil || n != 0 {
		t.Fatal("cancelled failed chunk resurrected", n, err)
	}
	if err := s.ResolveSent(first, 0, "invented"); err == nil {
		t.Fatal("failed cancellation marked sent")
	}
	if rows, err := s.FeedbackRows(); err != nil || rows[0].Delivery != "failed" || rows[0].Thinking {
		t.Fatal("cancellation must not report success or typing", rows, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, ledgerPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if after, err := reopened.Delivery(first); err != nil || !reflect.DeepEqual(d, after) {
		t.Fatal("restart changed cancellation", after, err)
	}
	if n, err := reopened.RetryFailed(first); err == nil || n != 0 {
		t.Fatal("restart resurrected cancellation", n, err)
	}
	next, err := reopened.NextChunk()
	if err != nil || next == nil || next.ReplyID != second || next.Source.EventID != "later" || next.Source.ConversationID != "same" {
		t.Fatal("later reply did not retain exact route", next, err)
	}
}

func TestCancelReplyPreservesSentPrefixAndRejectsAmbiguous(t *testing.T) {
	for _, state := range []string{"pending", "sending", "uncertain", "failed", "sent"} {
		t.Run(state, func(t *testing.T) {
			s, _ := testStore(t)
			ingestLedger(t, s, "one", "same")
			id := replyLedger(t, s, claimLedger(t, s), strings.Repeat("a", 2000))
			first, _ := s.NextChunk()
			if err := s.RecordResult(*first, SendResult{State: "sent", MessageID: "verified-first"}); err != nil {
				t.Fatal(err)
			}
			if state != "pending" {
				second, _ := s.NextChunk()
				if state != "sending" {
					if err := s.RecordResult(*second, SendResult{State: state, MessageID: map[string]string{"sent": "verified-second"}[state], Code: "test_result"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, _ := s.Delivery(id)
			d, err := s.CancelReply(id)
			if state == "pending" || state == "failed" {
				if err != nil || d.State != "cancelled" || !reflect.DeepEqual(before.Chunks[0], d.Chunks[0]) {
					t.Fatal("sent prefix changed", before, d, err)
				}
			} else {
				if err == nil {
					t.Fatal("cancelled unsafe or completed reply", state)
				}
				after, _ := s.Delivery(id)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("rejected cancellation mutated ledger", before, after)
				}
			}
		})
	}
}

func TestCancelReplyTransactionRollbackAndOrdering(t *testing.T) {
	s, _ := testStore(t)
	ingestLedger(t, s, "first", "same")
	first := replyLedger(t, s, claimLedger(t, s), strings.Repeat("a", 2000))
	ingestLedger(t, s, "later", "same")
	replyLedger(t, s, claimLedger(t, s), "later")
	_, err := s.call(func(db *storeConn) (any, error) {
		_, err := db.Exec("CREATE TRIGGER fail_cancel BEFORE UPDATE ON chunks WHEN NEW.state='cancelled' BEGIN SELECT RAISE(ABORT,'test_failure'); END")
		return nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.Delivery(first)
	if _, err := s.CancelReply(first); err == nil {
		t.Fatal("missing injected failure")
	}
	after, _ := s.Delivery(first)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("partial cancellation commit", before, after)
	}
	next, err := s.NextChunk()
	if err != nil || next == nil || next.ReplyID != first {
		t.Fatal("rollback released later reply", next, err)
	}
	if _, err := s.CancelReply("unknown"); err == nil {
		t.Fatal("unknown reply accepted")
	}
}

func TestCancelReplyRacesDispatcherAtomically(t *testing.T) {
	for i := 0; i < 12; i++ {
		s, path := testStore(t)
		other, err := OpenStore(path, ledgerPolicy{})
		if err != nil {
			t.Fatal(err)
		}
		ingestLedger(t, s, "one", "same")
		id := replyLedger(t, s, claimLedger(t, s), "answer")
		start := make(chan struct{})
		cancelled := make(chan error, 1)
		go func() {
			<-start
			_, err := other.CancelReply(id)
			cancelled <- err
		}()
		close(start)
		chunk, err := s.NextChunk()
		if err != nil {
			t.Fatal(err)
		}
		cancelErr := <-cancelled
		d, err := s.Delivery(id)
		if err != nil {
			t.Fatal(err)
		}
		if chunk == nil {
			if cancelErr != nil || d.State != "cancelled" || d.Chunks[0].Attempts != 0 {
				t.Fatal("cancel won but dispatch state changed", d, cancelErr)
			}
		} else if cancelErr == nil || d.State != "sending" || d.Cancellation != nil || d.Chunks[0].Attempts != 1 {
			t.Fatal("dispatch won but in-flight chunk was cancelled", d, cancelErr)
		}
		other.Close()
		s.Close()
	}
}

// A failed head blocks its conversation until resolved (by design), but
// the expiry sweep must release it once it is stale, with the failed
// chunk preserved as evidence — the 2026-11-25 boost incident wedged
// #일반 for ~10 hours because nothing bounded the wait.
func TestExpireBlockedFailedRepliesReleasesConversation(t *testing.T) {
	s, _ := testStore(t)
	ingestLedger(t, s, "one", "same")
	first := replyLedger(t, s, claimLedger(t, s), "first answer")
	ingestLedger(t, s, "two", "same")
	second := replyLedger(t, s, claimLedger(t, s), "second answer")
	chunk, err := s.NextChunk()
	if err != nil || chunk == nil || chunk.ReplyID != first {
		t.Fatal(chunk, err)
	}
	if err = s.RecordResult(*chunk, SendResult{State: "failed", Code: "http_400"}); err != nil {
		t.Fatal(err)
	}
	if next, err := s.NextChunk(); err != nil || next != nil {
		t.Fatal("failed head must block until resolved", next, err)
	}
	status, err := s.Status()
	if err != nil {
		t.Fatal(err)
	}
	queue, _ := status["queue"].(map[string]any)
	if queue["blocked_conversations"] != 1 || queue["uncertain_blocked_conversations"] != 0 {
		t.Fatal("status hides the blockage", status["queue"])
	}
	// Not yet stale: a generous maxAge leaves the head for the operator.
	if n, err := s.ExpireBlockedFailedReplies(3600); err != nil || n != 0 {
		t.Fatal("fresh failure expired early", n, err)
	}
	if next, err := s.NextChunk(); err != nil || next != nil {
		t.Fatal("fresh failure must still block", next, err)
	}
	// Stale: the sweep cancels it like an operator would.
	if n, err := s.ExpireBlockedFailedReplies(0); err != nil || n != 1 {
		t.Fatal("stale failure not expired", n, err)
	}
	d, err := s.Delivery(first)
	if err != nil || d.State != "cancelled" || d.Cancellation == nil || d.Cancellation.Code != "failed_head_expired" {
		t.Fatal("expiry is not a durable cancellation", d, err)
	}
	if d.Chunks[0].State != "failed" {
		t.Fatal("failed evidence was rewritten", d.Chunks[0])
	}
	next, err := s.NextChunk()
	if err != nil || next == nil || next.ReplyID != second {
		t.Fatal("conversation still wedged after expiry", next, err)
	}
	status, err = s.Status()
	if err != nil {
		t.Fatal(err)
	}
	queue, _ = status["queue"].(map[string]any)
	if queue["blocked_conversations"] != 0 {
		t.Fatal("status still reports the released blockage", status["queue"])
	}
}

// The sweep must not touch a failed reply that blocks nothing (an
// operator may still retry it), nor any reply with an uncertain chunk
// (that POST may have been delivered).
func TestExpireBlockedFailedRepliesLeavesUnblockedAndUncertain(t *testing.T) {
	s, _ := testStore(t)
	ingestLedger(t, s, "one", "same")
	lonely := replyLedger(t, s, claimLedger(t, s), "lonely answer")
	chunk, err := s.NextChunk()
	if err != nil || chunk == nil || chunk.ReplyID != lonely {
		t.Fatal(chunk, err)
	}
	if err = s.RecordResult(*chunk, SendResult{State: "failed", Code: "http_400"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ExpireBlockedFailedReplies(0); err != nil || n != 0 {
		t.Fatal("unblocking failure was expired", n, err)
	}
	if n, err := s.RetryFailed(lonely); err != nil || n != 1 {
		t.Fatal("operator retry lost", n, err)
	}
	chunk, err = s.NextChunk()
	if err != nil || chunk == nil {
		t.Fatal(chunk, err)
	}
	if err = s.RecordResult(*chunk, SendResult{State: "sent", MessageID: "remote"}); err != nil {
		t.Fatal(err)
	}

	ingestLedger(t, s, "two", "same")
	uncertain := replyLedger(t, s, claimLedger(t, s), "maybe sent")
	ingestLedger(t, s, "three", "same")
	replyLedger(t, s, claimLedger(t, s), "waiting answer")
	chunk, err = s.NextChunk()
	if err != nil || chunk == nil || chunk.ReplyID != uncertain {
		t.Fatal(chunk, err)
	}
	if err = s.RecordResult(*chunk, SendResult{State: "uncertain", Code: "request_or_ack_failed"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ExpireBlockedFailedReplies(0); err != nil || n != 0 {
		t.Fatal("uncertain head was expired", n, err)
	}
	if next, err := s.NextChunk(); err != nil || next != nil {
		t.Fatal("uncertain head must keep blocking", next, err)
	}
	status, err := s.Status()
	if err != nil {
		t.Fatal(err)
	}
	queue, _ := status["queue"].(map[string]any)
	if queue["blocked_conversations"] != 1 || queue["uncertain_blocked_conversations"] != 1 {
		t.Fatal("status hides the uncertain blockage", status["queue"])
	}
}
