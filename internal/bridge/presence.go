package bridge

import (
	"context"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// onlineWindow is how long the bridge shows the owner as online in WhatsApp after
// they last acted through the mirror. WhatsApp sends typing only to online devices,
// and while one is online the phone gets no notifications, so it stays short.
const onlineWindow = 3 * time.Minute

// presenceTimeout bounds one presence request sent when the window closes.
const presenceTimeout = 15 * time.Second

// typingEvery is how often a typing action is repeated; Telegram shows one for
// five seconds.
const typingEvery = 4 * time.Second

// typingFor is how long typing is shown after the last "composing" from WhatsApp,
// in case the "paused" that should end it never arrives.
const typingFor = 30 * time.Second

// online keeps the owner online in WhatsApp for a window after each action.
type online struct {
	mu     sync.Mutex
	window time.Duration
	on     bool // WhatsApp was told the owner is available
	until  time.Time
	timer  *time.Timer
}

// sendWA sends a message the owner wrote, then keeps the owner online so the
// replies of the other side show as typing.
func (b *Bridge) sendWA(ctx context.Context, chat types.JID, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	resp, err := b.wa.SendMessage(ctx, chat, msg)
	if err == nil {
		b.stayOnline(ctx, chat)
	}
	return resp, err
}

// stayOnline marks the owner online, or extends the window when they already are,
// and subscribes to the presence of chat. Every presence change happens under
// online.mu, so the last one sent is always the one the state records.
func (b *Bridge) stayOnline(ctx context.Context, chat types.JID) {
	b.online.mu.Lock()
	defer b.online.mu.Unlock()
	if !b.online.on {
		if err := b.wa.SendPresence(ctx, types.PresenceAvailable); err != nil {
			b.log.Warn("go online in WhatsApp", "err", err)
			return // the next action tries again
		}
		b.online.on = true
		b.subscribePeople(ctx)
	}
	if chat.Server == types.DefaultUserServer {
		if _, person := b.byPhone[chat.User]; !person {
			b.subscribe(ctx, chat)
		}
	}
	b.online.until = time.Now().Add(b.online.window)
	if b.online.timer != nil {
		b.online.timer.Stop()
	}
	b.online.timer = time.AfterFunc(b.online.window, b.goOffline)
}

// subscribePeople asks for the presence of every configured person.
func (b *Bridge) subscribePeople(ctx context.Context) {
	for _, p := range b.byPhone {
		b.subscribe(ctx, p.jid)
	}
}

func (b *Bridge) subscribe(ctx context.Context, jid types.JID) {
	if err := b.wa.SubscribePresence(ctx, jid); err != nil {
		b.log.Warn("subscribe to presence", "chat", jid.User, "err", err)
	}
}

// goOffline marks the owner unavailable once the window has passed.
func (b *Bridge) goOffline() {
	b.online.mu.Lock()
	defer b.online.mu.Unlock()
	if !b.online.on || time.Now().Before(b.online.until) {
		return // extended meanwhile; the new timer will come
	}
	b.online.timer = nil
	b.online.on = false
	ctx, cancel := context.WithTimeout(context.Background(), presenceTimeout)
	defer cancel()
	if err := b.wa.SendPresence(ctx, types.PresenceUnavailable); err != nil {
		b.log.Warn("go offline in WhatsApp", "err", err)
	}
}

// Connected restores the owner's presence after WhatsApp (re)connects: online with
// fresh subscriptions when the window is open, otherwise unavailable so the phone
// keeps its notifications.
func (b *Bridge) Connected(ctx context.Context) {
	b.online.mu.Lock()
	defer b.online.mu.Unlock()
	if b.online.on && time.Now().Before(b.online.until) {
		if err := b.wa.SendPresence(ctx, types.PresenceAvailable); err != nil {
			b.log.Warn("set WhatsApp presence", "err", err)
		}
		b.subscribePeople(ctx)
		return
	}
	b.online.on = false
	if err := b.wa.SendPresence(ctx, types.PresenceUnavailable); err != nil {
		b.log.Warn("set WhatsApp presence", "err", err)
	}
}

// typingKey names one place where typing is shown: a bot in a chat or topic.
type typingKey struct {
	bot    int64
	chat   int64
	thread int
}

// typer repeats one typing action until its deadline or until it is stopped.
type typer struct {
	until time.Time
	voice bool
	stop  chan struct{}
}

// HandleChatPresence shows that someone is typing or recording in WhatsApp through
// the bot that would mirror their message. ctx must live as long as the bridge:
// the typing action is repeated after the call returns.
func (b *Bridge) HandleChatPresence(ctx context.Context, evt *events.ChatPresence) {
	if evt.IsFromMe || evt.Chat.Server == types.BroadcastServer || evt.Chat.Server == types.NewsletterServer {
		return
	}
	chat := b.canonical(ctx, evt.Chat)
	sender := b.canonical(ctx, evt.Sender)
	if b.ignored[evt.Chat.ToNonAD().String()] {
		return
	}
	key, bot, ok := b.typingPlace(ctx, chat, sender, evt.IsGroup)
	if !ok {
		return
	}
	if evt.State != types.ChatPresenceComposing {
		b.stopTyping(key)
		return
	}
	voice := evt.Media == types.ChatPresenceMediaAudio
	b.typingMu.Lock()
	defer b.typingMu.Unlock()
	if t, ok := b.typing[key]; ok && t.voice == voice {
		t.until = time.Now().Add(typingFor)
		return
	} else if ok {
		close(t.stop)
	}
	t := &typer{until: time.Now().Add(typingFor), voice: voice, stop: make(chan struct{})}
	b.typing[key] = t
	go b.showTyping(ctx, bot, key, t)
}

// typingPlace finds where typing in a WhatsApp chat is shown. It never creates a
// forum topic: typing in a chat without one is not shown.
func (b *Bridge) typingPlace(ctx context.Context, chat, sender types.JID, isGroup bool) (typingKey, Telegram, bool) {
	if g, ok := b.groups[chat.String()]; ok {
		if p, ok := b.byPhone[sender.User]; ok {
			return typingKey{bot: p.bot.ID(), chat: g.chat}, p.bot, true
		}
		return typingKey{bot: b.service.ID(), chat: g.chat}, b.service, true
	}
	if !isGroup {
		if p, ok := b.byPhone[chat.User]; ok {
			return typingKey{bot: p.bot.ID(), chat: b.owner}, p.bot, true
		}
	}
	if b.overflow == 0 || b.ignored[chat.String()] {
		return typingKey{}, nil, false
	}
	thread, err := b.store.Topic(ctx, chat.String())
	if err != nil {
		return typingKey{}, nil, false
	}
	return typingKey{bot: b.service.ID(), chat: b.overflow, thread: thread}, b.service, true
}

// showTyping repeats the typing action of t until it ends.
func (b *Bridge) showTyping(ctx context.Context, bot Telegram, key typingKey, t *typer) {
	tick := time.NewTicker(typingEvery)
	defer tick.Stop()
	for {
		b.typingMu.Lock()
		expired := time.Now().After(t.until)
		if expired && b.typing[key] == t {
			delete(b.typing, key)
		}
		b.typingMu.Unlock()
		if expired {
			return
		}
		if err := bot.Typing(ctx, key.chat, key.thread, t.voice); err != nil {
			b.log.Debug("typing", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.stop:
			return
		case <-tick.C:
		}
	}
}

// stopTyping ends the typing shown at key, if any.
func (b *Bridge) stopTyping(key typingKey) {
	b.typingMu.Lock()
	defer b.typingMu.Unlock()
	if t, ok := b.typing[key]; ok {
		close(t.stop)
		delete(b.typing, key)
	}
}
