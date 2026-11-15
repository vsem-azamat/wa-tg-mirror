package tg

import (
	"errors"
	"strings"
	"testing"

	"github.com/mymmrac/telego"
	ta "github.com/mymmrac/telego/telegoapi"

	"github.com/vsem-azamat/wa-tg-mirror/internal/bridge"
)

func TestSplitPrefersLineBreaksAndKeepsEverything(t *testing.T) {
	s := strings.Repeat("a", 6) + "\n" + strings.Repeat("b", 6)
	got := split(s, 8)
	if strings.Join(got, "") != s {
		t.Fatalf("lost text: %q", got)
	}
	if got[0] != "aaaaaa" {
		t.Fatalf("first piece %q", got[0])
	}
	for _, p := range got {
		if len([]rune(p)) > 8 {
			t.Fatalf("piece too long: %q", p)
		}
	}
}

func TestComposeEscapesTextOnly(t *testing.T) {
	if got := compose("<b>A</b>", "x<y"); got != "<b>A</b>\nx&lt;y" {
		t.Fatalf("got %q", got)
	}
}

func TestToIncoming(t *testing.T) {
	topic := &telego.Message{
		MessageID: 9, MessageThreadID: 4, IsTopicMessage: true, Chat: telego.Chat{ID: -1001, Type: "supergroup"},
		From: &telego.User{ID: 7}, Text: "hi", ReplyToMessage: &telego.Message{MessageID: 4},
	}
	in := ToIncoming(topic)
	if in.Thread != 4 || in.ReplyTo != 0 || in.Kind != bridge.KindText || in.From != 7 {
		t.Fatalf("a reply to the topic root is no reply: %+v", in)
	}

	voice := ToIncoming(&telego.Message{Chat: telego.Chat{Type: "private"}, Voice: &telego.Voice{FileID: "v", Duration: 3}})
	if !voice.Private || voice.Kind != bridge.KindVoice || voice.File.Seconds != 3 {
		t.Fatalf("voice %+v", voice)
	}

	sticker := ToIncoming(&telego.Message{Sticker: &telego.Sticker{IsAnimated: true, Emoji: "😀"}})
	if sticker.Kind != bridge.KindText || sticker.Text != "😀" {
		t.Fatalf("animated sticker %+v", sticker)
	}

	pinned := ToIncoming(&telego.Message{PinnedMessage: &telego.Message{MessageID: 1}})
	if !pinned.Service || pinned.Unsupported != "" {
		t.Fatalf("service message %+v", pinned)
	}

	start := ToIncoming(&telego.Message{
		Text: "/start@some_bot", Entities: []telego.MessageEntity{{Type: telego.EntityTypeBotCommand, Offset: 0, Length: 15}},
	})
	if start.Command != "start" {
		t.Fatalf("command %+v", start)
	}

	unknown := ToIncoming(&telego.Message{Caption: "x"})
	if unknown.Unsupported == "" || unknown.Service {
		t.Fatalf("unknown content %+v", unknown)
	}

	poll := ToIncoming(&telego.Message{Poll: &telego.Poll{}})
	if poll.Unsupported == "" {
		t.Fatalf("poll %+v", poll)
	}
}

func TestClassify(t *testing.T) {
	forbidden := classify(&ta.Error{ErrorCode: 403, Description: "Forbidden: bot can't initiate conversation"})
	if !errors.Is(forbidden, bridge.ErrForbidden) {
		t.Fatal("403 is ErrForbidden")
	}
	if !errors.Is(classify(&ta.Error{ErrorCode: 400, Description: "Bad Request: chat not found"}), bridge.ErrForbidden) {
		t.Fatal("a chat the bot is not in is ErrForbidden")
	}
	gone := classify(&ta.Error{ErrorCode: 400, Description: "Bad Request: message thread not found"})
	if !errors.Is(gone, bridge.ErrTopicGone) {
		t.Fatal("missing thread is ErrTopicGone")
	}
}

func TestRetryWaitsOutRateLimit(t *testing.T) {
	calls := 0
	v, err := retry(t.Context(), func() (int, error) {
		calls++
		if calls == 1 {
			return 0, &ta.Error{ErrorCode: 429, Parameters: &ta.ResponseParameters{RetryAfter: 0}}
		}
		return 42, nil
	})
	if err != nil || v != 42 || calls != 2 {
		t.Fatalf("v=%d err=%v calls=%d", v, err, calls)
	}
}

func TestExtension(t *testing.T) {
	if extension("audio/ogg; codecs=opus", ".mp3") != ".ogg" || extension("", ".mp4") != ".mp4" {
		t.Fatal("extension")
	}
}

func TestNormalizeReaction(t *testing.T) {
	cases := map[string]string{"❤️": "❤", "👍🏽": "👍", "😂": "", "🔥": "🔥"}
	for in, want := range cases {
		if got := NormalizeReaction(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
