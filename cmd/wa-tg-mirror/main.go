// Command wa-tg-mirror mirrors WhatsApp chats into Telegram, one bot per person.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/vsem-azamat/wa-tg-mirror/internal/config"
	"github.com/vsem-azamat/wa-tg-mirror/internal/wa"
)

// version is set at build time.
var version = "dev"

const usage = `usage: wa-tg-mirror <command> [flags]

commands:
  serve        run the bridge (-config config.yaml, -heartbeat file)
  check        validate the configuration (-config config.yaml)
  healthcheck  exit 0 when the heartbeat file is fresh
  version      print the build version
  discover     link WhatsApp if needed, then list groups, recent chats and contacts
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "wa-tg-mirror:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command")
	}
	switch args[0] {
	case "version":
		fmt.Println(version) //nolint:forbidigo // the version command prints to stdout
		return nil
	case "serve":
		return serve(ctx, args[1:])
	case "check":
		return check(args[1:])
	case "healthcheck":
		return healthcheck(args[1:])
	case "discover":
		return discover(ctx, args[1:])
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func discover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	dbPath := fs.String("db", "wa-tg-mirror.db", "SQLite database with the WhatsApp session")
	qrPNG := fs.String("qr-png", "", "write each pairing QR code to this PNG file instead of the terminal")
	wait := fs.Duration("history-wait", 2*time.Minute, "longest wait for the history sync")
	if err := fs.Parse(args); err != nil {
		return err
	}

	db, err := wa.OpenDB(*dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	log := waLog.Stdout("wa", "WARN", true)
	cli, err := wa.NewClient(ctx, db, log)
	if err != nil {
		return err
	}
	history := newHistoryCollector()
	cli.AddEventHandler(history.handle)

	fresh := cli.Store.ID == nil
	err = wa.Connect(ctx, cli, func(code string) {
		if *qrPNG == "" {
			q, _ := qrcode.New(code, qrcode.Low)
			fmt.Fprintln(os.Stderr, q.ToSmallString(false))
			return
		}
		if err := qrcode.WriteFile(code, qrcode.Medium, 512, *qrPNG); err != nil {
			fmt.Fprintln(os.Stderr, "write QR:", err)
			return
		}
		fmt.Fprintln(os.Stderr, "new QR code written to", *qrPNG)
	})
	if err != nil {
		return err
	}
	defer cli.Disconnect()
	fmt.Fprintln(os.Stderr, "connected")

	// A fresh link receives the chat list in history sync events; wait until they stop.
	deadline := time.After(*wait)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	quiet := 5 * time.Second
	if fresh {
		quiet = 20 * time.Second
	}
wait:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			break wait
		case <-tick.C:
			if history.quietFor(quiet) {
				break wait
			}
		}
	}
	return writeDiscovery(ctx, os.Stdout, cli, history.sorted())
}

func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	cfgPath := fs.String("config", "config.yaml", "configuration file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	overflow := "off"
	if cfg.Overflow != nil {
		overflow = "on"
	}
	fmt.Printf("config ok: %d people, %d groups, overflow %s\n", len(cfg.People), len(cfg.Groups), overflow) //nolint:forbidigo // the check command reports to stdout
	return nil
}
