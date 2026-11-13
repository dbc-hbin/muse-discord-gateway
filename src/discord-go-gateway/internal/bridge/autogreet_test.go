package bridge

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func autoGreetPolicy() Policy {
	return Policy{OwnerID: "111111111111111111", AllowedDMIDs: []string{"111111111111111111"}, Platform: "discord", GuildID: "555555555555555555", GuildChannelID: "222222222222222222", GuildMode: "mention", ExtraGuildID: "666666666666666666", ExtraGuildChannelID: "777777777777777777"}
}

func autoGreetStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "queue.db")
	s, err := OpenStore(path, autoGreetPolicy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func seedJoinInbound(t *testing.T, path, id, eventID string, created float64) Envelope {
	t.Helper()
	e := Envelope{Platform: "discord", EventID: eventID, ConversationID: "777777777777777777", GuildID: "666666666666666666", RouteKind: "guild_text", SenderID: "424242424242424242", SenderName: "newbie", MemberJoined: true, ReceivedAt: created}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("INSERT INTO inbound(id,platform,event_id,envelope,created) VALUES(?,?,?,?,?)", id, e.Platform, e.EventID, string(raw), created); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAutoReplyJoinGreetsPendingJoinOnce(t *testing.T) {
	s, path := autoGreetStore(t)
	e := seedJoinInbound(t, path, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "100000000000000001", epoch())

	pending, err := s.PendingAutoGreetJoin(120)
	if err != nil || pending == nil || pending.EventID != "100000000000000001" {
		t.Fatalf("pending join not found: %#v %v", pending, err)
	}
	text := joinGreetingText(e)
	known := false
	for _, tpl := range joinGreetings {
		if text == fmt.Sprintf(tpl, e.SenderID) {
			known = true
			break
		}
	}
	if !known {
		t.Fatalf("greeting %q is not one of the known templates", text)
	}
	// The pool must actually vary: many draws for the same member should
	// not all land on one line.
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		seen[joinGreetingText(e)] = true
	}
	if len(seen) < 3 {
		t.Fatalf("greeting pool does not vary: %v", seen)
	}
	greeted, err := s.AutoReplyJoin(e, text)
	if err != nil || !greeted {
		t.Fatalf("auto greet failed: %v %v", greeted, err)
	}
	// The dispatcher must find exactly one pending chunk with the greeting.
	c, err := s.NextChunk()
	if err != nil || c == nil || c.Text != text {
		t.Fatalf("expected greeting chunk, got %#v %v", c, err)
	}
	// A second attempt must not greet again (inbound already replied).
	greeted, err = s.AutoReplyJoin(e, text)
	if err != nil || greeted {
		t.Fatalf("second auto greet = %v, %v", greeted, err)
	}
	pending, err = s.PendingAutoGreetJoin(120)
	if err != nil || pending != nil {
		t.Fatalf("join still pending after greet: %#v %v", pending, err)
	}
	// The consumer must no longer be offered the greeted join.
	claim, err := s.ClaimNext(300, 60)
	if err != nil || claim != nil {
		t.Fatalf("greeted join offered to consumer: %#v %v", claim, err)
	}
}

func TestFreshJoinIsReservedForAutoGreet(t *testing.T) {
	s, path := autoGreetStore(t)
	seedJoinInbound(t, path, "dddddddddddddddddddddddddddddddd", "100000000000000004", epoch())
	// A just-staged join is inside the auto-greet grace window: the
	// consumer must not be offered it at all.
	claim, err := s.ClaimNext(300, 60)
	if err != nil || claim != nil {
		t.Fatalf("fresh join offered to consumer inside grace: %#v %v", claim, err)
	}
}

func TestIsGreetingOnly(t *testing.T) {
	yes := []string{
		"<@1528396024492593324> 쌀숭이 안뇽",
		"<@1528396024492593324> 안녕",
		"안뇽~",
		"<@1528396024492593324> 하이!",
		"ㅎㅇ",
		"해적쌀숭 안녕ㅋㅋ",
		"<@1528396024492593324> 방가 방가", // two tokens: must NOT match
	}
	// "방가 방가" is two non-filler tokens → not a pure greeting.
	yes = yes[:len(yes)-1]
	for _, text := range yes {
		e := Envelope{Text: text, GuildID: "666666666666666666", BotMentioned: true}
		if !IsGreetingOnly(e) {
			t.Errorf("IsGreetingOnly(%q) = false, want true", text)
		}
	}
	no := []string{
		"안녕 가격 얼마야?",
		"<@1528396024492593324> 하이쿠 가격 알려줘",
		"<@1528396024492593324> 너처럼 좋은 봇이 있는데 왜 안온다고 생각하냐",
		"안녕하세요 오늘 날씨 어때",
		"방가 방가",
		"",
		"<@1528396024492593324>",
	}
	for _, text := range no {
		e := Envelope{Text: text, GuildID: "666666666666666666", BotMentioned: true}
		if IsGreetingOnly(e) {
			t.Errorf("IsGreetingOnly(%q) = true, want false", text)
		}
	}
}

func seedGreetingInbound(t *testing.T, path, id, eventID, text string, created float64) Envelope {
	t.Helper()
	e := Envelope{Platform: "discord", EventID: eventID, ConversationID: "777777777777777777", GuildID: "666666666666666666", RouteKind: "guild_text", SenderID: "424242424242424242", SenderName: "crew", Text: text, BotMentioned: true, ReceivedAt: created}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("INSERT INTO inbound(id,platform,event_id,envelope,created) VALUES(?,?,?,?,?)", id, e.Platform, e.EventID, string(raw), created); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAutoReplyGreetingAnswersPendingGreeting(t *testing.T) {
	s, path := autoGreetStore(t)
	e := seedGreetingInbound(t, path, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "100000000000000005", "<@1528396024492593324> 쌀숭이 안뇽", epoch())

	pending, err := s.PendingAutoGreeting(120)
	if err != nil || pending == nil || pending.EventID != "100000000000000005" {
		t.Fatalf("pending greeting not found: %#v %v", pending, err)
	}
	// A fresh greeting is reserved for the gateway inside the grace
	// window: the consumer must not be offered it.
	if claim, err := s.ClaimNext(300, 60); err != nil || claim != nil {
		t.Fatalf("fresh greeting offered to consumer inside grace: %#v %v", claim, err)
	}
	answered, err := s.AutoReplyGreeting(e, "쌀쌀쌀! <@424242424242424242> 반갑다.")
	if err != nil || !answered {
		t.Fatalf("auto greeting failed: %v %v", answered, err)
	}
	c, err := s.NextChunk()
	if err != nil || c == nil || c.Text == "" {
		t.Fatalf("expected greeting chunk, got %#v %v", c, err)
	}
	// Second attempt must not answer again.
	answered, err = s.AutoReplyGreeting(e, "again")
	if err != nil || answered {
		t.Fatalf("second auto greeting = %v, %v", answered, err)
	}
	// A non-greeting message is never pending for the greeting lane.
	seedGreetingInbound(t, path, "ffffffffffffffffffffffffffffffff", "100000000000000006", "<@1528396024492593324> 가격 얼마야", epoch())
	if pending, err := s.PendingAutoGreeting(120); err != nil || pending != nil {
		t.Fatalf("content message offered to greeting lane: %#v %v", pending, err)
	}
}

func TestAutoReplyJoinSkipsStaleAndNonJoin(t *testing.T) {
	s, path := autoGreetStore(t)
	// Stale join (staged 10 minutes ago) is not eligible for auto-greet.
	stale := seedJoinInbound(t, path, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "100000000000000002", epoch()-600)
	if pending, err := s.PendingAutoGreetJoin(120); err != nil || pending != nil {
		t.Fatalf("stale join offered for auto greet: %#v %v", pending, err)
	}
	// A claimed join belongs to the consumer; AutoReplyJoin must refuse.
	// Remove the stale row so the claim below lands on the fresh join.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM inbound WHERE id='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// Seeded 60s old: past the auto-greet claim grace, so the consumer
	// may claim it — and a claimed join must refuse AutoReplyJoin.
	fresh := seedJoinInbound(t, path, "cccccccccccccccccccccccccccccccc", "100000000000000003", epoch()-60)
	claim, err := s.ClaimNext(300, 60)
	if err != nil || claim == nil || claim.InboundID != "cccccccccccccccccccccccccccccccc" {
		t.Fatalf("claim = %#v %v", claim, err)
	}
	greeted, err := s.AutoReplyJoin(fresh, joinGreetingText(fresh))
	if err != nil || greeted {
		t.Fatalf("auto greet of claimed join = %v, %v", greeted, err)
	}
	_ = stale
}
