package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/vsem-azamat/wa-tg-mirror/internal/config"
	"github.com/vsem-azamat/wa-tg-mirror/internal/wa"
)

const (
	// historyQuiet is how long no history sync may arrive before it counts as done.
	historyQuiet = 2 * time.Minute
	// historyMax caps the wait for the history sync.
	historyMax = 30 * time.Minute
)

func backfill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "configuration file")
	days := fs.Int("days", 30, "how many days of history to mirror")
	phone := fs.String("phone", "", "your WhatsApp number, digits only; read from the current session when empty")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	rt, err := openRuntime(ctx, log, cfg)
	if err != nil {
		return err
	}
	defer rt.close()
	notify := func(text string) { rt.service.Notify(ctx, log, cfg.OwnerID, 0, 0, text) }

	// WhatsApp sends the history only when a device is linked: unlink the current one.
	if id := rt.cli.Store.ID; id != nil {
		*phone = cmp.Or(*phone, id.User)
		if err := rt.cli.Connect(); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		if err := rt.cli.Logout(ctx); err != nil {
			return fmt.Errorf("unlink the current device: %w", err)
		}
		if err := rt.newClient(ctx); err != nil {
			return err
		}
		log.Info("current device unlinked")
	}
	if *phone == "" {
		return errors.New("no session to read the number from: pass -phone")
	}

	wa.RequestFullHistory(*days)
	since := time.Now().AddDate(0, 0, -*days)
	hist := newHistoryBuffer(rt.cli, since)
	rt.cli.AddEventHandler(hist.handle)

	err = wa.PairWithCode(ctx, rt.cli, *phone, func(code string) {
		notify(fmt.Sprintf("Pairing code for the history load: %s\\n\\nWhatsApp → Settings → Linked devices → Link a device → Link with phone number instead, then type the code.", code))
		log.Info("pairing code sent to the owner")
	})
	if err != nil {
		return err
	}
	defer rt.cli.Disconnect()
	notify(fmt.Sprintf("Linked. Waiting for WhatsApp to send %d days of history…", *days))

	if err := hist.wait(ctx); err != nil {
		return err
	}
	msgs := hist.sorted()
	notify(fmt.Sprintf("Loading %d messages into Telegram. New messages will follow once this is done.", len(msgs)))
	log.Info("history received", "messages", len(msgs))

	sent := 0
	for i, m := range msgs {
		mctx, done := context.WithTimeout(ctx, handleTimeout)
		if rt.bridge.HandleHistory(mctx, m) {
			sent++
		}
		done()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if (i+1)%100 == 0 {
			log.Info("history progress", "done", i+1, "of", len(msgs))
		}
	}
	notify(fmt.Sprintf("History loaded: %d messages mirrored.", sent))
	log.Info("history loaded", "sent", sent, "seen", len(msgs))
	return nil
}

// historyBuffer collects the messages of the history sync, and live messages that
// arrive meanwhile, newer than since.
type historyBuffer struct {
	parse func(types.JID, *waWeb.WebMessageInfo) (*events.Message, error)
	since time.Time

	mu   sync.Mutex
	msgs map[string]*events.Message
	last time.Time
	seen bool
}

func newHistoryBuffer(cli interface {
	ParseWebMessage(types.JID, *waWeb.WebMessageInfo) (*events.Message, error)
}, since time.Time,
) *historyBuffer {
	return &historyBuffer{parse: cli.ParseWebMessage, since: since, msgs: map[string]*events.Message{}}
}

func (h *historyBuffer) handle(evt any) {
	switch e := evt.(type) {
	case *events.HistorySync:
		h.mu.Lock()
		defer h.mu.Unlock()
		h.seen, h.last = true, time.Now()
		for _, conv := range e.Data.GetConversations() {
			chat, err := types.ParseJID(conv.GetID())
			if err != nil {
				continue
			}
			for _, hm := range conv.GetMessages() {
				m, err := h.parse(chat, hm.GetMessage())
				if err != nil || m.Info.Timestamp.Before(h.since) {
					continue
				}
				h.msgs[chat.String()+"/"+m.Info.ID] = m
			}
		}
	case *events.Message:
		h.mu.Lock()
		defer h.mu.Unlock()
		h.msgs[e.Info.Chat.String()+"/"+e.Info.ID] = e
	}
}

// wait returns once history syncs stopped arriving for historyQuiet.
func (h *historyBuffer) wait(ctx context.Context) error {
	deadline := time.After(historyMax)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return nil
		case <-tick.C:
			h.mu.Lock()
			done := h.seen && time.Since(h.last) > historyQuiet
			h.mu.Unlock()
			if done {
				return nil
			}
		}
	}
}

// sorted returns the collected messages, oldest first.
func (h *historyBuffer) sorted() []*events.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*events.Message, 0, len(h.msgs))
	for _, m := range h.msgs {
		out = append(out, m)
	}
	slices.SortStableFunc(out, func(a, b *events.Message) int {
		return cmp.Or(a.Info.Timestamp.Compare(b.Info.Timestamp), cmp.Compare(a.Info.ID, b.Info.ID))
	})
	return out
}
