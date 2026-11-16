// Package bridge moves messages between WhatsApp and the Telegram bots.
package bridge

import (
	"context"
	"errors"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// WhatsApp is the part of the WhatsApp client the bridge uses.
type WhatsApp interface {
	SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	Upload(ctx context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)
	MarkRead(ctx context.Context, ids []types.MessageID, ts time.Time, chat, sender types.JID, receiptTypeExtra ...types.ReceiptType) error
	BuildEdit(chat types.JID, id types.MessageID, newContent *waE2E.Message) *waE2E.Message
	BuildRevoke(chat, sender types.JID, id types.MessageID) *waE2E.Message
	BuildReaction(chat, sender types.JID, id types.MessageID, reaction string) *waE2E.Message
	BuildPollCreation(name string, optionNames []string, selectableOptionCount int) *waE2E.Message
	BuildPollVote(ctx context.Context, pollInfo *types.MessageInfo, optionNames []string) (*waE2E.Message, error)
	DecryptPollVote(ctx context.Context, vote *events.Message) (*waE2E.PollVoteMessage, error)
	SendPresence(ctx context.Context, state types.Presence) error
	SubscribePresence(ctx context.Context, jid types.JID) error
}

// Directory answers who is who on WhatsApp.
type Directory interface {
	// Own returns the account's own phone-number JID.
	Own() types.JID
	// IsOwn reports whether a JID (phone number or LID) is the account's own.
	IsOwn(jid types.JID) bool
	// PhoneJID returns the phone-number JID for a LID, or jid itself when it already is one.
	PhoneJID(ctx context.Context, jid types.JID) types.JID
	// Name returns the best display name known for a user or group.
	Name(ctx context.Context, jid types.JID) string
}

// Kind is the type of a Telegram message the bridge sends.
type Kind int

// The kinds of outgoing Telegram messages.
const (
	KindText Kind = iota
	KindPhoto
	KindVideo
	KindAnimation
	KindVoice
	KindAudio
	KindDocument
	KindSticker
	KindLocation
	KindContact
	KindPoll
)

// Location is a point on the map, with a place name for a venue.
type Location struct {
	Lat, Lon float64
	Title    string
	Address  string
	// Live marks a shared live location; only its first position is mirrored.
	Live bool
}

// Contact is a shared contact card.
type Contact struct {
	Name  string
	Phone string // international, digits only; may be empty
	VCard string
}

// Poll is a poll's question and options.
type Poll struct {
	Question string
	Options  []string
	// Multi allows choosing more than one option.
	Multi bool
}

// Outgoing is one message for a bot to send.
type Outgoing struct {
	Chat    int64
	Thread  int // forum topic, 0 for none
	ReplyTo int // Telegram message to reply to, 0 for none
	Kind    Kind
	// Stamp is a plain time stamp shown first, for messages from the history.
	Stamp string
	// Header is trusted HTML put before the text, such as the sender's name in bold.
	Header string
	// Text is the message text or caption, sent as plain text.
	Text     string
	Data     []byte
	FileName string
	MimeType string
	Location *Location
	Contacts []Contact
	Poll     *Poll
}

// IncomingFile is a file attached to a message the owner sent in Telegram.
type IncomingFile struct {
	FileID   string
	FileName string
	MimeType string
	Seconds  int
}

// Telegram is one bot.
type Telegram interface {
	// ID is the bot's user ID.
	ID() int64
	// Send sends out and returns the ID of the first message it produced.
	Send(ctx context.Context, out Outgoing) (int, error)
	// Download fetches a file the owner sent.
	Download(ctx context.Context, fileID string) ([]byte, error)
	// CreateTopic creates a forum topic and returns its thread ID.
	CreateTopic(ctx context.Context, chat int64, name string) (int, error)
	// Edit replaces the text, or the caption of a media message, of a message the bot sent.
	Edit(ctx context.Context, chat int64, msg int, out Outgoing) error
	// Delete deletes a message.
	Delete(ctx context.Context, chat int64, msg int) error
	// React sets the bot's reaction on a message; "" removes it.
	React(ctx context.Context, chat int64, msg int, emoji string) error
	// SendPoll sends out.Poll as a non-anonymous poll and returns its message and poll IDs.
	SendPoll(ctx context.Context, out Outgoing) (int, string, error)
	// Typing shows that the bot is typing, or recording a voice message, for a few seconds.
	Typing(ctx context.Context, chat int64, thread int, voice bool) error
}

// PollAnswer is the owner's vote in a mirrored poll.
type PollAnswer struct {
	PollID  string
	From    int64
	Options []int // chosen option indexes; empty when the vote was retracted
}

// Profile is a bot that can change its own name and photo.
type Profile interface {
	Telegram
	// SetName changes the bot's display name when it differs.
	SetName(ctx context.Context, name string) error
	// SetPhoto replaces the bot's profile photo; nil removes it.
	SetPhoto(ctx context.Context, jpeg []byte) error
}

// Pictures fetches WhatsApp profile pictures.
type Pictures interface {
	// Picture returns the current picture ID and image of a user, or "" when there is none.
	Picture(ctx context.Context, jid types.JID) (id string, jpeg []byte, err error)
}

// Reaction is a reaction the owner changed in Telegram.
type Reaction struct {
	Chat  int64
	Msg   int
	From  int64
	Emoji string // "" when the reaction was removed
	// Other marks a reaction WhatsApp cannot show (custom emoji, paid stars).
	Other bool
}

// Incoming is a message the owner sent to a bot.
type Incoming struct {
	Chat    int64
	Private bool
	Thread  int // forum topic, 0 outside forums
	ID      int
	From    int64
	ReplyTo int // the message it replies to, 0 for none
	Kind    Kind
	Text    string // text or caption
	File    *IncomingFile
	// Location is set for a shared location or venue.
	Location *Location
	Contact  *Contact
	Poll     *Poll
	// Unsupported names a message type the bridge cannot send, such as an animated sticker.
	Unsupported string
	// Service marks a Telegram service message (members, title, topics), never sent on.
	Service bool
	// Command is the name of a bot command such as "start"; commands are not sent on.
	Command string
}

// Errors a Telegram implementation reports so the bridge can recover.
var (
	// ErrForbidden means the bot may not write to the chat: it is not a member, or the
	// owner never started it.
	ErrForbidden = errors.New("bot may not write to this chat")
	// ErrTopicGone means the forum topic was deleted.
	ErrTopicGone = errors.New("forum topic no longer exists")
)
