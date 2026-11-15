// Package store keeps the bridge state: which Telegram message mirrors which WhatsApp
// message, and which forum topic belongs to which overflow chat.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
	// SenderBot is the bot that posted the Telegram message, 0 for the owner's own
	// messages, UnknownBot for links stored before the bridge recorded it.
	SenderBot int64
}

// UnknownBot is the SenderBot of links from before the column existed.
const UnknownBot int64 = -1

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
CREATE TABLE IF NOT EXISTS mirror_polls (
	wa_chat   TEXT NOT NULL,
	wa_id     TEXT NOT NULL,
	wa_sender TEXT NOT NULL,
	options   TEXT NOT NULL,
	tg_poll   TEXT UNIQUE,
	PRIMARY KEY (wa_chat, wa_id)
);
CREATE TABLE IF NOT EXISTS mirror_avatars (
	bot        INTEGER PRIMARY KEY,
	picture_id TEXT    NOT NULL
);
`

// Open creates the bridge tables in db if they are missing and adds columns that
// later versions introduced.
func Open(ctx context.Context, db *sql.DB) (*Store, error) {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("create bridge tables: %w", err)
	}
	var has int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('mirror_links') WHERE name = 'sender_bot'`).Scan(&has)
	if err != nil {
		return nil, fmt.Errorf("inspect bridge tables: %w", err)
	}
	if has == 0 {
		if _, err := db.ExecContext(ctx, `ALTER TABLE mirror_links ADD COLUMN sender_bot INTEGER NOT NULL DEFAULT -1`); err != nil {
			return nil, fmt.Errorf("add sender_bot: %w", err)
		}
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
		(bot, tg_chat, tg_msg, wa_chat, wa_id, wa_sender, text, sender_bot) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		GroupBot(l.TGChat, l.Bot), l.TGChat, l.TGMsg, l.WAChat, l.WAID, l.WASender, l.Text, l.SenderBot)
	if err != nil {
		return fmt.Errorf("put link: %w", err)
	}
	return nil
}

// ByTelegram finds the WhatsApp message mirrored by a Telegram message.
func (s *Store) ByTelegram(ctx context.Context, bot, chat int64, msg int) (Link, error) {
	l := Link{Bot: GroupBot(chat, bot), TGChat: chat, TGMsg: msg}
	err := s.db.QueryRowContext(ctx, `SELECT wa_chat, wa_id, wa_sender, text, sender_bot FROM mirror_links
		WHERE bot = ? AND tg_chat = ? AND tg_msg = ?`, l.Bot, chat, msg).
		Scan(&l.WAChat, &l.WAID, &l.WASender, &l.Text, &l.SenderBot)
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

// HasWhatsApp reports whether a WhatsApp message is mirrored already.
func (s *Store) HasWhatsApp(ctx context.Context, waChat, waID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM mirror_links WHERE wa_chat = ? AND wa_id = ?`, waChat, waID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("find link: %w", err)
	}
	return n > 0, nil
}

// LinksOf returns every Telegram copy of a WhatsApp message.
func (s *Store) LinksOf(ctx context.Context, waChat, waID string) ([]Link, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT bot, tg_chat, tg_msg, wa_sender, text, sender_bot FROM mirror_links
		WHERE wa_chat = ? AND wa_id = ? ORDER BY tg_msg`, waChat, waID)
	if err != nil {
		return nil, fmt.Errorf("find links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Link
	for rows.Next() {
		l := Link{WAChat: waChat, WAID: waID}
		if err := rows.Scan(&l.Bot, &l.TGChat, &l.TGMsg, &l.WASender, &l.Text, &l.SenderBot); err != nil {
			return nil, fmt.Errorf("read link: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read links: %w", err)
	}
	return out, nil
}

// SetText updates the quoted copy of a WhatsApp message after an edit.
func (s *Store) SetText(ctx context.Context, waChat, waID, text string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE mirror_links SET text = ? WHERE wa_chat = ? AND wa_id = ?`, text, waChat, waID)
	if err != nil {
		return fmt.Errorf("update link: %w", err)
	}
	return nil
}

// DeleteLinks forgets every Telegram copy of a WhatsApp message.
func (s *Store) DeleteLinks(ctx context.Context, waChat, waID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mirror_links WHERE wa_chat = ? AND wa_id = ?`, waChat, waID)
	if err != nil {
		return fmt.Errorf("delete links: %w", err)
	}
	return nil
}

// PollRef is a WhatsApp poll with its options and the Telegram poll that mirrors it.
type PollRef struct {
	WAChat   string
	WAID     string
	WASender string
	Options  []string
	TGPoll   string // empty for polls the owner created in Telegram
}

// PutPoll records a poll.
func (s *Store) PutPoll(ctx context.Context, p PollRef) error {
	opts, err := json.Marshal(p.Options)
	if err != nil {
		return fmt.Errorf("encode poll options: %w", err)
	}
	var tgPoll any
	if p.TGPoll != "" {
		tgPoll = p.TGPoll
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO mirror_polls (wa_chat, wa_id, wa_sender, options, tg_poll)
		VALUES (?, ?, ?, ?, ?)`, p.WAChat, p.WAID, p.WASender, string(opts), tgPoll)
	if err != nil {
		return fmt.Errorf("put poll: %w", err)
	}
	return nil
}

// PollByWhatsApp finds a poll by its WhatsApp message.
func (s *Store) PollByWhatsApp(ctx context.Context, waChat, waID string) (PollRef, error) {
	return s.poll(ctx, `wa_chat = ? AND wa_id = ?`, waChat, waID)
}

// PollByTelegram finds a poll by the ID of its Telegram poll.
func (s *Store) PollByTelegram(ctx context.Context, tgPoll string) (PollRef, error) {
	return s.poll(ctx, `tg_poll = ?`, tgPoll)
}

func (s *Store) poll(ctx context.Context, where string, args ...any) (PollRef, error) {
	var (
		p      PollRef
		opts   string
		tgPoll sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `SELECT wa_chat, wa_id, wa_sender, options, tg_poll FROM mirror_polls WHERE `+where, args...).
		Scan(&p.WAChat, &p.WAID, &p.WASender, &opts, &tgPoll)
	if errors.Is(err, sql.ErrNoRows) {
		return PollRef{}, ErrNotFound
	}
	if err != nil {
		return PollRef{}, fmt.Errorf("find poll: %w", err)
	}
	p.TGPoll = tgPoll.String
	if err := json.Unmarshal([]byte(opts), &p.Options); err != nil {
		return PollRef{}, fmt.Errorf("decode poll options: %w", err)
	}
	return p, nil
}
