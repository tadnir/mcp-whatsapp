package client

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestGroupInfoCache_ExpiresAndForgets(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	g := newGroupInfoCache(10 * time.Minute)
	g.clock = func() time.Time { return now }

	jid := types.NewJID("123456789", types.GroupServer)
	g.put(&types.GroupInfo{JID: jid, GroupName: types.GroupName{Name: "Family"}})

	if info, ok := g.get(jid); !ok || info.Name != "Family" {
		t.Fatalf("expected a cached entry, got %v %v", info, ok)
	}
	now = now.Add(9 * time.Minute)
	if _, ok := g.get(jid); !ok {
		t.Fatal("entry should still be fresh after 9 minutes")
	}
	now = now.Add(time.Minute)
	if _, ok := g.get(jid); ok {
		t.Fatal("entry should expire after 10 minutes")
	}

	g.put(&types.GroupInfo{JID: jid})
	g.forget(jid)
	if _, ok := g.get(jid); ok {
		t.Fatal("forget should drop the entry")
	}
}
