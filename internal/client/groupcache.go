package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/sealjay/mcp-whatsapp/internal/ratelimit"
)

// groupInfoTTL is how long a group's details are served from memory before
// WhatsApp is asked again. Group changes arrive as events and clear the
// entry, so the TTL only bounds how stale a missed change can get.
const groupInfoTTL = 10 * time.Minute

// groupInfoCache keeps recent GetGroupInfo answers, so repeated lookups and
// sends to the same group do not each become a query to WhatsApp's servers.
type groupInfoCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	clock   func() time.Time
	entries map[types.JID]groupInfoEntry
}

type groupInfoEntry struct {
	info    *types.GroupInfo
	fetched time.Time
}

func newGroupInfoCache(ttl time.Duration) *groupInfoCache {
	return &groupInfoCache{ttl: ttl, clock: time.Now, entries: make(map[types.JID]groupInfoEntry)}
}

func (g *groupInfoCache) get(jid types.JID) (*types.GroupInfo, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entries[jid.ToNonAD()]
	if !ok {
		return nil, false
	}
	if g.clock().Sub(e.fetched) >= g.ttl {
		delete(g.entries, jid.ToNonAD())
		return nil, false
	}
	return e.info, true
}

func (g *groupInfoCache) put(info *types.GroupInfo) {
	if info == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock()
	for k, e := range g.entries {
		if now.Sub(e.fetched) >= g.ttl {
			delete(g.entries, k)
		}
	}
	g.entries[info.JID.ToNonAD()] = groupInfoEntry{info: info, fetched: now}
}

func (g *groupInfoCache) forget(jid types.JID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.entries, jid.ToNonAD())
}

// errRateLimited formats a limiter denial the same way for every call site.
func errRateLimited(d ratelimit.Decision) error {
	return fmt.Errorf("rate limited: %s — retry in %s (set the X-Rate-Limit-Override header to bypass)",
		d.Reason, d.RetryAfter.Round(time.Second))
}

// allow runs a limiter check unless there is no limiter or the operator
// override is on the context.
func (c *Client) allow(ctx context.Context, check func(*ratelimit.Limiter) ratelimit.Decision) error {
	if c.limiter == nil || ratelimit.BypassFromContext(ctx) {
		return nil
	}
	if d := check(c.limiter); !d.Allowed {
		return errRateLimited(d)
	}
	return nil
}

// groupInfo returns a group's details, from the cache when they are recent.
// limited makes a cache miss count against the lookup limit; internal callers
// whose own traffic is already capped (sends) or happens once per group
// (naming a new chat) pass false.
func (c *Client) groupInfo(ctx context.Context, jid types.JID, limited bool) (*types.GroupInfo, error) {
	if c.groups != nil {
		if info, ok := c.groups.get(jid); ok {
			return info, nil
		}
	}
	if c.wa == nil {
		return nil, errors.New("not connected to WhatsApp")
	}
	if limited {
		if err := c.allow(ctx, (*ratelimit.Limiter).AllowLookup); err != nil {
			return nil, err
		}
	}
	info, err := c.wa.GetGroupInfo(ctx, jid)
	if err != nil {
		return nil, err
	}
	if c.groups != nil {
		c.groups.put(info)
	}
	return info, nil
}

// forgetGroup drops a group's cached details after it changes.
func (c *Client) forgetGroup(jid types.JID) {
	if c.groups != nil {
		c.groups.forget(jid)
	}
}
