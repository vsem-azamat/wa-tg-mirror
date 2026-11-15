package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// chatSummary is one chat seen in the history sync.
type chatSummary struct {
	jid      string
	name     string
	lastSeen time.Time
}

// historyCollector keeps the chats the phone sends in history sync events.
type historyCollector struct {
	mu    sync.Mutex
	chats map[string]chatSummary
	last  time.Time
}

func newHistoryCollector() *historyCollector {
	return &historyCollector{chats: map[string]chatSummary{}}
}

func (h *historyCollector) handle(evt any) {
	hs, ok := evt.(*events.HistorySync)
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last = time.Now()
	for _, c := range hs.Data.GetConversations() {
		ts := max(c.GetConversationTimestamp(), c.GetLastMsgTimestamp())
		s := chatSummary{jid: c.GetID(), name: cmp.Or(c.GetName(), c.GetDisplayName()), lastSeen: time.Unix(int64(ts), 0)} //nolint:gosec // WhatsApp timestamps fit int64
		if old, ok := h.chats[s.jid]; ok {
			s.name = cmp.Or(s.name, old.name)
			if old.lastSeen.After(s.lastSeen) {
				s.lastSeen = old.lastSeen
			}
		}
		h.chats[s.jid] = s
	}
}

// quietFor reports whether no history sync arrived for d, and at least one did.
func (h *historyCollector) quietFor(d time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.last.IsZero() && time.Since(h.last) > d
}

func (h *historyCollector) sorted() []chatSummary {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]chatSummary, 0, len(h.chats))
	for _, c := range h.chats {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b chatSummary) int { return b.lastSeen.Compare(a.lastSeen) })
	return out
}

// writeDiscovery prints the joined groups, the recent chats and the named contacts.
func writeDiscovery(ctx context.Context, out io.Writer, cli *whatsmeow.Client, chats []chatSummary) error {
	w := &strings.Builder{}
	contacts, err := cli.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return fmt.Errorf("contacts: %w", err)
	}
	nameOf := func(jid types.JID) string {
		c := contacts[jid]
		if !c.Found && jid.Server == types.HiddenUserServer {
			if pn, err := cli.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
				c = contacts[pn]
			}
		}
		return cmp.Or(c.FullName, c.FirstName, c.PushName, c.BusinessName)
	}

	groups, err := cli.GetJoinedGroups(ctx)
	if err != nil {
		return fmt.Errorf("groups: %w", err)
	}
	fmt.Fprintf(w, "# Groups (%d)\n", len(groups))
	for _, g := range groups {
		fmt.Fprintf(w, "\n%s  [%s]  %d members\n", g.Name, g.JID, len(g.Participants))
		for _, p := range g.Participants {
			pn := p.PhoneNumber
			if pn.IsEmpty() {
				pn = p.JID
			}
			fmt.Fprintf(w, "  - %s  %s\n", cmp.Or(nameOf(pn), nameOf(p.JID), p.DisplayName, "?"), pn.User)
		}
	}

	fmt.Fprintf(w, "\n# Recent chats (%d)\n", len(chats))
	for _, c := range chats {
		name := c.name
		if jid, err := types.ParseJID(c.jid); err == nil {
			name = cmp.Or(nameOf(jid), name)
		}
		fmt.Fprintf(w, "%s  %-30s  %s\n", c.lastSeen.Format(time.DateOnly), name, c.jid)
	}

	fmt.Fprintf(w, "\n# Contacts with a saved name (%d total contacts)\n", len(contacts))
	for jid, c := range contacts {
		if c.FullName != "" {
			fmt.Fprintf(w, "%-30s  %s\n", c.FullName, jid.User)
		}
	}
	if _, err := io.WriteString(out, w.String()); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
