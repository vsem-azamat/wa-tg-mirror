package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/vsem-azamat/wa-tg-mirror/internal/bridge"
	"github.com/vsem-azamat/wa-tg-mirror/internal/config"
	"github.com/vsem-azamat/wa-tg-mirror/internal/store"
	"github.com/vsem-azamat/wa-tg-mirror/internal/tg"
	"github.com/vsem-azamat/wa-tg-mirror/internal/wa"
)

// handleTimeout bounds the work on one message, including media transfers and the
// waits Telegram demands when it limits the rate.
const handleTimeout = 5 * time.Minute

// heartbeatEvery is how often a connected bridge touches the heartbeat file.
const heartbeatEvery = 30 * time.Second

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "configuration file")
	heartbeat := fs.String("heartbeat", "", "file touched while WhatsApp is connected, for the health check")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *heartbeat != "" {
		// A heartbeat left by an earlier run must not make this one look healthy.
		if err := os.Remove(*heartbeat); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove old heartbeat: %w", err)
		}
	}

	rt, err := openRuntime(ctx, log, cfg)
	if err != nil {
		return err
	}
	defer rt.close()
	cli, b, service, all, dir := rt.cli, rt.bridge, rt.service, rt.bots, rt.dir

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	cli.AddEventHandler(func(evt any) {
		switch e := evt.(type) {
		case *events.Message:
			// whatsmeow delivers events one by one: a stuck request must not stall them all.
			hctx, done := context.WithTimeout(ctx, handleTimeout)
			b.HandleWhatsApp(hctx, e)
			done()
		case *events.LoggedOut:
			service.Notify(ctx, log, cfg.OwnerID, 0, 0, "WhatsApp logged this bridge out. It will restart and send a new QR code.")
			cancel(errors.New("logged out of WhatsApp"))
		case *events.Connected:
			log.Info("whatsapp connected")
			go b.SyncProfiles(ctx, dir)
		case *events.GroupInfo:
			hctx, done := context.WithTimeout(ctx, handleTimeout)
			change := bridge.GroupChange{Chat: e.JID, TopicChanged: e.Topic != nil, Join: e.Join, Leave: e.Leave}
			if e.Name != nil {
				change.Name = e.Name.Name
			}
			b.HandleGroupInfo(hctx, change)
			done()
		case *events.CallOffer:
			hctx, done := context.WithTimeout(ctx, handleTimeout)
			_, video := e.Data.GetOptionalChildByTag("video")
			b.HandleCall(hctx, e.CallCreator, e.GroupJID, video)
			done()
		case *events.Picture:
			go b.PictureChanged(ctx, dir, e.JID)
		case *events.Disconnected:
			log.Warn("whatsapp disconnected")
		}
	})

	err = wa.Connect(ctx, cli, sendQR(ctx, log, service, cfg.OwnerID))
	if err != nil {
		return err
	}
	defer cli.Disconnect()
	log.Info("bridge running", "people", len(cfg.People), "groups", len(cfg.Groups), "overflow", cfg.Overflow != nil)

	var wg sync.WaitGroup
	for _, bot := range all {
		wg.Go(func() {
			err := bot.Listen(ctx, log, tg.Handlers{
				Message: func(ctx context.Context, m *telego.Message) {
					in := tg.ToIncoming(m)
					if in.From != cfg.OwnerID {
						return
					}
					hctx, done := context.WithTimeout(ctx, handleTimeout)
					defer done()
					if problem := b.HandleTelegram(hctx, bot, in); problem != "" {
						bot.Notify(ctx, log, in.Chat, in.Thread, in.ID, problem)
					}
				},
				Edited: func(ctx context.Context, m *telego.Message) {
					in := tg.ToIncoming(m)
					hctx, done := context.WithTimeout(ctx, handleTimeout)
					defer done()
					if problem := b.HandleTelegramEdit(hctx, bot, in); problem != "" {
						bot.Notify(ctx, log, in.Chat, in.Thread, in.ID, problem)
					}
				},
				Reaction: func(ctx context.Context, r bridge.Reaction) {
					hctx, done := context.WithTimeout(ctx, handleTimeout)
					defer done()
					b.HandleTelegramReaction(hctx, bot, r)
				},
				PollAnswer: func(ctx context.Context, a bridge.PollAnswer) {
					hctx, done := context.WithTimeout(ctx, handleTimeout)
					defer done()
					b.HandlePollAnswer(hctx, a)
				},
			})
			if err != nil {
				cancel(fmt.Errorf("bot %s: %w", bot.Username(), err))
			}
		})
	}
	if *heartbeat != "" {
		wg.Go(func() { beat(ctx, *heartbeat, cli.IsConnected) })
	}
	<-ctx.Done()
	wg.Wait()
	if cause := context.Cause(ctx); !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

// beat touches path while connected reports true.
func beat(ctx context.Context, path string, connected func() bool) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		if connected() {
			now := time.Now()
			if err := os.Chtimes(path, now, now); errors.Is(err, os.ErrNotExist) {
				_ = os.WriteFile(path, nil, 0o600)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// healthcheck exits non-zero when the heartbeat file is older than three beats.
func healthcheck(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: wa-tg-mirror healthcheck <heartbeat file>")
	}
	fi, err := os.Stat(args[0]) //nolint:gosec // the operator names the heartbeat file
	if err != nil {
		return fmt.Errorf("no heartbeat: %w", err)
	}
	if age := time.Since(fi.ModTime()); age > 3*heartbeatEvery {
		return fmt.Errorf("heartbeat is %s old", age.Round(time.Second))
	}
	return nil
}

// runtime is what serve and backfill share: the bots, the database, the WhatsApp
// client and the bridge.
type runtime struct {
	service *tg.Bot
	bots    []*tg.Bot // the service bot first, then one per person
	byPhone map[string]bridge.Telegram
	db      *sql.DB
	store   *store.Store
	cli     *whatsmeow.Client
	dir     wa.Directory
	bridge  *bridge.Bridge
	cfg     *config.Config
	log     *slog.Logger
}

func openRuntime(ctx context.Context, log *slog.Logger, cfg *config.Config) (*runtime, error) {
	rt := &runtime{byPhone: map[string]bridge.Telegram{}, cfg: cfg, log: log}
	var err error
	rt.service, err = tg.New(ctx, log, cfg.ServiceBot.Token)
	if err != nil {
		return nil, fmt.Errorf("service bot: %w", err)
	}
	rt.bots = []*tg.Bot{rt.service}
	for _, p := range cfg.People {
		bot, err := tg.New(ctx, log, p.Bot.Token)
		if err != nil {
			return nil, fmt.Errorf("bot of %s: %w", p.Name, err)
		}
		rt.byPhone[p.Phone] = bot
		rt.bots = append(rt.bots, bot)
	}
	rt.db, err = wa.OpenDB(cfg.Database)
	if err != nil {
		return nil, err
	}
	rt.store, err = store.Open(ctx, rt.db)
	if err != nil {
		rt.close()
		return nil, err
	}
	if err := rt.newClient(ctx); err != nil {
		rt.close()
		return nil, err
	}
	return rt, nil
}

// newClient loads the WhatsApp device from the database (a new one when there is
// none) and builds the bridge around it.
func (rt *runtime) newClient(ctx context.Context) error {
	cli, err := wa.NewClient(ctx, rt.db, waLog.Stdout("wa", "WARN", false))
	if err != nil {
		return err
	}
	rt.cli = cli
	rt.dir = wa.Directory{Client: cli}
	rt.bridge, err = bridge.New(rt.log, rt.cfg, cli, rt.dir, rt.store, rt.service, rt.byPhone)
	return err
}

func (rt *runtime) close() { _ = rt.db.Close() }

// sendQR returns a callback that shows each pairing QR code to the owner, in one
// message whose photo is replaced as the codes rotate.
func sendQR(ctx context.Context, log *slog.Logger, bot *tg.Bot, owner int64) func(code string) {
	msg := bot.NewQRMessage(owner, log)
	return func(code string) {
		png, err := qrcode.Encode(code, qrcode.Medium, 512)
		if err != nil {
			log.Error("encode QR", "err", err)
			return
		}
		msg.Show(ctx, png, "Scan in WhatsApp → Settings → Linked devices → Link a device. The code renews itself every 20 s.")
	}
}
