package store

import (
	"context"
	"testing"
	"time"
)

func TestListChatActivity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// A LID chat for Bob (lid 99887766 maps to 447700000002) folds into his phone chat.
	if _, err := s.db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES ('99887766@lid', 'Bob', '2026-01-08 09:00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('b4', '99887766@lid', 'me', 'later', '2026-01-08 09:00:00', 1)`); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListChatActivity(ctx, ChatActivityParams{After: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 direct chats with groups left out, got %d: %+v", len(got), got)
	}
	alice, bob := got[0], got[1]
	if alice.ChatJID != "447700000001@s.whatsapp.net" || alice.PhoneNumber != "447700000001" {
		t.Fatalf("alice = %+v", alice)
	}
	if len(alice.Days) != 5 || alice.Days[0].Date != "2026-01-01" || alice.Days[0].Received != 1 {
		t.Fatalf("alice days = %+v", alice.Days)
	}
	if bob.ChatJID != "447700000002@s.whatsapp.net" {
		t.Fatalf("bob = %+v", bob)
	}
	last := bob.Days[len(bob.Days)-1]
	if last.Date != "2026-01-08" || last.Sent != 1 || last.Received != 1 {
		t.Fatalf("bob's merged LID day = %+v", last)
	}

	// Day boundaries follow the requested zone: 23:30 UTC is the next day in Jerusalem.
	if _, err := s.db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES ('a6', '447700000001@s.whatsapp.net', 'me', 'late', '2026-01-09 23:30:00', 1)`); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.ListChatActivity(ctx, ChatActivityParams{After: time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC), Loc: loc})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Days) != 2 || got[0].Days[1].Date != "2026-01-10" {
		t.Fatalf("zoned days = %+v", got)
	}
}
