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

	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/vsem-azamat/wa-tg-mirror/internal/config"
	"github.com/vsem-azamat/wa-tg-mirror/internal/wa"
)

const (
	// historyQuiet is how long no history sync may arrive before it counts as done.
	historyQuiet = 5 * time.Minute
	// historySettle is the pause after the full sync reported completion.
	historySettle = 20 * time.Second
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
		if err := waitLoggedIn(ctx, rt.cli.IsLoggedIn); err != nil {
			return err
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
	hist.log = log
	rt.cli.AddEventHandler(hist.handle)

	// A code is valid while the login socket is open, under three minutes; a code that
	// was not typed in time is replaced by a new one.
	for attempt := 1; ; attempt++ {
		err = wa.PairWithCode(ctx, rt.cli, *phone, func(code string) {
			notify(fmt.Sprintf("Pairing code for the history load: %s\n\n"+
				"WhatsApp → Settings → Linked devices → Link a device → Link with phone number instead, then type the code.", code))
			log.Info("pairing code sent to the owner", "attempt", attempt)
		})
		if err == nil {
			break
		}
		if attempt == pairAttempts || ctx.Err() != nil {
			notify("Pairing failed; run backfill again with -phone <your number>.")
			return err
		}
		log.Warn("pairing did not complete; sending a new code", "err", err)
		rt.cli.Disconnect()
	}
	defer rt.cli.Disconnect()
	notify(fmt.Sprintf("Linked. Waiting for WhatsApp to send %d days of history…", *days))

	if err := hist.wait(ctx); err != nil {
		return err
	}
	notify(fmt.Sprintf("Loading %d messages into Telegram. New messages follow once this is done.", hist.size()))

	// Posting takes long, and messages keep arriving meanwhile: they are delivered to
	// this device only, so they are posted here too. Drain until a pass finds nothing
	// new, stop receiving, and drain once more; later messages wait on the server for
	// serve.
	sent := 0
	for {
		n, err := postBatch(ctx, log, rt, hist)
		sent += n
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	rt.cli.Disconnect()
	n, err := postBatch(ctx, log, rt, hist)
	sent += n
	if err != nil {
		return err
	}
	notify(fmt.Sprintf("History loaded: %d messages mirrored. Start the bridge again.", sent))
	log.Info("history loaded", "sent", sent)
	return nil
}

// pairAttempts is how many pairing codes are offered before giving up.
const pairAttempts = 5

// postBatch posts the buffered messages not posted yet, oldest first, and returns
// how many it took from the buffer.
func postBatch(ctx context.Context, log *slog.Logger, rt *runtime, hist *historyBuffer) (int, error) {
	msgs := hist.take()
	for i, m := range msgs {
		mctx, done := context.WithTimeout(ctx, handleTimeout)
		rt.bridge.HandleHistory(mctx, m)
		done()
		if ctx.Err() != nil {
			return i, ctx.Err()
		}
		if (i+1)%100 == 0 {
			log.Info("history progress", "done", i+1, "of", len(msgs))
		}
	}
	return len(msgs), nil
}

// waitLoggedIn waits until the server has accepted the session.
func waitLoggedIn(ctx context.Context, loggedIn func() bool) error {
	deadline := time.Now().Add(30 * time.Second)
	for !loggedIn() {
		if time.Now().After(deadline) {
			return errors.New("WhatsApp did not accept the session within 30 s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil
}

// historyBuffer collects the messages of the history sync, and live messages that
// arrive meanwhile, newer than since.
type historyBuffer struct {
	parse func(types.JID, *waWeb.WebMessageInfo) (*events.Message, error)
	since time.Time

	mu   sync.Mutex
	msgs map[string]*events.Message
	// taken holds the keys of messages already handed out for posting.
	taken map[string]bool
	last  time.Time
	seen  bool
	// fullDone is set when a full sync chunk reports 100 % progress.
	fullDone bool
	log      *slog.Logger
}

func newHistoryBuffer(cli interface {
	ParseWebMessage(types.JID, *waWeb.WebMessageInfo) (*events.Message, error)
}, since time.Time,
) *historyBuffer {
	return &historyBuffer{
		parse: cli.ParseWebMessage, since: since, msgs: map[string]*events.Message{}, taken: map[string]bool{},
		log: slog.New(slog.DiscardHandler),
	}
}

func (h *historyBuffer) handle(evt any) {
	switch e := evt.(type) {
	case *events.HistorySync:
		h.mu.Lock()
		defer h.mu.Unlock()
		h.seen, h.last = true, time.Now()
		kind, progress := e.Data.GetSyncType(), e.Data.GetProgress()
		h.log.Info("history sync chunk", "type", kind.String(), "progress", progress, "chats", len(e.Data.GetConversations()))
		if kind == waHistorySync.HistorySync_FULL && progress >= 100 {
			h.fullDone = true
		}
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
				h.add(chat.String()+"/"+m.Info.ID, m)
			}
		}
	case *events.Message:
		h.mu.Lock()
		defer h.mu.Unlock()
		h.add(e.Info.Chat.String()+"/"+e.Info.ID, e)
	}
}

// add keeps m unless it was handed out already. Callers hold mu.
func (h *historyBuffer) add(key string, m *events.Message) {
	if !h.taken[key] {
		h.msgs[key] = m
	}
}

func (h *historyBuffer) size() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs)
}

// wait returns once the full sync reported completion and chunks stopped for a
// moment, or chunks stopped arriving for historyQuiet, or after historyMax.
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
			quiet := time.Since(h.last)
			done := h.seen && (quiet > historyQuiet || h.fullDone && quiet > historySettle)
			h.mu.Unlock()
			if done {
				return nil
			}
		}
	}
}

// take hands out the buffered messages, oldest first, and forgets them.
func (h *historyBuffer) take() []*events.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*events.Message, 0, len(h.msgs))
	for k, m := range h.msgs {
		out = append(out, m)
		h.taken[k] = true
	}
	clear(h.msgs)
	slices.SortStableFunc(out, func(a, b *events.Message) int {
		return cmp.Or(a.Info.Timestamp.Compare(b.Info.Timestamp), cmp.Compare(a.Info.ID, b.Info.ID))
	})
	return out
}
