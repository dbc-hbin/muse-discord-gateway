package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func catchupFixture(t *testing.T) (*Store, Settings, time.Time) {
	t.Helper()
	s, path := threadStore(t)
	cfg := Settings{DBPath: path, Policy: threadPolicy(), ExpectedBotID: "4"}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := s.ActivateCatchup(cfg, now); err != nil {
		t.Fatal(err)
	}
	return s, cfg, now
}
func catchupAdd(id string, n uint64) string {
	v, _ := strconv.ParseUint(id, 10, 64)
	return strconv.FormatUint(v+n, 10)
}
func catchupMessage(t *testing.T, route ReadRoute, id, author string) *discordgo.Message {
	t.Helper()
	raw, _ := json.Marshal(readFixtureMessage(id, route.ChannelID, author))
	m, err := decodeReadMessage(raw, route)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func catchupForceGap(t *testing.T, s *Store, upper string) *CatchupStatus {
	t.Helper()
	_, err := s.call(func(db *storeConn) (any, error) { return nil, requestCatchupDB(db, "2", upper, "test_gap") })
	if err != nil {
		t.Fatal(err)
	}
	in, err := s.nextCatchup(epoch() + 100)
	if err != nil || in == nil {
		t.Fatal(in, err)
	}
	return in
}
func catchupRow(t *testing.T, s *Store) CatchupStatus {
	t.Helper()
	v, err := s.call(func(db *storeConn) (any, error) {
		return scanCatchup(db.QueryRow(`SELECT channel_id,route,floor,upper_id,scan_before,state,code,attempts,next_attempt,generation,page_limit FROM catchup_routes WHERE channel_id='2'`))
	})
	if err != nil {
		t.Fatal(err)
	}
	return v.(CatchupStatus)
}
func TestCatchupFirstActivationProspectiveAndIntactRestart(t *testing.T) {
	s, path := threadStore(t)
	old := threadEnvelope()
	old.RouteKind = "guild_text"
	old.ConversationID = "2"
	old.ParentChannelID = ""
	old.ThreadType = 0
	if _, err := s.Ingest(old); err != nil {
		t.Fatal(err)
	}
	cfg := Settings{DBPath: path, Policy: threadPolicy(), ExpectedBotID: "4"}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := s.ActivateCatchup(cfg, now); err != nil {
		t.Fatal(err)
	}
	row := catchupRow(t, s)
	if row.Floor != snowPrevious(snowAt(now.Add(time.Millisecond))) || row.State != "idle" {
		t.Fatal(row)
	}
	if in, _ := s.nextCatchup(epoch() + 100); in != nil {
		t.Fatal("first activation replayed historic inbox", in)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.ActivateCatchup(cfg, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	row = catchupRow(t, reopened)
	if row.State != "gap" || row.Floor != snowPrevious(snowAt(now.Add(time.Millisecond))) || row.Upper != snowPrevious(snowAt(now.Add(time.Minute))) {
		t.Fatal(row)
	}
}
func TestCatchupNewestPagesDoNotAdvanceFloorAndOlderAdmissionFirst(t *testing.T) {
	s, cfg, _ := catchupFixture(t)
	floor := catchupRow(t, s).Floor
	in := catchupForceGap(t, s, catchupAdd(floor, 250))
	newer := []*discordgo.Message{}
	for n := uint64(250); n > 150; n-- {
		newer = append(newer, catchupMessage(t, in.Route, catchupAdd(floor, n), "1"))
	}
	if out, err := s.applyCatchupPage(cfg, *in, newer); err != nil || out != "page_pending" {
		t.Fatal(out, err)
	}
	row := catchupRow(t, s)
	if row.Floor != floor || row.Before != catchupAdd(floor, 151) {
		t.Fatal(row)
	}
	if next, _ := s.NextValidation(epoch()); next != nil {
		t.Fatal("newest page admitted before older page")
	}
	// A newer live request is quarantined behind the incomplete window.
	live := catchupMessage(t, in.Route, catchupAdd(floor, 300), "1")
	if out, err := receiveMessage(context.Background(), nil, s, cfg, live); err != nil || out != "validation_staged" {
		t.Fatal(out, err)
	}
	if next, _ := s.NextValidation(epoch()); next != nil {
		t.Fatal("newer live overtook gap")
	}
	in, _ = s.nextCatchup(epoch() + 100)
	oldest := []*discordgo.Message{catchupMessage(t, in.Route, catchupAdd(floor, 2), "1"), catchupMessage(t, in.Route, catchupAdd(floor, 1), "1")}
	if out, err := s.applyCatchupPage(cfg, *in, oldest); err != nil || out != "progress" {
		t.Fatal(out, err)
	}
	next, err := s.NextValidation(epoch())
	if err != nil || next == nil || next.Event.EventID != catchupAdd(floor, 1) {
		t.Fatal(next, err)
	}
	if out, err := s.PromoteValidation(*next); err != nil || out != "accepted" {
		t.Fatal(out, err)
	}
	claim, err := s.ClaimNext(30, 0)
	if err != nil || claim == nil || claim.Envelope.EventID != catchupAdd(floor, 1) {
		t.Fatal(claim, err)
	}
}
func TestCatchupQueueFullDoesNotAdvancePastUnadmittedAndResumes(t *testing.T) {
	s, cfg, _ := catchupFixture(t)
	floor := catchupRow(t, s).Floor
	in := catchupForceGap(t, s, catchupAdd(floor, 10))
	// Fill ordinary capacity with unrelated earlier work. Recovery cannot evict it.
	_, err := s.call(func(db *storeConn) (any, error) {
		return transact(db, func(db *storeConn) (any, error) {
			for n := 1; n <= 1000; n++ {
				e := threadEnvelope()
				e.RouteKind = "dm"
				e.ConversationID = "7"
				e.GuildID = ""
				e.ParentChannelID = ""
				e.ThreadType = 0
				e.EventID = strconv.Itoa(n)
				raw, _ := json.Marshal(e)
				if _, e := db.Exec(`INSERT INTO inbound(id,platform,event_id,envelope,created)VALUES(?,'discord',?,?,?)`, fmt.Sprint(n), fmt.Sprint(n), string(raw), epoch()); e != nil {
					return nil, e
				}
			}
			return nil, nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	page := []*discordgo.Message{catchupMessage(t, in.Route, catchupAdd(floor, 2), "1"), catchupMessage(t, in.Route, catchupAdd(floor, 1), "1")}
	if out, err := s.applyCatchupPage(cfg, *in, page); err != nil || out != "queue_full" {
		t.Fatal(out, err)
	}
	row := catchupRow(t, s)
	if row.Floor != floor || row.Before != in.Before || row.Code != "queue_full" {
		t.Fatal(row)
	}
	s.call(func(db *storeConn) (any, error) {
		_, e := db.Exec(`UPDATE inbound SET state='ignored' WHERE id='1'`)
		return nil, e
	})
	in, _ = s.nextCatchup(epoch() + 100)
	if out, err := s.applyCatchupPage(cfg, *in, page); err != nil || out != "queue_full" {
		t.Fatal(out, err)
	}
	row = catchupRow(t, s)
	if row.Floor != catchupAdd(floor, 1) {
		t.Fatal("advanced past unadmitted second request", row)
	}
	s.call(func(db *storeConn) (any, error) {
		_, e := db.Exec(`UPDATE inbound SET state='ignored' WHERE id='2'`)
		return nil, e
	})
	in, _ = s.nextCatchup(epoch() + 100)
	if _, err := s.applyCatchupPage(cfg, *in, page); err != nil {
		t.Fatal(err)
	}
	row = catchupRow(t, s)
	if row.State != "idle" || row.Floor != catchupAdd(floor, 10) {
		t.Fatal(row)
	}
}
func TestCatchupTombstonesAndUnknownMutationRace(t *testing.T) {
	s, cfg, _ := catchupFixture(t)
	floor := catchupRow(t, s).Floor
	in := catchupForceGap(t, s, catchupAdd(floor, 10))
	m := catchupMessage(t, in.Route, catchupAdd(floor, 1), "1")
	if _, err := receiveSourceEvent(context.Background(), nil, s, cfg, sourceGatewayEvent{Delete: &discordgo.Message{ID: m.ID, ChannelID: "2", GuildID: "5"}}); err != nil {
		t.Fatal(err)
	}
	if out, err := s.applyCatchupPage(cfg, *in, []*discordgo.Message{m}); err != nil || out != "stale" {
		t.Fatal(out, err)
	}
	if row := catchupRow(t, s); row.Floor != floor {
		t.Fatal(row)
	}
	// Existing tombstones always beat a later recovered CREATE.
	if _, err := receiveMessage(context.Background(), nil, s, cfg, m); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteSource("2", "5", m.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	in, _ = s.nextCatchup(epoch() + 100)
	if _, err := s.applyCatchupPage(cfg, *in, []*discordgo.Message{m}); err != nil {
		t.Fatal(err)
	}
	if next, _ := s.NextValidation(epoch()); next != nil {
		t.Fatal("deleted source resurrected", next)
	}
}
func TestCatchupFixedUpperRetainsSuccessorGap(t *testing.T) {
	s, cfg, _ := catchupFixture(t)
	floor := catchupRow(t, s).Floor
	in := catchupForceGap(t, s, catchupAdd(floor, 10))
	later := catchupAdd(floor, 20)
	catchupForceGap(t, s, later)
	if row := catchupRow(t, s); row.Upper != in.Upper {
		t.Fatal("moving upper", row)
	}
	if _, err := s.applyCatchupPage(cfg, *in, nil); err != nil {
		t.Fatal(err)
	}
	row := catchupRow(t, s)
	if row.State != "gap" || row.Floor != in.Upper || row.Upper != later {
		t.Fatal(row)
	}
}
func TestCatchupCopiedAndRestoredLedgerStayDisarmed(t *testing.T) {
	s, cfg, now := catchupFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	dest := filepath.Join(dir, "copy.db")
	raw, err := os.ReadFile(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(dest, raw, 0600); err != nil {
		t.Fatal(err)
	}
	witness, _ := os.ReadFile(cfg.DBPath + ".catchup-live")
	os.WriteFile(dest+".catchup-live", witness, 0600)
	copyStore, err := OpenStore(dest, cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	defer copyStore.Close()
	cfg.DBPath = dest
	if err = copyStore.ActivateCatchup(cfg, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	status, err := copyStore.CatchupStatus()
	if err != nil || status["state"] != "disarmed_continuity" {
		t.Fatal(status, err)
	}
	empty, path := threadStore(t)
	empty.call(func(db *storeConn) (any, error) {
		_, e := db.Exec(`INSERT INTO catchup_meta VALUES('state','disarmed_restore')`)
		return nil, e
	})
	cfg.DBPath = path
	if err = empty.ActivateCatchup(cfg, now); err != nil {
		t.Fatal(err)
	}
	status, err = empty.CatchupStatus()
	if err != nil || status["state"] != "disarmed_restore" || len(status["routes"].([]CatchupStatus)) != 0 {
		t.Fatal(status, err)
	}
}
func TestCatchupReadFailureRetainsCursor(t *testing.T) {
	for _, code := range []int{429, 500, 403} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s, _, _ := catchupFixture(t)
			floor := catchupRow(t, s).Floor
			in := catchupForceGap(t, s, catchupAdd(floor, 10))
			r := readREST(t, func(w http.ResponseWriter, q *http.Request) bool {
				if q.URL.Path == "/channels/2/messages" {
					w.WriteHeader(code)
					fmt.Fprint(w, `{}`)
					return true
				}
				return false
			})
			if _, err := r.historyPage(context.Background(), in.Route, in.Before, 100); err == nil {
				t.Fatal("accepted failure")
			} else if err = s.deferCatchup(*in, err.Error()); err != nil {
				t.Fatal(err)
			}
			row := catchupRow(t, s)
			if row.Floor != floor || row.Upper != in.Upper || row.Before != in.Before || row.State != "gap" || row.Attempts != 1 {
				t.Fatal(row)
			}
		})
	}
}

func TestCatchupHourWindowsDoNotDropRemainder(t *testing.T) {
	s, cfg, _ := catchupFixture(t)
	floor := catchupRow(t, s).Floor
	n, _ := strconv.ParseUint(floor, 10, 64)
	target := strconv.FormatUint(n+(uint64(3*time.Hour/time.Millisecond)<<22), 10)
	in := catchupForceGap(t, s, target)
	for window := 0; window < 3; window++ {
		if in == nil {
			t.Fatal("dropped successor", window)
		}
		lo, _ := strconv.ParseUint(in.Floor, 10, 64)
		hi, _ := strconv.ParseUint(in.Upper, 10, 64)
		if hi-lo > uint64(time.Hour/time.Millisecond)<<22 {
			t.Fatal("unbounded interval")
		}
		if _, err := s.applyCatchupPage(cfg, *in, nil); err != nil {
			t.Fatal(err)
		}
		in, _ = s.nextCatchup(epoch() + 100)
	}
	row := catchupRow(t, s)
	if row.State != "idle" || row.Floor != target {
		t.Fatal(row)
	}
}
func TestCatchupSameMillisecondClaimOrderAfterAllPromotions(t *testing.T) {
	s, cfg, _ := catchupFixture(t)
	floor := catchupRow(t, s).Floor
	in := catchupForceGap(t, s, catchupAdd(floor, 50))
	var page []*discordgo.Message
	for i := uint64(20); i > 0; i-- {
		page = append(page, catchupMessage(t, in.Route, catchupAdd(floor, i), "1"))
	}
	if _, err := s.applyCatchupPage(cfg, *in, page); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		next, err := s.NextValidation(epoch())
		if err != nil || next == nil || next.Event.EventID != catchupAdd(floor, uint64(i)) {
			t.Fatal(i, next, err)
		}
		if _, err = s.PromoteValidation(*next); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 20; i++ {
		claim, err := s.ClaimNext(30, 0)
		if err != nil || claim == nil || claim.Envelope.EventID != catchupAdd(floor, uint64(i)) {
			t.Fatal(i, claim, err)
		}
		if err = s.Ignore(claim.InboundID, claim.Claim); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCatchupFloorSealDetectsInPlaceOldCheckpoint(t *testing.T) {
	s, cfg, now := catchupFixture(t)
	s.Close()
	old, err := os.ReadFile(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(cfg.DBPath, cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	floor := catchupRow(t, s).Floor
	in := catchupForceGap(t, s, catchupAdd(floor, 10))
	m := catchupMessage(t, in.Route, catchupAdd(floor, 1), "1")
	if _, err = s.applyCatchupPage(cfg, *in, []*discordgo.Message{m}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Keep the same inode, path and current external witness; roll back only DB.
	if err = os.WriteFile(cfg.DBPath, old, 0600); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(cfg.DBPath, cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.ActivateCatchup(cfg, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	status, err := s.CatchupStatus()
	if err != nil || status["state"] != "disarmed_continuity" {
		t.Fatal(status, err)
	}
}
func TestCatchupMalformedApplyDoesNotPartiallyAdmit(t *testing.T) {
	s, cfg, _ := catchupFixture(t)
	floor := catchupRow(t, s).Floor
	in := catchupForceGap(t, s, catchupAdd(floor, 10))
	good := catchupMessage(t, in.Route, catchupAdd(floor, 2), "1")
	bad := catchupMessage(t, in.Route, catchupAdd(floor, 1), "1")
	bad.ChannelID = "99"
	if _, err := s.applyCatchupPage(cfg, *in, []*discordgo.Message{good, bad}); err == nil {
		t.Fatal("cross-route page applied")
	}
	if row := catchupRow(t, s); row.Floor != floor {
		t.Fatal(row)
	}
	if next, _ := s.NextValidation(epoch()); next != nil {
		t.Fatal(next)
	}
}

func TestCatchupLiveAboveUpperRemainsFencedInSuccessor(t *testing.T) {
	s, cfg, _ := catchupFixture(t)
	floor := catchupRow(t, s).Floor
	in := catchupForceGap(t, s, catchupAdd(floor, 10))
	live := catchupMessage(t, in.Route, catchupAdd(floor, 20), "1")
	if _, err := receiveMessage(context.Background(), nil, s, cfg, live); err != nil {
		t.Fatal(err)
	}
	if row := catchupRow(t, s); row.Upper != in.Upper || row.Floor != floor {
		t.Fatal("live changed fixed window", row)
	}
	if _, err := s.applyCatchupPage(cfg, *in, nil); err != nil {
		t.Fatal(err)
	}
	row := catchupRow(t, s)
	if row.State != "gap" || row.Floor != in.Upper || row.Upper != live.ID {
		t.Fatal("successor lost", row)
	}
	if next, err := s.NextValidation(epoch()); err != nil || next != nil {
		t.Fatal("live overtook uncovered successor", next, err)
	}
	in, err := s.nextCatchup(epoch() + 100)
	if err != nil || in == nil {
		t.Fatal(in, err)
	}
	if _, err = s.applyCatchupPage(cfg, *in, []*discordgo.Message{live}); err != nil {
		t.Fatal(err)
	}
	if next, err := s.NextValidation(epoch()); err != nil || next == nil || next.Event.EventID != live.ID {
		t.Fatal(next, err)
	}
}

// Catchup must stage exactly what the live path would stage on the
// extra guild route: a stranger's bot mention and a boost system
// message were silently dropped by the old owner-only page filter, and
// the floor advanced past them (permanent loss, incl. hall-of-fame
// boost cards missed during an outage).
func TestCatchupStagesExtraGuildStrangerAndBoost(t *testing.T) {
	s, path := autoGreetStore(t)
	cfg := Settings{DBPath: path, Policy: autoGreetPolicy(), ExpectedBotID: "999999999999999999"}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := s.ActivateCatchup(cfg, now); err != nil {
		t.Fatal(err)
	}
	baseline := snowPrevious(snowAt(now.Add(time.Millisecond)))
	route := Envelope{ConversationID: "777777777777777777", GuildID: "666666666666666666", RouteKind: "guild_text"}
	if _, err := s.call(func(db *storeConn) (any, error) {
		return nil, catchupRegisterDB(db, route, baseline)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.call(func(db *storeConn) (any, error) {
		return nil, requestCatchupDB(db, "777777777777777777", catchupAdd(baseline, 250), "test_gap")
	}); err != nil {
		t.Fatal(err)
	}
	in, err := s.nextCatchup(epoch() + 100)
	if err != nil || in == nil || in.ChannelID != "777777777777777777" {
		t.Fatalf("gap not due: %#v %v", in, err)
	}
	bot := &discordgo.User{ID: "999999999999999999"}
	stranger := &discordgo.User{ID: "424242424242424242", Username: "crew"}
	mk := func(id string, typ discordgo.MessageType, content string, mentions []*discordgo.User) *discordgo.Message {
		return &discordgo.Message{ID: id, ChannelID: "777777777777777777", GuildID: "666666666666666666", Type: typ, Author: stranger, Content: content, Mentions: mentions}
	}
	page := []*discordgo.Message{
		mk(catchupAdd(baseline, 3), discordgo.MessageTypeUserPremiumGuildSubscription, "", nil),
		mk(catchupAdd(baseline, 2), discordgo.MessageTypeDefault, "<@999999999999999999> 가격 얼마야", []*discordgo.User{bot}),
		mk(catchupAdd(baseline, 1), discordgo.MessageTypeDefault, "그냥 잡담", nil),
	}
	if out, err := s.applyCatchupPage(cfg, *in, page); err != nil {
		t.Fatalf("apply page: %q %v", out, err)
	}
	staged := map[string]bool{}
	if _, err := s.call(func(db *storeConn) (any, error) {
		rows, err := db.Query(`SELECT envelope FROM ingress_validation`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			if err = rows.Scan(&raw); err != nil {
				return nil, err
			}
			var e Envelope
			if err = json.Unmarshal([]byte(raw), &e); err != nil {
				return nil, err
			}
			staged[e.EventID] = true
		}
		return nil, rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if !staged[catchupAdd(baseline, 3)] {
		t.Error("boost message was not staged by catchup")
	}
	if !staged[catchupAdd(baseline, 2)] {
		t.Error("stranger mention was not staged by catchup")
	}
	if staged[catchupAdd(baseline, 1)] {
		t.Error("unmentioned stranger chat must not be staged")
	}
}

func TestLiveMessageAdmissibleMatchesRoutes(t *testing.T) {
	cfg := Settings{Policy: autoGreetPolicy(), ExpectedBotID: "999999999999999999"}
	owner := &discordgo.User{ID: "111111111111111111"}
	stranger := &discordgo.User{ID: "424242424242424242"}
	bot := &discordgo.User{ID: "999999999999999999", Bot: true}
	base := func() *discordgo.Message {
		return &discordgo.Message{ID: "1", ChannelID: "777777777777777777", GuildID: "666666666666666666", Type: discordgo.MessageTypeDefault, Author: stranger, Content: "hi"}
	}
	cases := []struct {
		name   string
		mutate func(*discordgo.Message)
		want   bool
	}{
		{"stranger in extra guild", func(m *discordgo.Message) {}, true},
		{"owner in extra guild", func(m *discordgo.Message) { m.Author = owner }, true},
		{"owner DM", func(m *discordgo.Message) { m.Author = owner; m.GuildID = ""; m.ChannelID = "313131313131313131" }, true},
		{"stranger DM", func(m *discordgo.Message) { m.GuildID = ""; m.ChannelID = "313131313131313131" }, false},
		{"stranger in primary guild", func(m *discordgo.Message) { m.GuildID = "555555555555555555"; m.ChannelID = "222222222222222222" }, false},
		{"bot author", func(m *discordgo.Message) { m.Author = bot }, false},
		{"webhook", func(m *discordgo.Message) { m.WebhookID = "1" }, false},
		{"join in extra home channel", func(m *discordgo.Message) { m.Type = discordgo.MessageTypeGuildMemberJoin }, true},
		{"join in other extra channel", func(m *discordgo.Message) { m.Type = discordgo.MessageTypeGuildMemberJoin; m.ChannelID = "888888888888888888" }, false},
		{"boost in extra guild", func(m *discordgo.Message) { m.Type = discordgo.MessageTypeUserPremiumGuildSubscriptionTierOne }, true},
		{"boost in primary guild", func(m *discordgo.Message) { m.Type = discordgo.MessageTypeUserPremiumGuildSubscription; m.GuildID = "555555555555555555"; m.ChannelID = "222222222222222222" }, false},
		{"foreign guild", func(m *discordgo.Message) { m.GuildID = "121212121212121212"; m.Author = owner }, false},
	}
	for _, tc := range cases {
		m := base()
		tc.mutate(m)
		if got := liveMessageAdmissible(cfg, m); got != tc.want {
			t.Errorf("%s: liveMessageAdmissible = %v, want %v", tc.name, got, tc.want)
		}
	}
}
