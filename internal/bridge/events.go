package bridge

import (
	"context"
	"errors"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/vsem-azamat/wa-tg-mirror/internal/store"
)

// editedHeader marks a message that was edited in WhatsApp.
const editedHeader = "<i>✏️ edited</i>"

// deletedText replaces a mirrored message deleted in WhatsApp when Telegram no
// longer lets the bot delete it (after 48 hours).
const deletedText = "🗑 deleted in WhatsApp"

// handleProtocol mirrors edits and deletions of WhatsApp messages. It reports
// whether m was one of them.
func (b *Bridge) handleProtocol(ctx context.Context, chat, sender types.JID, info types.MessageInfo, m *waE2E.Message) bool {
	pm := m.GetProtocolMessage()
	if pm == nil {
		return false
	}
	target := pm.GetKey().GetID()
	switch pm.GetType() {
	case waE2E.ProtocolMessage_REVOKE:
		b.revoked(ctx, chat, target)
	case waE2E.ProtocolMessage_MESSAGE_EDIT:
		b.edited(ctx, chat, sender, info, target, pm.GetEditedMessage())
	default:
	}
	return true
}

// bot returns the bot with the given ID.
func (b *Bridge) bot(id int64) (Telegram, bool) {
	if id == b.service.ID() {
		return b.service, true
	}
	p, ok := b.byBot[id]
	if !ok {
		return nil, false
	}
	return p.bot, true
}

func (b *Bridge) revoked(ctx context.Context, chat types.JID, target string) {
	links, err := b.store.LinksOf(ctx, chat.String(), target)
	if err != nil {
		b.log.Error("find deleted message", "err", err)
		return
	}
	for _, l := range links {
		// Bots may delete their own messages, incoming ones in private chats, and any
		// message where they are administrators: try the poster first, then the service bot.
		bot, ok := b.bot(l.SenderBot)
		if !ok {
			bot, ok = b.bot(l.Bot)
		}
		if !ok {
			bot = b.service
		}
		err := bot.Delete(ctx, l.TGChat, l.TGMsg)
		if err != nil && bot != b.service && l.TGChat < 0 {
			err = b.service.Delete(ctx, l.TGChat, l.TGMsg)
		}
		if err != nil && l.SenderBot != 0 {
			err = bot.Edit(ctx, l.TGChat, l.TGMsg, Outgoing{Kind: KindText, Text: deletedText})
		}
		if err != nil {
			b.log.Warn("mirror deletion", "chat", chat.User, "err", err)
		}
	}
	if err := b.store.DeleteLinks(ctx, chat.String(), target); err != nil {
		b.log.Error("forget deleted message", "err", err)
	}
}

func (b *Bridge) edited(ctx context.Context, chat, sender types.JID, info types.MessageInfo, target string, content *waE2E.Message) {
	c := describeWA(content)
	if c.skip {
		return
	}
	links, err := b.store.LinksOf(ctx, chat.String(), target)
	if err != nil {
		b.log.Error("find edited message", "err", err)
		return
	}
	for _, l := range links {
		bot, ok := b.bot(l.SenderBot)
		if !ok {
			continue // the owner's own message from Telegram: nothing to update there
		}
		header := editedHeader
		if bot == b.service {
			header = b.senderHeader(ctx, sender, info, info.IsFromMe) + " " + editedHeader
		}
		if err := bot.Edit(ctx, l.TGChat, l.TGMsg, Outgoing{Kind: KindText, Header: header, Text: c.text}); err != nil {
			b.log.Warn("mirror edit", "chat", chat.User, "err", err)
		}
	}
	if err := b.store.SetText(ctx, chat.String(), target, truncate(c.text, quoteLen)); err != nil {
		b.log.Error("update edited message", "err", err)
	}
}

// reacted mirrors a WhatsApp reaction onto the Telegram copies of its message,
// set by the bot of the person who reacted, or noted by the service bot.
func (b *Bridge) reacted(ctx context.Context, chat, sender types.JID, info types.MessageInfo, r *waE2E.ReactionMessage) {
	if info.IsFromMe {
		return // the owner's reactions from the phone have no bot to show them
	}
	links, err := b.store.LinksOf(ctx, chat.String(), r.GetKey().GetID())
	if err != nil {
		b.log.Error("find reacted message", "err", err)
		return
	}
	emoji := r.GetText()
	for _, l := range links {
		bot := b.service
		if p, ok := b.byPhone[sender.User]; ok && (l.TGChat < 0 || l.Bot == p.bot.ID()) {
			bot = p.bot
		}
		if l.TGChat > 0 && l.Bot != bot.ID() {
			// In a private chat only the chat's own bot can react.
			continue
		}
		err := bot.React(ctx, l.TGChat, l.TGMsg, emoji)
		if err == nil || emoji == "" {
			if err != nil {
				b.log.Warn("remove reaction", "err", err)
			}
			continue
		}
		// An emoji Telegram does not offer, a bot outside the group, or a second person
		// reacting through the service bot: say it in a short reply instead.
		note := Outgoing{Chat: l.TGChat, ReplyTo: l.TGMsg, Kind: KindText, Text: emoji}
		if l.TGChat < 0 {
			note.Header = b.senderHeader(ctx, sender, info, false)
		}
		if bot != b.service && l.TGChat < 0 {
			bot = b.service
		}
		if _, err := bot.Send(ctx, note); err != nil {
			b.log.Warn("mirror reaction", "chat", chat.User, "err", err)
		}
	}
}

// HandleTelegramEdit sends an edit of the owner's text message to WhatsApp.
func (b *Bridge) HandleTelegramEdit(ctx context.Context, bot Telegram, in Incoming) string {
	if in.From != b.owner || in.Command != "" {
		return ""
	}
	if _, ok, _ := b.target(ctx, bot, in); !ok {
		return ""
	}
	l, err := b.store.ByTelegram(ctx, bot.ID(), in.Chat, in.ID)
	if err != nil {
		return ""
	}
	if l.SenderBot != 0 {
		return ""
	}
	if in.Kind != KindText {
		return "⚠️ Caption edits are not sent to WhatsApp."
	}
	chat, err := types.ParseJID(l.WAChat)
	if err != nil {
		return ""
	}
	msg := b.wa.BuildEdit(chat, l.WAID, &waE2E.Message{Conversation: proto.String(in.Text)})
	if _, err := b.wa.SendMessage(ctx, chat, msg); err != nil {
		return "⚠️ Edit not sent to WhatsApp (it allows edits for 15 minutes): " + err.Error()
	}
	if err := b.store.SetText(ctx, l.WAChat, l.WAID, truncate(in.Text, quoteLen)); err != nil {
		b.log.Error("update edited message", "err", err)
	}
	return ""
}

// HandleTelegramReaction sends the owner's reaction to WhatsApp.
func (b *Bridge) HandleTelegramReaction(ctx context.Context, bot Telegram, r Reaction) {
	if r.From != b.owner {
		return
	}
	if _, ok, _ := b.target(ctx, bot, Incoming{Chat: r.Chat, Private: r.Chat > 0, From: r.From}); !ok {
		return
	}
	l, err := b.store.ByTelegram(ctx, bot.ID(), r.Chat, r.Msg)
	if err != nil {
		return
	}
	chat, err := types.ParseJID(l.WAChat)
	if err != nil {
		return
	}
	sender, err := types.ParseJID(l.WASender)
	if err != nil {
		return
	}
	emoji := r.Emoji
	if emoji == "❤" {
		emoji = "❤️" // WhatsApp shows the bare heart as a black glyph
	}
	if _, err := b.wa.SendMessage(ctx, chat, b.wa.BuildReaction(chat, sender, l.WAID, emoji)); err != nil {
		b.log.Warn("send reaction to WhatsApp", "chat", chat.User, "err", err)
	}
}

// deleteOwn deletes the owner's message that a /del command replies to, in
// WhatsApp for everyone and in Telegram.
func (b *Bridge) deleteOwn(ctx context.Context, bot Telegram, in Incoming) string {
	if in.ReplyTo == 0 {
		return "Reply with /del to your own message to delete it in WhatsApp."
	}
	l, err := b.store.ByTelegram(ctx, bot.ID(), in.Chat, in.ReplyTo)
	if errors.Is(err, store.ErrNotFound) || err == nil && l.WASender != b.dir.Own().String() {
		return "Only your own messages can be deleted in WhatsApp."
	}
	if err != nil {
		return "⚠️ " + err.Error()
	}
	chat, err := types.ParseJID(l.WAChat)
	if err != nil {
		return "⚠️ " + err.Error()
	}
	if _, err := b.wa.SendMessage(ctx, chat, b.wa.BuildRevoke(chat, b.dir.Own(), l.WAID)); err != nil {
		return "⚠️ Not deleted in WhatsApp: " + err.Error()
	}
	for _, id := range []int{in.ReplyTo, in.ID} {
		if err := bot.Delete(ctx, in.Chat, id); err != nil {
			b.log.Warn("delete in Telegram", "err", err)
		}
	}
	if err := b.store.DeleteLinks(ctx, l.WAChat, l.WAID); err != nil {
		b.log.Error("forget deleted message", "err", err)
	}
	return ""
}

// notice posts a line about a chat event (a call, members joining) where the chat
// is mirrored: the person's bot for a direct chat, the service bot for groups.
func (b *Bridge) notice(ctx context.Context, chat types.JID, isGroup bool, text string) {
	info := types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: chat, IsGroup: isGroup}}
	out := Outgoing{Kind: KindText, Text: text}
	bot, ok := b.route(ctx, chat, chat, info, &out)
	if !ok {
		return
	}
	if isGroup || bot == b.service {
		bot, out.Header = b.service, ""
	}
	if _, err := bot.Send(ctx, out); err != nil {
		b.log.Warn("chat notice", "chat", chat.User, "err", err)
	}
}

// nameList joins the display names of users.
func (b *Bridge) nameList(ctx context.Context, users []types.JID) string {
	names := make([]string, 0, len(users))
	for _, u := range users {
		pn := b.canonical(ctx, u)
		name := b.dir.Name(ctx, pn)
		if name == "" {
			name = "+" + pn.User
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// HandleGroupInfo posts changes of a mirrored WhatsApp group: members, name, description.
func (b *Bridge) HandleGroupInfo(ctx context.Context, g GroupChange) {
	var lines []string
	if g.Name != "" {
		lines = append(lines, "✏️ Group renamed to "+g.Name)
	}
	if g.TopicChanged {
		lines = append(lines, "📝 Description changed")
	}
	if len(g.Join) > 0 {
		lines = append(lines, "➕ Joined: "+b.nameList(ctx, g.Join))
	}
	if len(g.Leave) > 0 {
		lines = append(lines, "➖ Left: "+b.nameList(ctx, g.Leave))
	}
	for _, l := range lines {
		b.notice(ctx, g.Chat, true, l)
	}
}

// GroupChange is the part of a WhatsApp group update the bridge shows.
type GroupChange struct {
	Chat         types.JID
	Name         string
	TopicChanged bool
	Join, Leave  []types.JID
}

// HandleCall notes an incoming WhatsApp call in the caller's chat.
func (b *Bridge) HandleCall(ctx context.Context, from, group types.JID, video bool) {
	kind := "📞 Incoming WhatsApp call"
	if video {
		kind = "📹 Incoming WhatsApp video call"
	}
	if !group.IsEmpty() {
		b.notice(ctx, group, true, kind+" from "+b.nameList(ctx, []types.JID{from}))
		return
	}
	b.notice(ctx, b.canonical(ctx, from), false, kind+" — answer it on the phone")
}
