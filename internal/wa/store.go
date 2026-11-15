// Package wa connects to WhatsApp as a linked device through whatsmeow.
package wa

import (
	"context"
	"database/sql"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

// sqliteDSN enables the options whatsmeow expects from SQLite.
const sqliteDSN = "file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"

// OpenDB opens the SQLite database that holds the WhatsApp session and the bridge state.
func OpenDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", fmt.Sprintf(sqliteDSN, path))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return db, nil
}

// NewClient returns a client for the first device in db, or for a new device to pair.
func NewClient(ctx context.Context, db *sql.DB, log waLog.Logger) (*whatsmeow.Client, error) {
	container := sqlstore.NewWithDB(db, "sqlite", log.Sub("store"))
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade whatsmeow store: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("load device: %w", err)
	}
	return whatsmeow.NewClient(device, log.Sub("client")), nil
}
