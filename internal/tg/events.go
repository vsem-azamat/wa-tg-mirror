package tg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/vsem-azamat/wa-tg-mirror/internal/bridge"
)

// allowedReactions are the emoji a bot may set as a reaction.
var allowedReactions = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(`❤ 👍 👎 🔥 🥰 👏 😁 🤔 🤯 😱 🤬 😢 🎉 🤩 🤮 💩 🙏 👌 🕊 🤡 🥱 🥴 😍 🐳 ❤‍🔥 🌚 🌭 💯 🤣 ⚡ 🍌 🏆
		💔 🤨 😐 🍓 🍾 💋 🖕 😈 😴 😭 🤓 👻 👨‍💻 👀 🎃 🙈 😇 😨 🤝 ✍ 🤗 🫡 🎅 🎄 ☃ 💅 🤪 🗿 🆒 💘 🙉 🦄 😘 💊 🙊 😎 👾 🤷‍♂ 🤷 🤷‍♀ 😡`) {
		m[e] = true
	}
	return m
}()

// NormalizeReaction maps a WhatsApp reaction emoji to the form Telegram knows:
// without the emoji presentation selector and without skin tones. It returns ""
// when Telegram does not allow the emoji as a bot reaction.
func NormalizeReaction(emoji string) string {
	var b strings.Builder
	for _, r := range emoji {
		if r == '️' || r >= 0x1F3FB && r <= 0x1F3FF {
			continue
		}
		b.WriteRune(r)
	}
	if e := b.String(); allowedReactions[e] {
		return e
	}
	return ""
}

// ErrReactionNotAllowed means Telegram does not let bots react with the emoji.
var ErrReactionNotAllowed = errors.New("reaction not allowed for bots in Telegram")

// React sets the bot's reaction on a message; "" removes it.
func (b *Bot) React(ctx context.Context, chat int64, msg int, emoji string) error {
	var reactions []telego.ReactionType
	if emoji != "" {
		e := NormalizeReaction(emoji)
		if e == "" {
			return ErrReactionNotAllowed
		}
		reactions = []telego.ReactionType{&telego.ReactionTypeEmoji{Type: telego.ReactionEmoji, Emoji: e}}
	}
	_, err := retry(ctx, func() (bool, error) {
		return true, b.api.SetMessageReaction(ctx, &telego.SetMessageReactionParams{
			ChatID: tu.ID(chat), MessageID: msg, Reaction: reactions,
		})
	})
	if err != nil {
		return fmt.Errorf("set reaction: %w", err)
	}
	return nil
}

// Edit replaces the text of a message the bot sent, or its caption when it is media.
func (b *Bot) Edit(ctx context.Context, chat int64, msg int, out bridge.Outgoing) error {
	text := compose(out.Header, out.Text)
	if len([]rune(text)) > textLimit {
		text = string([]rune(text)[:textLimit])
	}
	_, err := retry(ctx, func() (*telego.Message, error) {
		return b.api.EditMessageText(ctx, &telego.EditMessageTextParams{
			ChatID: tu.ID(chat), MessageID: msg, Text: text, ParseMode: telego.ModeHTML,
			LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
		})
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "no text in the message") {
		return wrap("edit message", err)
	}
	if len([]rune(text)) > captionLimit {
		text = string([]rune(text)[:captionLimit])
	}
	_, err = retry(ctx, func() (*telego.Message, error) {
		return b.api.EditMessageCaption(ctx, &telego.EditMessageCaptionParams{
			ChatID: tu.ID(chat), MessageID: msg, Caption: text, ParseMode: telego.ModeHTML,
		})
	})
	return wrap("edit caption", err)
}

// Delete deletes a message.
func (b *Bot) Delete(ctx context.Context, chat int64, msg int) error {
	_, err := retry(ctx, func() (bool, error) {
		return true, b.api.DeleteMessage(ctx, &telego.DeleteMessageParams{ChatID: tu.ID(chat), MessageID: msg})
	})
	return wrap("delete message", err)
}

func wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

// Handlers receive what a bot's long poll delivers.
type Handlers struct {
	Message  func(context.Context, *telego.Message)
	Edited   func(context.Context, *telego.Message)
	Reaction func(context.Context, bridge.Reaction)
}

// ToReaction converts a reaction update; the new reaction is the first emoji, if any.
func ToReaction(r *telego.MessageReactionUpdated) bridge.Reaction {
	out := bridge.Reaction{Chat: r.Chat.ID, Msg: r.MessageID}
	if r.User != nil {
		out.From = r.User.ID
	}
	for _, t := range r.NewReaction {
		if e, ok := t.(*telego.ReactionTypeEmoji); ok {
			out.Emoji = e.Emoji
			break
		}
	}
	return out
}

// QRMessage shows pairing QR codes in one message that is replaced as codes rotate.
type QRMessage struct {
	bot   *Bot
	chat  int64
	msgID int
	log   *slog.Logger
}

// NewQRMessage prepares a QR message for chat.
func (b *Bot) NewQRMessage(chat int64, log *slog.Logger) *QRMessage {
	return &QRMessage{bot: b, chat: chat, log: log}
}

// Show sends png the first time and replaces the photo afterwards.
func (q *QRMessage) Show(ctx context.Context, png []byte, caption string) {
	if q.msgID != 0 {
		_, err := q.bot.api.EditMessageMedia(ctx, &telego.EditMessageMediaParams{
			ChatID: tu.ID(q.chat), MessageID: q.msgID,
			Media: &telego.InputMediaPhoto{
				Type: telego.MediaTypePhoto, Media: tu.FileFromReader(bytes.NewReader(png), "qr.png"), Caption: caption,
			},
		})
		if err == nil {
			return
		}
		q.log.Warn("replace QR", "err", err)
	}
	m, err := q.bot.api.SendPhoto(ctx, &telego.SendPhotoParams{
		ChatID: tu.ID(q.chat), Photo: tu.FileFromReader(bytes.NewReader(png), "qr.png"), Caption: caption,
	})
	if err != nil {
		q.log.Error("send QR", "err", err)
		return
	}
	q.msgID = m.MessageID
}
