package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func open(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLinksSeparatePrivateChatsByBotAndShareSupergroups(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.PutLink(ctx, Link{Bot: 1, TGChat: 100, TGMsg: 5, WAChat: "a", WAID: "X"}))
	must(s.PutLink(ctx, Link{Bot: 2, TGChat: 100, TGMsg: 5, WAChat: "b", WAID: "Y"}))
	must(s.PutLink(ctx, Link{Bot: 1, TGChat: -1001, TGMsg: 7, WAChat: "g", WAID: "Z"}))

	if l, err := s.ByTelegram(ctx, 2, 100, 5); err != nil || l.WAID != "Y" {
		t.Fatalf("private: %+v %v", l, err)
	}
	// Any bot sees the same supergroup message ID.
	if l, err := s.ByTelegram(ctx, 3, -1001, 7); err != nil || l.WAID != "Z" {
		t.Fatalf("group: %+v %v", l, err)
	}
	if id, err := s.ByWhatsApp(ctx, "g", "Z", 9, -1001); err != nil || id != 7 {
		t.Fatalf("by WhatsApp: %d %v", id, err)
	}
	if _, err := s.ByTelegram(ctx, 1, 100, 6); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestTopicsAndAvatars(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	if err := s.PutTopic(ctx, "c", -1009, 41); err != nil {
		t.Fatal(err)
	}
	if chat, err := s.ChatOfTopic(ctx, -1009, 41); err != nil || chat != "c" {
		t.Fatalf("%q %v", chat, err)
	}
	if err := s.DeleteTopic(ctx, "c"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Topic(ctx, "c"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted topic: %v", err)
	}
	if id, err := s.Avatar(ctx, 1); err != nil || id != "" {
		t.Fatalf("no avatar: %q %v", id, err)
	}
	if err := s.PutAvatar(ctx, 1, "p1"); err != nil {
		t.Fatal(err)
	}
	if id, _ := s.Avatar(ctx, 1); id != "p1" {
		t.Fatalf("avatar %q", id)
	}
}
