package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ActivityDay counts the messages of one direct chat on one calendar day.
type ActivityDay struct {
	Date     string    `json:"date"` // YYYY-MM-DD in the requested time zone
	Sent     int       `json:"sent"`
	Received int       `json:"received"`
	FirstAt  time.Time `json:"first_at"`
	LastAt   time.Time `json:"last_at"`
}

// ChatActivity is the per-day message activity of one direct chat. A person
// whose chat exists under both a LID and a phone JID is merged into one entry
// keyed by the phone JID.
type ChatActivity struct {
	ChatJID     string        `json:"chat_jid"`
	PhoneNumber string        `json:"phone_number,omitempty"`
	Name        string        `json:"name,omitempty"`
	Days        []ActivityDay `json:"days"`
}

// ChatActivityParams bounds ListChatActivity. After is exclusive, Before
// inclusive; a zero Before means now. Loc groups messages into days (UTC when
// nil).
type ChatActivityParams struct {
	After  time.Time
	Before time.Time
	Loc    *time.Location
}

// ListChatActivity returns, for every direct (1:1) chat with messages in the
// window, how many messages were sent and received on each day. Groups,
// broadcast lists and status updates are left out. Message content is never
// read, so the result is safe to hand to a contact-tracking app.
func (s *Store) ListChatActivity(ctx context.Context, p ChatActivityParams) ([]ChatActivity, error) {
	loc := p.Loc
	if loc == nil {
		loc = time.UTC
	}
	where := []string{
		"chat_jid NOT LIKE '%@g.us'",
		"chat_jid NOT LIKE '%@broadcast'",
		"chat_jid NOT LIKE '%@newsletter'",
		"timestamp > ?",
	}
	args := []any{p.After}
	if !p.Before.IsZero() {
		where = append(where, "timestamp <= ?")
		args = append(args, p.Before)
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT chat_jid, timestamp, is_from_me FROM messages WHERE "+
			strings.Join(where, " AND ")+" ORDER BY timestamp", args...)
	if err != nil {
		return nil, fmt.Errorf("list chat activity: %w", err)
	}
	defer rows.Close()

	type key struct{ jid, date string }
	days := map[key]*ActivityDay{}
	chats := map[string]*ChatActivity{}
	var order []string
	for rows.Next() {
		var (
			jid    string
			ts     time.Time
			fromMe bool
		)
		if err := rows.Scan(&jid, &ts, &fromMe); err != nil {
			return nil, err
		}
		jid = s.ResolveLIDToJID(jid)
		c := chats[jid]
		if c == nil {
			c = &ChatActivity{ChatJID: jid, PhoneNumber: s.ResolveJIDToPhone(jid)}
			chats[jid] = c
			order = append(order, jid)
		}
		date := ts.In(loc).Format("2006-01-02")
		d := days[key{jid, date}]
		if d == nil {
			d = &ActivityDay{Date: date, FirstAt: ts.UTC()}
			days[key{jid, date}] = d
		}
		if fromMe {
			d.Sent++
		} else {
			d.Received++
		}
		d.LastAt = ts.UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for k, d := range days {
		chats[k.jid].Days = append(chats[k.jid].Days, *d)
	}
	result := make([]ChatActivity, 0, len(order))
	for _, jid := range order {
		c := chats[jid]
		sort.Slice(c.Days, func(i, j int) bool { return c.Days[i].Date < c.Days[j].Date })
		c.Name = s.ContactName(jid)
		if c.Name == "" {
			c.Name = s.FindChatName(jid)
		}
		result = append(result, *c)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ChatJID < result[j].ChatJID })
	return result, nil
}
