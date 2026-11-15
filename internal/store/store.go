// Package store keeps the bridge state: which Telegram message mirrors which WhatsApp
// message, and which forum topic belongs to which overflow chat.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// anyBot is the Bot value of a link in a supergroup. Message IDs in a supergroup
// are shared by every bot in it; in a private chat each bot has its own.
const anyBot int64 = 0

// Store wraps the bridge tables in the shared SQLite database.
type Store struct {
	db *sql.DB
}

// Link ties one Telegram message to one WhatsApp message.
type Link struct {
	// Bot is the ID of the bot whose private chat holds the message, or 0 in a group chat.
	Bot      int64
	TGChat   int64
	TGMsg    int
	WAChat   string
	WAID     string
	WASender string
	// Text is a short copy of the message, used to quote it in a WhatsApp reply.
	Text string
}

// ErrNotFound means there is no link or topic for the key.
var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS mirror_links (
	bot       INTEGER NOT NULL,
	tg_chat   INTEGER NOT NULL,
	tg_msg    INTEGER NOT NULL,
	wa_chat   TEXT    NOT NULL,
	wa_id     TEXT    NOT NULL,
	wa_sender TEXT    NOT NULL,
	text      TEXT    NOT NULL DEFAULT '',
	created   INTEGER NOT NULL DEFAULT (unixepoch()),
	PRIMARY KEY (bot, tg_chat, tg_msg)
);
CREATE INDEX IF NOT EXISTS mirror_links_wa ON mirror_links (wa_chat, wa_id);
CREATE TABLE IF NOT EXISTS mirror_topics (
	wa_chat   TEXT    PRIMARY KEY,
	tg_chat   INTEGER NOT NULL,
	thread_id INTEGER NOT NULL,
	UNIQUE (tg_chat, thread_id)
);
CREATE TABLE IF NOT EXISTS mirror_avatars (
	bot        INTEGER PRIMARY KEY,
	picture_id TEXT    NOT NULL
);
`

// Open creates the bridge tables in db if they are missing.
func Open(ctx context.Context, db *sql.DB) (*Store, error) {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("create bridge tables: %w", err)
	}
	return &Store{db: db}, nil
}

// GroupBot returns the Bot value to store for a message in chat.
func GroupBot(chat, bot int64) int64 {
	if chat < 0 {
		return anyBot
	}
	return bot
}

// PutLink records l, replacing an older link with the same Telegram key.
func (s *Store) PutLink(ctx context.Context, l Link) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO mirror_links
		(bot, tg_chat, tg_msg, wa_chat, wa_id, wa_sender, text) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		GroupBot(l.TGChat, l.Bot), l.TGChat, l.TGMsg, l.WAChat, l.WAID, l.WASender, l.Text)
	if err != nil {
		return fmt.Errorf("put link: %w", err)
	}
	return nil
}

// ByTelegram finds the WhatsApp message mirrored by a Telegram message.
func (s *Store) ByTelegram(ctx context.Context, bot, chat int64, msg int) (Link, error) {
	l := Link{Bot: GroupBot(chat, bot), TGChat: chat, TGMsg: msg}
	err := s.db.QueryRowContext(ctx, `SELECT wa_chat, wa_id, wa_sender, text FROM mirror_links
		WHERE bot = ? AND tg_chat = ? AND tg_msg = ?`, l.Bot, chat, msg).
		Scan(&l.WAChat, &l.WAID, &l.WASender, &l.Text)
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("find link: %w", err)
	}
	return l, nil
}

// ByWhatsApp finds the Telegram message in chat, as seen by bot, that mirrors a WhatsApp message.
func (s *Store) ByWhatsApp(ctx context.Context, waChat, waID string, bot, chat int64) (int, error) {
	var msg int
	err := s.db.QueryRowContext(ctx, `SELECT tg_msg FROM mirror_links
		WHERE wa_chat = ? AND wa_id = ? AND bot = ? AND tg_chat = ? ORDER BY tg_msg LIMIT 1`,
		waChat, waID, GroupBot(chat, bot), chat).Scan(&msg)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("find link: %w", err)
	}
	return msg, nil
}

// Topic returns the forum topic of an overflow chat.
func (s *Store) Topic(ctx context.Context, waChat string) (int, error) {
	var id int
	err := s.db.QueryRowContext(ctx, `SELECT thread_id FROM mirror_topics WHERE wa_chat = ?`, waChat).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("find topic: %w", err)
	}
	return id, nil
}

// ChatOfTopic returns the WhatsApp chat of a forum topic.
func (s *Store) ChatOfTopic(ctx context.Context, tgChat int64, thread int) (string, error) {
	var chat string
	err := s.db.QueryRowContext(ctx, `SELECT wa_chat FROM mirror_topics WHERE tg_chat = ? AND thread_id = ?`,
		tgChat, thread).Scan(&chat)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("find topic: %w", err)
	}
	return chat, nil
}

// PutTopic records the forum topic of an overflow chat.
func (s *Store) PutTopic(ctx context.Context, waChat string, tgChat int64, thread int) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO mirror_topics (wa_chat, tg_chat, thread_id) VALUES (?, ?, ?)`,
		waChat, tgChat, thread)
	if err != nil {
		return fmt.Errorf("put topic: %w", err)
	}
	return nil
}

// Avatar returns the WhatsApp picture ID last set as a bot's profile photo, or "".
func (s *Store) Avatar(ctx context.Context, bot int64) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT picture_id FROM mirror_avatars WHERE bot = ?`, bot).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find avatar: %w", err)
	}
	return id, nil
}

// PutAvatar records the WhatsApp picture ID set as a bot's profile photo.
func (s *Store) PutAvatar(ctx context.Context, bot int64, pictureID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO mirror_avatars (bot, picture_id) VALUES (?, ?)`, bot, pictureID)
	if err != nil {
		return fmt.Errorf("put avatar: %w", err)
	}
	return nil
}

// DeleteTopic forgets the forum topic of an overflow chat.
func (s *Store) DeleteTopic(ctx context.Context, waChat string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM mirror_topics WHERE wa_chat = ?`, waChat); err != nil {
		return fmt.Errorf("delete topic: %w", err)
	}
	return nil
}
