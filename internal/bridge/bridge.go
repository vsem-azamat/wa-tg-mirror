package bridge

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/vsem-azamat/wa-tg-mirror/internal/config"
	"github.com/vsem-azamat/wa-tg-mirror/internal/store"
)

// quoteLen is how much of a message is kept to quote it in a WhatsApp reply.
const quoteLen = 200

// quietCommands are the usual bot commands, such as the /start of a new bot; they
// are ignored without a reply.
var quietCommands = map[string]bool{"start": true, "help": true, "settings": true, "stop": true}

// maxUnread caps the unread messages remembered per chat for read receipts, so a
// busy chat the owner only reads does not grow without bound.
const maxUnread = 50

// sentMemory is how many own message IDs are remembered to drop their echo.
const sentMemory = 1000

// person is a configured contact with their bot.
type person struct {
	name    string
	botName string
	jid     types.JID
	bot     Telegram
}

// group is a configured WhatsApp group with its Telegram group.
type group struct {
	name string
	jid  types.JID
	chat int64
}

// pending is a received WhatsApp message not yet marked as read.
type pending struct {
	id     types.MessageID
	chat   types.JID
	sender types.JID
	at     time.Time
}

// Bridge routes messages between WhatsApp and the bots.
type Bridge struct {
	log     *slog.Logger
	wa      WhatsApp
	dir     Directory
	store   *store.Store
	owner   int64
	service Telegram
	markRd  bool

	byPhone   map[string]*person // phone digits → person
	byBot     map[int64]*person  // bot ID → person
	groups    map[string]*group  // WhatsApp group JID → group
	byTGChat  map[int64]*group   // Telegram chat → group
	overflow  int64              // forum chat, 0 when disabled
	ignored   map[string]bool
	topicLock sync.Mutex

	mu      sync.Mutex
	sent    map[types.MessageID]bool
	sentLog []types.MessageID
	unread  map[string][]pending // canonical chat → messages to mark read
	// notified holds the bots the owner was already told about.
	notified map[int64]bool
	// loc is the time zone of history time stamps.
	loc *time.Location
	// lastDay is the last day marked in each Telegram chat during a history run.
	lastDay map[string]string
	// lives holds when each sender last started a live location in a chat.
	lives map[string]time.Time

	online   online
	typingMu sync.Mutex
	typing   map[typingKey]*typer
}

// New builds a bridge. bots maps each person's phone to their bot.
func New(log *slog.Logger, cfg *config.Config, wa WhatsApp, dir Directory, st *store.Store,
	service Telegram, bots map[string]Telegram,
) (*Bridge, error) {
	b := &Bridge{
		log: log, wa: wa, dir: dir, store: st, owner: cfg.OwnerID, service: service, markRd: cfg.MarkRead,
		byPhone: map[string]*person{}, byBot: map[int64]*person{}, groups: map[string]*group{},
		byTGChat: map[int64]*group{}, ignored: map[string]bool{}, sent: map[types.MessageID]bool{},
		unread: map[string][]pending{}, notified: map[int64]bool{}, lastDay: map[string]string{},
		lives: map[string]time.Time{}, typing: map[typingKey]*typer{},
	}
	loc, err := time.LoadLocation(cfg.TimeZone)
	if err != nil {
		return nil, fmt.Errorf("time zone: %w", err)
	}
	b.loc = loc
	for _, p := range cfg.People {
		bot, ok := bots[p.Phone]
		if !ok {
			return nil, fmt.Errorf("no bot for %s", p.Name)
		}
		pp := &person{name: p.Name, botName: cfg.BotDisplayName(p.Name), jid: types.NewJID(p.Phone, types.DefaultUserServer), bot: bot}
		b.byPhone[p.Phone] = pp
		b.byBot[bot.ID()] = pp
	}
	for _, g := range cfg.Groups {
		jid, err := types.ParseJID(g.WAJID)
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", g.Name, err)
		}
		gg := &group{name: g.Name, jid: jid, chat: g.TGChatID}
		b.groups[jid.String()] = gg
		b.byTGChat[g.TGChatID] = gg
	}
	if o := cfg.Overflow; o != nil {
		b.overflow = o.TGChatID
		for _, j := range o.Ignore {
			b.ignored[j] = true
		}
	}
	return b, nil
}

// canonical returns the chat JID used as the key of a chat: phone-number JIDs for
// people, group JIDs for groups.
func (b *Bridge) canonical(ctx context.Context, jid types.JID) types.JID {
	jid = jid.ToNonAD()
	if jid.Server == types.HiddenUserServer {
		return b.dir.PhoneJID(ctx, jid).ToNonAD()
	}
	return jid
}

func (b *Bridge) rememberSent(id types.MessageID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent[id] = true
	b.sentLog = append(b.sentLog, id)
	if len(b.sentLog) > sentMemory {
		delete(b.sent, b.sentLog[0])
		b.sentLog = b.sentLog[1:]
	}
}

func (b *Bridge) wasSent(id types.MessageID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sent[id]
}

// HandleWhatsApp mirrors one WhatsApp message into Telegram.
func (b *Bridge) HandleWhatsApp(ctx context.Context, evt *events.Message) {
	b.mirror(ctx, evt, false)
}

// HandleHistory mirrors one message from the history sync, marked with its time
// and preceded by a day marker when the day changes in its Telegram chat. A message
// that is mirrored already is skipped. It reports whether a message was sent.
func (b *Bridge) HandleHistory(ctx context.Context, evt *events.Message) bool {
	return b.mirror(ctx, evt, true)
}

func (b *Bridge) mirror(ctx context.Context, evt *events.Message, history bool) bool {
	info := evt.Info
	if info.Chat.Server == types.BroadcastServer || info.Chat.Server == types.NewsletterServer {
		return false
	}
	if !history && info.IsFromMe && b.wasSent(info.ID) {
		return false
	}
	chat := b.canonical(ctx, info.Chat)
	sender := b.canonical(ctx, info.Sender)
	if info.IsFromMe {
		sender = b.dir.Own()
	}
	if !history {
		if b.handleProtocol(ctx, chat, sender, info, evt.Message) {
			return false
		}
		if r := evt.Message.GetReactionMessage(); r != nil {
			b.reacted(ctx, chat, sender, info, r)
			return false
		}
		if evt.Message.GetPollUpdateMessage() != nil {
			b.voted(ctx, evt, chat, sender)
			return false
		}
	}
	c := describeWA(evt.Message)
	if c.skip {
		return false
	}
	if c.location != nil && c.location.Live && !history && b.liveSeen(chat, sender, info.Timestamp) {
		return false // a position update of a live location already shown
	}
	if history {
		if done, err := b.store.HasWhatsApp(ctx, chat.String(), info.ID); err != nil || done {
			return false
		}
	}
	out := Outgoing{
		Kind: c.kind, Text: c.text, FileName: c.fileName, MimeType: c.mimeType,
		Location: c.location, Contacts: c.contacts, Poll: c.poll,
	}
	bot, ok := b.route(ctx, chat, sender, info, &out)
	if !ok {
		return false
	}
	if !history {
		b.stopTyping(typingKey{bot: bot.ID(), chat: out.Chat, thread: out.Thread})
	}
	if history {
		at := info.Timestamp.In(b.loc)
		out.Stamp = at.Format("02.01 15:04")
		b.markDay(ctx, bot, out, at)
	}
	if c.quoted != "" {
		if id, err := b.store.ByWhatsApp(ctx, chat.String(), c.quoted, bot.ID(), out.Chat); err == nil {
			out.ReplyTo = id
		}
	}
	if err := fetchMedia(ctx, b.wa, c, &out); err != nil {
		b.log.Warn("media not mirrored", "chat", chat.User, "err", err)
		out.Kind, out.Data = KindText, nil
		out.Text = fmt.Sprintf("[%s could not be downloaded]\n%s", kindName(c.kind), c.text)
	}
	bot, msgID, pollID, err := b.deliver(ctx, bot, chat, sender, info, out)
	if err != nil {
		b.log.Error("send to Telegram", "chat", chat.User, "kind", kindName(out.Kind), "err", err)
		return false
	}
	if c.poll != nil {
		ref := store.PollRef{
			WAChat: chat.String(), WAID: info.ID, WASender: info.Sender.ToNonAD().String(), Options: c.poll.Options, TGPoll: pollID,
		}
		if err := b.store.PutPoll(ctx, ref); err != nil {
			b.log.Error("store poll", "err", err)
		}
	}
	link := store.Link{
		Bot: bot.ID(), TGChat: out.Chat, TGMsg: msgID, WAChat: chat.String(), WAID: info.ID,
		WASender: info.Sender.ToNonAD().String(), Text: truncate(c.text, quoteLen), SenderBot: bot.ID(),
	}
	if err := b.store.PutLink(ctx, link); err != nil {
		b.log.Error("store link", "err", err)
	}
	if !info.IsFromMe && !history {
		b.addUnread(chat.String(), pending{id: info.ID, chat: info.Chat, sender: info.Sender, at: info.Timestamp})
	}
	return true
}

// liveWindow is how long a live location counts as the same share; WhatsApp shares
// last at most eight hours.
const liveWindow = 8 * time.Hour

// liveSeen reports whether sender shared a live location in chat within liveWindow
// before at, and remembers this one.
func (b *Bridge) liveSeen(chat, sender types.JID, at time.Time) bool {
	key := chat.String() + "/" + sender.String()
	b.mu.Lock()
	defer b.mu.Unlock()
	last, ok := b.lives[key]
	if !ok || at.Sub(last) > liveWindow {
		b.lives[key] = at
		return false
	}
	return true
}

// markDay sends a day marker through bot when out starts a new day in its chat.
func (b *Bridge) markDay(ctx context.Context, bot Telegram, out Outgoing, at time.Time) {
	key := fmt.Sprintf("%d/%d/%d", store.GroupBot(out.Chat, bot.ID()), out.Chat, out.Thread)
	day := at.Format("Mon, 2 Jan 2006")
	if b.lastDay[key] == day {
		return
	}
	b.lastDay[key] = day
	marker := Outgoing{Chat: out.Chat, Thread: out.Thread, Kind: KindText, Text: "📅 " + day}
	if _, err := bot.Send(ctx, marker); err != nil {
		b.log.Warn("day marker", "err", err)
	}
}

// deliver sends out through bot and recovers from the failures that have a way
// around: a person's bot that may not write falls back to the service bot with the
// sender's name, and a deleted overflow topic is created again. It returns the bot
// that sent the message.
func (b *Bridge) deliver(ctx context.Context, bot Telegram, chat, sender types.JID, info types.MessageInfo,
	out Outgoing,
) (Telegram, int, string, error) {
	id, poll, err := send(ctx, bot, out)
	switch {
	case err == nil:
		return bot, id, poll, nil
	case errors.Is(err, ErrForbidden) && bot.ID() != b.service.ID():
		name := "the bot"
		if p, ok := b.byBot[bot.ID()]; ok {
			name = p.name + "'s bot"
		}
		b.log.Warn("person's bot may not write; using the service bot", "chat", chat.User, "err", err)
		if out.Chat == b.owner {
			b.notifyOnce(ctx, bot.ID(), fmt.Sprintf("⚠️ %s cannot write to you yet. Open it and press Start; until then its messages come here.", name))
		}
		out.Header = b.senderHeader(ctx, sender, info, info.IsFromMe)
		if out.Chat == b.owner {
			// Private chats number messages per bot: the reply target belongs to the other bot.
			out.ReplyTo = 0
		}
		if out.Chat == b.owner && info.IsFromMe {
			out.Header = ownHeader + " → <b>" + html.EscapeString(b.dir.Name(ctx, chat)) + "</b>"
		}
		id, poll, err = send(ctx, b.service, out)
		return b.service, id, poll, err
	case errors.Is(err, ErrTopicGone) && out.Chat == b.overflow:
		if err := b.store.DeleteTopic(ctx, chat.String()); err != nil {
			return bot, 0, "", err
		}
		thread, err := b.topic(ctx, chat, info)
		if err != nil {
			return bot, 0, "", err
		}
		out.Thread, out.ReplyTo = thread, 0
		id, poll, err = send(ctx, bot, out)
		return bot, id, poll, err
	}
	return bot, 0, "", err
}

// send posts out through bot; polls go through SendPoll and return their poll ID.
func send(ctx context.Context, bot Telegram, out Outgoing) (int, string, error) {
	if out.Kind == KindPoll {
		return bot.SendPoll(ctx, out)
	}
	id, err := bot.Send(ctx, out)
	return id, "", err
}

// notifyOnce tells the owner something about a bot once per run.
func (b *Bridge) notifyOnce(ctx context.Context, bot int64, text string) {
	b.mu.Lock()
	seen := b.notified[bot]
	b.notified[bot] = true
	b.mu.Unlock()
	if seen {
		return
	}
	if _, err := b.service.Send(ctx, Outgoing{Chat: b.owner, Kind: KindText, Text: text}); err != nil {
		b.log.Warn("notify owner", "err", err)
	}
}

// route picks the bot and the Telegram chat for a WhatsApp message and sets the
// header that names the sender when the bot does not speak for them.
func (b *Bridge) route(ctx context.Context, chat, sender types.JID, info types.MessageInfo, out *Outgoing) (Telegram, bool) {
	fromMe := info.IsFromMe
	if g, ok := b.groups[chat.String()]; ok {
		out.Chat = g.chat
		if p, ok := b.byPhone[sender.User]; ok && !fromMe {
			return p.bot, true
		}
		out.Header = b.senderHeader(ctx, sender, info, fromMe)
		return b.service, true
	}
	if !info.IsGroup {
		if p, ok := b.byPhone[chat.User]; ok {
			out.Chat = b.owner
			if fromMe {
				out.Header = ownHeader
			}
			return p.bot, true
		}
	}
	if b.overflow == 0 || b.ignored[chat.String()] || b.ignored[info.Chat.ToNonAD().String()] {
		return nil, false
	}
	thread, err := b.topic(ctx, chat, info)
	if err != nil {
		b.log.Error("overflow topic", "chat", chat.User, "err", err)
		return nil, false
	}
	out.Chat, out.Thread = b.overflow, thread
	if info.IsGroup || fromMe {
		out.Header = b.senderHeader(ctx, sender, info, fromMe)
	}
	return b.service, true
}

// ownHeader marks a message the owner sent from another WhatsApp device.
const ownHeader = "<i>📱 You</i>"

func (b *Bridge) senderHeader(ctx context.Context, sender types.JID, info types.MessageInfo, fromMe bool) string {
	if fromMe {
		return ownHeader
	}
	name := b.dir.Name(ctx, sender)
	if name == "" {
		name = info.PushName
	}
	if name == "" {
		name = "+" + sender.User
	}
	return "<b>" + html.EscapeString(name) + "</b>"
}

// topic returns the overflow forum topic of chat, creating it on first use.
func (b *Bridge) topic(ctx context.Context, chat types.JID, info types.MessageInfo) (int, error) {
	b.topicLock.Lock()
	defer b.topicLock.Unlock()
	id, err := b.store.Topic(ctx, chat.String())
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	name := b.dir.Name(ctx, chat)
	if name == "" && !info.IsGroup && !info.IsFromMe {
		name = info.PushName
	}
	if name == "" {
		name = "+" + chat.User
	}
	id, err = b.service.CreateTopic(ctx, b.overflow, truncate(name, 128))
	if err != nil {
		return 0, fmt.Errorf("create topic: %w", err)
	}
	if err := b.store.PutTopic(ctx, chat.String(), b.overflow, id); err != nil {
		return 0, err
	}
	return id, nil
}

func (b *Bridge) addUnread(chat string, p pending) {
	if !b.markRd {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	list := append(b.unread[chat], p)
	if len(list) > maxUnread {
		list = list[len(list)-maxUnread:]
	}
	b.unread[chat] = list
}

// markRead sends read receipts for every unread message in chat.
func (b *Bridge) markRead(ctx context.Context, chat string) {
	b.mu.Lock()
	list := b.unread[chat]
	delete(b.unread, chat)
	b.mu.Unlock()
	bySender := map[types.JID][]pending{}
	for _, p := range list {
		bySender[p.sender] = append(bySender[p.sender], p)
	}
	for sender, ps := range bySender {
		ids := make([]types.MessageID, len(ps))
		for i, p := range ps {
			ids[i] = p.id
		}
		last := ps[len(ps)-1]
		if err := b.wa.MarkRead(ctx, ids, last.at, last.chat, sender); err != nil {
			b.log.Warn("mark read", "err", err)
		}
	}
}

// HandleTelegram sends a message the owner wrote to bot into WhatsApp. It returns
// a text to show the owner when the message was not sent.
func (b *Bridge) HandleTelegram(ctx context.Context, bot Telegram, in Incoming) (problem string) {
	if in.From != b.owner || in.Service {
		return ""
	}
	if in.Command != "" {
		// Every bot in a group sees commands; only the one that serves the chat answers.
		if _, ok, problem := b.target(ctx, bot, in); !ok {
			return problem
		}
		if in.Command == "del" {
			return b.deleteOwn(ctx, bot, in)
		}
		if quietCommands[in.Command] {
			return ""
		}
		return "⚠️ Messages that start with / are bot commands and are not sent to WhatsApp. Reply /del to your message to delete it there."
	}
	target, ok, problem := b.target(ctx, bot, in)
	if !ok {
		return problem
	}
	var q *quote
	if in.ReplyTo != 0 {
		if l, err := b.store.ByTelegram(ctx, bot.ID(), in.Chat, in.ReplyTo); err == nil && l.WAChat == target.String() {
			q = &quote{id: l.WAID, sender: l.WASender, text: l.Text}
		}
	}
	msg, err := buildWA(ctx, b.wa, bot, in, q)
	if err != nil {
		return "⚠️ Not sent to WhatsApp: " + err.Error()
	}
	resp, err := b.sendWA(ctx, target, msg)
	if err != nil {
		b.log.Error("send to WhatsApp", "chat", target.User, "err", err)
		return "⚠️ Not sent to WhatsApp: " + err.Error()
	}
	b.rememberSent(resp.ID)
	if in.Poll != nil {
		ref := store.PollRef{WAChat: target.String(), WAID: resp.ID, WASender: b.dir.Own().String(), Options: in.Poll.Options}
		if err := b.store.PutPoll(ctx, ref); err != nil {
			b.log.Error("store poll", "err", err)
		}
	}
	link := store.Link{
		Bot: bot.ID(), TGChat: in.Chat, TGMsg: in.ID, WAChat: target.String(), WAID: resp.ID,
		WASender: b.dir.Own().String(), Text: truncate(in.Text, quoteLen),
	}
	if err := b.store.PutLink(ctx, link); err != nil {
		b.log.Error("store link", "err", err)
	}
	if b.markRd {
		b.markRead(ctx, target.String())
	}
	return ""
}

// target finds the WhatsApp chat a Telegram message is meant for.
func (b *Bridge) target(ctx context.Context, bot Telegram, in Incoming) (types.JID, bool, string) {
	if p, ok := b.byBot[bot.ID()]; ok {
		// A person's bot speaks only in its private chat; in groups the service bot answers.
		if !in.Private {
			return types.JID{}, false, ""
		}
		return p.jid, true, ""
	}
	if bot.ID() != b.service.ID() {
		return types.JID{}, false, ""
	}
	if g, ok := b.byTGChat[in.Chat]; ok {
		return g.jid, true, ""
	}
	if b.overflow != 0 && in.Chat == b.overflow {
		if in.Thread == 0 {
			return types.JID{}, false, "This topic is not linked to a WhatsApp chat."
		}
		chat, err := b.store.ChatOfTopic(ctx, in.Chat, in.Thread)
		if err != nil {
			return types.JID{}, false, "This topic is not linked to a WhatsApp chat."
		}
		jid, err := types.ParseJID(chat)
		if err != nil {
			return types.JID{}, false, "This topic is not linked to a WhatsApp chat."
		}
		return jid, true, ""
	}
	if in.Private {
		return types.JID{}, false, "I mirror WhatsApp groups and other chats. Write to a person through their own bot."
	}
	return types.JID{}, false, ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
