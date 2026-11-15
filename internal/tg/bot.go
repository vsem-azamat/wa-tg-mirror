// Package tg adapts a telego bot to the bridge.
package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
	"github.com/valyala/fasthttp"

	"github.com/vsem-azamat/wa-tg-mirror/internal/bridge"
)

const (
	textLimit    = 4096
	captionLimit = 1024
	// maxDownload is the largest file the public Bot API lets a bot download.
	maxDownload = 20 << 20
)

// Bot is one Telegram bot.
type Bot struct {
	api   *telego.Bot
	token string
	id    int64
	name  string
	http  *http.Client
}

// New connects a bot by token and reads its identity.
func New(ctx context.Context, log *slog.Logger, token string) (*Bot, error) {
	api, err := telego.NewBot(token,
		telego.WithFastHTTPClient(&fasthttp.Client{
			// Long polls wait 30 s; uploads of 50 MB on a slow link need minutes.
			ReadTimeout: 90 * time.Second, WriteTimeout: 5 * time.Minute,
		}),
		telego.WithLogger(errorLogger{log: log, token: token}))
	if err != nil {
		return nil, fmt.Errorf("create bot: %w", err)
	}
	me, err := api.GetMe(ctx)
	if err != nil {
		return nil, fmt.Errorf("get bot identity: %w", err)
	}
	return &Bot{api: api, token: token, id: me.ID, name: me.Username, http: &http.Client{Timeout: 2 * time.Minute}}, nil
}

// ID returns the bot's user ID.
func (b *Bot) ID() int64 { return b.id }

// Username returns the bot's username.
func (b *Bot) Username() string { return b.name }

// Send sends out and returns the ID of its first message.
func (b *Bot) Send(ctx context.Context, out bridge.Outgoing) (int, error) {
	text := out.Text
	header := fullHeader(out)
	if out.Kind == bridge.KindLocation || out.Kind == bridge.KindContact {
		return b.sendNative(ctx, out, header, text)
	}
	if out.Kind == bridge.KindText || out.Kind == bridge.KindSticker {
		if out.Kind == bridge.KindSticker && header != "" {
			// A sticker has no caption: name the sender in a message of its own first.
			id, err := b.sendText(ctx, out, header, "")
			if err != nil {
				return 0, err
			}
			out.ReplyTo = 0
			if _, err := b.sendMedia(ctx, out, ""); err != nil {
				return id, err
			}
			return id, nil
		}
		if out.Kind == bridge.KindText {
			return b.sendText(ctx, out, header, text)
		}
		return b.sendMedia(ctx, out, "")
	}
	caption := compose(header, text)
	if len([]rune(caption)) <= captionLimit {
		return b.sendMedia(ctx, out, caption)
	}
	id, err := b.sendMedia(ctx, out, header)
	if err != nil {
		return 0, err
	}
	out.ReplyTo = id
	if _, err := b.sendText(ctx, out, "", text); err != nil {
		return id, err
	}
	return id, nil
}

// fullHeader is the header with the time stamp of a history message in front.
func fullHeader(out bridge.Outgoing) string {
	if out.Stamp == "" {
		return out.Header
	}
	stamp := "<i>🕓 " + html.EscapeString(out.Stamp) + "</i>"
	if out.Header == "" {
		return stamp
	}
	return stamp + " " + out.Header
}

// maxVCard is the largest vCard Telegram accepts with a contact.
const maxVCard = 2048

// compose joins the trusted HTML header with escaped text.
func compose(header, text string) string {
	switch {
	case header == "":
		return html.EscapeString(text)
	case text == "":
		return header
	default:
		return header + "\n" + html.EscapeString(text)
	}
}

// sendText sends text in as many messages as it needs and returns the first ID.
func (b *Bot) sendText(ctx context.Context, out bridge.Outgoing, header, text string) (int, error) {
	first := 0
	chunks := split(text, textLimit-len([]rune(header))-1)
	for i, chunk := range chunks {
		h := ""
		if i == 0 {
			h = header
		}
		p := &telego.SendMessageParams{
			ChatID: tu.ID(out.Chat), MessageThreadID: out.Thread, Text: compose(h, chunk),
			ParseMode: telego.ModeHTML, ReplyParameters: replyTo(out.ReplyTo),
			LinkPreviewOptions: &telego.LinkPreviewOptions{IsDisabled: true},
		}
		m, err := retry(ctx, func() (*telego.Message, error) { return b.api.SendMessage(ctx, p) })
		if err != nil {
			return first, fmt.Errorf("send message: %w", err)
		}
		if first == 0 {
			first = m.MessageID
		}
	}
	return first, nil
}

// split cuts s into pieces of at most n runes, preferring line breaks.
func split(s string, n int) []string {
	r := []rune(s)
	if len(r) <= n {
		return []string{s}
	}
	var out []string
	for len(r) > n {
		cut := n
		for i := n; i > n/2; i-- {
			if r[i] == '\n' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		r = r[cut:]
	}
	if len(r) > 0 {
		out = append(out, string(r))
	}
	return out
}

func replyTo(id int) *telego.ReplyParameters {
	if id == 0 {
		return nil
	}
	return &telego.ReplyParameters{MessageID: id, AllowSendingWithoutReply: true}
}

func (b *Bot) sendMedia(ctx context.Context, out bridge.Outgoing, caption string) (int, error) {
	if out.Kind == bridge.KindText {
		return 0, errors.New("text is not media")
	}
	m, err := retry(ctx, func() (*telego.Message, error) { return b.sendMediaOnce(ctx, out, caption) })
	if err != nil {
		return 0, fmt.Errorf("send %s: %w", out.MimeType, err)
	}
	return m.MessageID, nil
}

// sendMediaOnce makes one upload; the file reader is fresh on every call.
func (b *Bot) sendMediaOnce(ctx context.Context, out bridge.Outgoing, caption string) (*telego.Message, error) {
	chat := tu.ID(out.Chat)
	file := tu.FileFromReader(bytes.NewReader(out.Data), fileName(out))
	reply := replyTo(out.ReplyTo)
	switch out.Kind {
	case bridge.KindPhoto:
		return b.api.SendPhoto(ctx, &telego.SendPhotoParams{
			ChatID: chat, MessageThreadID: out.Thread, Photo: file,
			Caption: caption, ParseMode: telego.ModeHTML, ReplyParameters: reply,
		})
	case bridge.KindVideo:
		return b.api.SendVideo(ctx, &telego.SendVideoParams{
			ChatID: chat, MessageThreadID: out.Thread, Video: file,
			Caption: caption, ParseMode: telego.ModeHTML, ReplyParameters: reply,
		})
	case bridge.KindAnimation:
		return b.api.SendAnimation(ctx, &telego.SendAnimationParams{
			ChatID: chat, MessageThreadID: out.Thread, Animation: file,
			Caption: caption, ParseMode: telego.ModeHTML, ReplyParameters: reply,
		})
	case bridge.KindVoice:
		return b.api.SendVoice(ctx, &telego.SendVoiceParams{
			ChatID: chat, MessageThreadID: out.Thread, Voice: file,
			Caption: caption, ParseMode: telego.ModeHTML, ReplyParameters: reply,
		})
	case bridge.KindAudio:
		return b.api.SendAudio(ctx, &telego.SendAudioParams{
			ChatID: chat, MessageThreadID: out.Thread, Audio: file,
			Caption: caption, ParseMode: telego.ModeHTML, ReplyParameters: reply,
		})
	case bridge.KindSticker:
		return b.api.SendSticker(ctx, &telego.SendStickerParams{
			ChatID: chat, MessageThreadID: out.Thread, Sticker: file, ReplyParameters: reply,
		})
	case bridge.KindDocument:
		return b.api.SendDocument(ctx, &telego.SendDocumentParams{
			ChatID: chat, MessageThreadID: out.Thread, Document: file,
			Caption: caption, ParseMode: telego.ModeHTML, ReplyParameters: reply,
		})
	case bridge.KindText, bridge.KindLocation, bridge.KindContact, bridge.KindPoll:
	}
	return nil, fmt.Errorf("unknown kind %d", out.Kind)
}

func fileName(out bridge.Outgoing) string {
	if out.FileName != "" {
		return out.FileName
	}
	switch out.Kind {
	case bridge.KindPhoto:
		return "photo.jpg"
	case bridge.KindVideo, bridge.KindAnimation:
		return "video" + extension(out.MimeType, ".mp4")
	case bridge.KindVoice:
		return "voice.ogg"
	case bridge.KindAudio:
		return "audio" + extension(out.MimeType, ".mp3")
	case bridge.KindSticker:
		return "sticker.webp"
	case bridge.KindText, bridge.KindDocument, bridge.KindLocation, bridge.KindContact, bridge.KindPoll:
	}
	return "file"
}

// Download fetches a file by ID.
func (b *Bot) Download(ctx context.Context, fileID string) ([]byte, error) {
	f, err := b.api.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("get file (bots can download up to 20 MB): %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.api.FileDownloadURL(f.FilePath), nil)
	if err != nil {
		return nil, fmt.Errorf("download file: %w", err)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, errors.New("download file: request failed") // the error would contain the token
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download file: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("download file: %w", err)
	}
	if len(data) > maxDownload {
		return nil, errors.New("file is larger than 20 MB")
	}
	return data, nil
}

// CreateTopic creates a forum topic.
func (b *Bot) CreateTopic(ctx context.Context, chat int64, name string) (int, error) {
	t, err := retry(ctx, func() (*telego.ForumTopic, error) {
		return b.api.CreateForumTopic(ctx, &telego.CreateForumTopicParams{ChatID: tu.ID(chat), Name: name})
	})
	if err != nil {
		return 0, fmt.Errorf("create forum topic: %w", err)
	}
	return t.MessageThreadID, nil
}

// Notify sends a plain text to a chat, ignoring the result.
func (b *Bot) Notify(ctx context.Context, log *slog.Logger, chat int64, thread, reply int, text string) {
	_, err := b.api.SendMessage(ctx, &telego.SendMessageParams{
		ChatID: tu.ID(chat), MessageThreadID: thread, Text: text, ReplyParameters: replyTo(reply),
	})
	if err != nil {
		log.Warn("notify owner", "bot", b.name, "err", err)
	}
}

// SendPhoto sends a PNG to a chat, used for the pairing QR code.
func (b *Bot) SendPhoto(ctx context.Context, chat int64, png []byte, caption string) error {
	_, err := b.api.SendPhoto(ctx, &telego.SendPhotoParams{
		ChatID: tu.ID(chat), Photo: tu.FileFromReader(bytes.NewReader(png), "qr.png"), Caption: caption,
	})
	if err != nil {
		return fmt.Errorf("send photo: %w", err)
	}
	return nil
}

// Listen long-polls updates and passes them to h until ctx ends.
func (b *Bot) Listen(ctx context.Context, log *slog.Logger, h Handlers) error {
	updates, err := b.api.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{
		Timeout: 30, AllowedUpdates: []string{"message", "edited_message", "message_reaction", "poll_answer"},
	})
	if err != nil {
		return fmt.Errorf("start long polling: %w", err)
	}
	for u := range updates {
		switch {
		case u.Message != nil && h.Message != nil:
			h.Message(ctx, u.Message)
		case u.EditedMessage != nil && h.Edited != nil:
			h.Edited(ctx, u.EditedMessage)
		case u.MessageReaction != nil && h.Reaction != nil:
			h.Reaction(ctx, ToReaction(u.MessageReaction))
		case u.PollAnswer != nil && h.PollAnswer != nil:
			a := bridge.PollAnswer{PollID: u.PollAnswer.PollID, Options: u.PollAnswer.OptionIDs}
			if u.PollAnswer.User != nil {
				a.From = u.PollAnswer.User.ID
			}
			h.PollAnswer(ctx, a)
		}
	}
	log.Info("long polling stopped", "bot", b.name)
	return nil
}

// sendNative sends locations and contacts, which carry no caption: the header and
// text go first in a message of their own.
func (b *Bot) sendNative(ctx context.Context, out bridge.Outgoing, header, text string) (int, error) {
	first := 0
	if header != "" || text != "" {
		id, err := b.sendText(ctx, out, header, text)
		if err != nil {
			return 0, err
		}
		first, out.ReplyTo = id, 0
	}
	chat := tu.ID(out.Chat)
	reply := replyTo(out.ReplyTo)
	var sent []*telego.Message
	switch {
	case out.Kind == bridge.KindLocation && out.Location != nil:
		l := out.Location
		m, err := retry(ctx, func() (*telego.Message, error) {
			if l.Title != "" || l.Address != "" {
				return b.api.SendVenue(ctx, &telego.SendVenueParams{
					ChatID: chat, MessageThreadID: out.Thread, Latitude: l.Lat, Longitude: l.Lon,
					Title: cmpOr(l.Title, l.Address), Address: cmpOr(l.Address, l.Title), ReplyParameters: reply,
				})
			}
			return b.api.SendLocation(ctx, &telego.SendLocationParams{
				ChatID: chat, MessageThreadID: out.Thread, Latitude: l.Lat, Longitude: l.Lon, ReplyParameters: reply,
			})
		})
		if err != nil {
			return first, fmt.Errorf("send location: %w", err)
		}
		sent = append(sent, m)
	case out.Kind == bridge.KindContact:
		for _, c := range out.Contacts {
			phone := c.Phone
			if phone != "" {
				phone = "+" + phone
			}
			vcard := c.VCard
			if len(vcard) > maxVCard {
				vcard = "" // a card with a photo or many fields: name and number still go through
			}
			m, err := retry(ctx, func() (*telego.Message, error) {
				return b.api.SendContact(ctx, &telego.SendContactParams{
					ChatID: chat, MessageThreadID: out.Thread, PhoneNumber: cmpOr(phone, "-"), FirstName: cmpOr(c.Name, "Contact"),
					Vcard: vcard, ReplyParameters: reply,
				})
			})
			if err != nil {
				return first, fmt.Errorf("send contact: %w", err)
			}
			sent = append(sent, m)
			reply = nil
		}
	}
	if first == 0 && len(sent) > 0 {
		first = sent[0].MessageID
	}
	return first, nil
}

// maxPollOptions is the most options a Telegram poll may have.
const maxPollOptions = 12

// SendPoll sends a non-anonymous poll, preceded by the header when there is one.
// Telegram allows 2 to 12 options of up to 100 characters and a question of 300.
func (b *Bot) SendPoll(ctx context.Context, out bridge.Outgoing) (int, string, error) {
	p := out.Poll
	header := fullHeader(out)
	if p == nil || len(p.Options) < 2 {
		id, err := b.sendText(ctx, out, header, "📊 "+pollText(p))
		return id, "", err
	}
	note := ""
	if extra := len(p.Options) - maxPollOptions; extra > 0 {
		note = fmt.Sprintf("Telegram shows %d options; also in WhatsApp: %s", maxPollOptions, strings.Join(p.Options[maxPollOptions:], ", "))
	}
	if header != "" || note != "" {
		if _, err := b.sendText(ctx, out, header, note); err != nil {
			return 0, "", err
		}
		out.ReplyTo = 0
	}
	opts := make([]telego.InputPollOption, 0, min(len(p.Options), maxPollOptions))
	for _, o := range p.Options[:min(len(p.Options), maxPollOptions)] {
		opts = append(opts, telego.InputPollOption{Text: truncateRunes(o, 100)})
	}
	anonymous := false
	m, err := retry(ctx, func() (*telego.Message, error) {
		return b.api.SendPoll(ctx, &telego.SendPollParams{
			ChatID: tu.ID(out.Chat), MessageThreadID: out.Thread, Question: truncateRunes(p.Question, 300), Options: opts,
			IsAnonymous: &anonymous, AllowsMultipleAnswers: p.Multi, ReplyParameters: replyTo(out.ReplyTo),
		})
	})
	if err != nil {
		return 0, "", fmt.Errorf("send poll: %w", err)
	}
	if m.Poll == nil {
		return m.MessageID, "", nil
	}
	return m.MessageID, m.Poll.ID, nil
}

func pollText(p *bridge.Poll) string {
	if p == nil {
		return "poll"
	}
	return p.Question + "\n• " + strings.Join(p.Options, "\n• ")
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ToIncoming converts a Telegram message to the bridge's form.
func ToIncoming(m *telego.Message) bridge.Incoming {
	in := bridge.Incoming{
		Chat: m.Chat.ID, Private: m.Chat.Type == telego.ChatTypePrivate, ID: m.MessageID,
		Kind: bridge.KindText, Text: m.Text,
	}
	if m.IsTopicMessage {
		in.Thread = m.MessageThreadID
	}
	if m.From != nil {
		in.From = m.From.ID
	}
	if r := m.ReplyToMessage; r != nil && (!m.IsTopicMessage || r.MessageID != m.MessageThreadID) {
		in.ReplyTo = r.MessageID
	}
	if m.Caption != "" {
		in.Text = m.Caption
	}
	for _, e := range m.Entities {
		if e.Type == telego.EntityTypeBotCommand && e.Offset == 0 {
			// A command to the bot itself, such as /start, is never a message for WhatsApp.
			in.Command = strings.TrimPrefix(strings.SplitN(m.Text[:min(len(m.Text), e.Length)], "@", 2)[0], "/")
			return in
		}
	}
	switch {
	case len(m.Photo) > 0:
		p := m.Photo[len(m.Photo)-1]
		in.Kind, in.File = bridge.KindPhoto, &bridge.IncomingFile{FileID: p.FileID}
	case m.Animation != nil:
		a := m.Animation
		in.Kind, in.File = bridge.KindAnimation, &bridge.IncomingFile{FileID: a.FileID, MimeType: a.MimeType, Seconds: a.Duration}
	case m.Video != nil:
		v := m.Video
		in.Kind, in.File = bridge.KindVideo, &bridge.IncomingFile{FileID: v.FileID, MimeType: v.MimeType, Seconds: v.Duration}
	case m.VideoNote != nil:
		v := m.VideoNote
		in.Kind, in.File = bridge.KindVideo, &bridge.IncomingFile{FileID: v.FileID, MimeType: "video/mp4", Seconds: v.Duration}
	case m.Voice != nil:
		v := m.Voice
		in.Kind, in.File = bridge.KindVoice, &bridge.IncomingFile{FileID: v.FileID, MimeType: v.MimeType, Seconds: v.Duration}
	case m.Audio != nil:
		a := m.Audio
		in.Kind, in.File = bridge.KindAudio, &bridge.IncomingFile{FileID: a.FileID, MimeType: a.MimeType, FileName: a.FileName, Seconds: a.Duration}
	case m.Document != nil:
		d := m.Document
		in.Kind, in.File = bridge.KindDocument, &bridge.IncomingFile{FileID: d.FileID, MimeType: d.MimeType, FileName: d.FileName}
	case m.Sticker != nil:
		s := m.Sticker
		if s.IsAnimated || s.IsVideo {
			// WhatsApp cannot show Telegram's animated stickers; send their emoji instead.
			in.Kind, in.Text = bridge.KindText, s.Emoji
			if in.Text == "" {
				in.Unsupported = "an animated sticker"
			}
		} else {
			in.Kind, in.File = bridge.KindSticker, &bridge.IncomingFile{FileID: s.FileID, MimeType: "image/webp"}
		}
	case m.Venue != nil:
		v := m.Venue
		in.Location = &bridge.Location{Lat: v.Location.Latitude, Lon: v.Location.Longitude, Title: v.Title, Address: v.Address}
	case m.Location != nil:
		in.Location = &bridge.Location{Lat: m.Location.Latitude, Lon: m.Location.Longitude}
	case m.Contact != nil:
		c := m.Contact
		name := strings.TrimSpace(c.FirstName + " " + c.LastName)
		in.Contact = &bridge.Contact{Name: name, Phone: strings.TrimPrefix(c.PhoneNumber, "+"), VCard: c.Vcard}
	case m.Poll != nil:
		p := m.Poll
		poll := &bridge.Poll{Question: p.Question, Multi: p.AllowsMultipleAnswers}
		for _, o := range p.Options {
			poll.Options = append(poll.Options, o.Text)
		}
		in.Poll = poll
	case isService(m):
		// A member joined, the title changed, a topic was edited...
		in.Service = true
	case m.Text == "":
		in.Unsupported = "this kind of message"
	}
	return in
}

// SetName changes the bot's display name when it differs from name.
func (b *Bot) SetName(ctx context.Context, name string) error {
	cur, err := b.api.GetMyName(ctx, nil)
	if err == nil && cur.Name == name {
		return nil
	}
	if err := b.api.SetMyName(ctx, &telego.SetMyNameParams{Name: name}); err != nil {
		return fmt.Errorf("set name: %w", err)
	}
	return nil
}

// SetPhoto replaces the bot's profile photo with a JPEG, or removes it for nil.
// telego cannot upload this file, so the request is built here as multipart form.
func (b *Bot) SetPhoto(ctx context.Context, jpeg []byte) error {
	if jpeg == nil {
		if err := b.api.RemoveMyProfilePhoto(ctx); err != nil {
			return fmt.Errorf("remove photo: %w", err)
		}
		return nil
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("photo", `{"type":"static","photo":"attach://avatar"}`); err != nil {
		return fmt.Errorf("set photo: %w", err)
	}
	part, err := form.CreateFormFile("avatar", "avatar.jpg")
	if err != nil {
		return fmt.Errorf("set photo: %w", err)
	}
	if _, err := part.Write(jpeg); err != nil {
		return fmt.Errorf("set photo: %w", err)
	}
	if err := form.Close(); err != nil {
		return fmt.Errorf("set photo: %w", err)
	}
	url := "https://api.telegram.org/bot" + b.token + "/setMyProfilePhoto"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return errors.New("set photo: cannot build request")
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	resp, err := b.http.Do(req)
	if err != nil {
		return errors.New("set photo: request failed") // the error would contain the token
	}
	defer func() { _ = resp.Body.Close() }()
	var answer struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&answer); err != nil {
		return fmt.Errorf("set photo: HTTP %d", resp.StatusCode)
	}
	if !answer.OK {
		return fmt.Errorf("set photo: %s", answer.Description)
	}
	return nil
}

// extension returns the usual file extension of a MIME type, or def.
func extension(mimeType, def string) string {
	base, _, _ := strings.Cut(mimeType, ";")
	switch strings.TrimSpace(base) {
	case "audio/ogg":
		return ".ogg"
	case "audio/mp4", "audio/m4a", "audio/x-m4a":
		return ".m4a"
	case "audio/aac":
		return ".aac"
	case "audio/mpeg":
		return ".mp3"
	case "audio/amr":
		return ".amr"
	case "video/mp4":
		return ".mp4"
	case "video/3gpp":
		return ".3gp"
	case "video/quicktime":
		return ".mov"
	}
	return def
}

// errorLogger passes telego's errors, such as failing long polls, to slog with the
// token removed. Debug output carries request URLs and is dropped.
type errorLogger struct {
	log   *slog.Logger
	token string
}

func (errorLogger) Debugf(string, ...any) {}

func (l errorLogger) Errorf(format string, args ...any) {
	msg := strings.ReplaceAll(fmt.Sprintf(format, args...), l.token, "<token>")
	l.log.Warn("telegram", "err", msg)
}

// isService reports whether m is a Telegram service message rather than content.
func isService(m *telego.Message) bool {
	return len(m.NewChatMembers) > 0 || m.LeftChatMember != nil || m.NewChatTitle != "" ||
		len(m.NewChatPhoto) > 0 || m.DeleteChatPhoto || m.GroupChatCreated || m.SupergroupChatCreated ||
		m.MessageAutoDeleteTimerChanged != nil || m.MigrateFromChatID != 0 || m.MigrateToChatID != 0 ||
		m.PinnedMessage != nil || m.ForumTopicCreated != nil || m.ForumTopicEdited != nil ||
		m.ForumTopicClosed != nil || m.ForumTopicReopened != nil || m.GeneralForumTopicHidden != nil ||
		m.GeneralForumTopicUnhidden != nil || m.VideoChatScheduled != nil || m.VideoChatStarted != nil ||
		m.VideoChatEnded != nil || m.VideoChatParticipantsInvited != nil || m.BoostAdded != nil ||
		m.ChatBackgroundSet != nil || m.WriteAccessAllowed != nil
}
