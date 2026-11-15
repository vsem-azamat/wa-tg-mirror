package bridge

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
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

// posterOf returns the bot that posted a link's Telegram message, and false for the
// owner's own messages. Links from before this was recorded are worked out from the
// chat and the sender.
func (b *Bridge) posterOf(ctx context.Context, l store.Link) (Telegram, bool) {
	switch l.SenderBot {
	case 0:
		return nil, false
	case store.UnknownBot:
		sender, err := types.ParseJID(l.WASender)
		if err == nil && b.dir.IsOwn(sender) && l.TGChat == b.owner {
			return nil, false
		}
		if l.TGChat > 0 {
			return b.bot(l.Bot)
		}
		if err == nil {
			if p, ok := b.byPhone[b.canonical(ctx, sender).User]; ok {
				return p.bot, true
			}
		}
		return b.service, true
	}
	return b.bot(l.SenderBot)
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
		poster, posted := b.posterOf(ctx, l)
		bot := poster
		if !posted {
			if chatBot, ok := b.bot(l.Bot); ok {
				bot = chatBot
			}
		}
		if bot == nil {
			bot = b.service
		}
		err := bot.Delete(ctx, l.TGChat, l.TGMsg)
		if err != nil && bot != b.service && l.TGChat < 0 {
			err = b.service.Delete(ctx, l.TGChat, l.TGMsg)
		}
		if err != nil && posted {
			err = poster.Edit(ctx, l.TGChat, l.TGMsg, Outgoing{Kind: KindText, Text: deletedText})
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
		bot, ok := b.posterOf(ctx, l)
		if !ok {
			continue // the owner's own message from Telegram: nothing to update there
		}
		header := editedHeader
		switch {
		case bot == b.service:
			header = b.senderHeader(ctx, sender, info, info.IsFromMe) + " " + editedHeader
		case info.IsFromMe:
			header = ownHeader + " " + editedHeader
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
		if bot == b.service && l.TGChat < 0 {
			// The service bot holds one reaction per message for everyone without a
			// bot of their own: name the reactor in a note instead. A removal stays.
			if emoji != "" {
				b.reactionNote(ctx, l, sender, info, emoji)
			}
			continue
		}
		err := bot.React(ctx, l.TGChat, l.TGMsg, emoji)
		if err == nil || emoji == "" {
			if err != nil {
				b.log.Warn("remove reaction", "err", err)
			}
			continue
		}
		// An emoji Telegram does not offer, or a bot outside the group: say it in a
		// short reply instead.
		if l.TGChat < 0 {
			b.reactionNote(ctx, l, sender, info, emoji)
			continue
		}
		note := Outgoing{Chat: l.TGChat, ReplyTo: l.TGMsg, Kind: KindText, Text: emoji}
		if _, err := bot.Send(ctx, note); err != nil {
			b.log.Warn("mirror reaction", "chat", chat.User, "err", err)
		}
	}
}

// reactionNote posts a reaction as a reply from the service bot naming the reactor.
func (b *Bridge) reactionNote(ctx context.Context, l store.Link, sender types.JID, info types.MessageInfo, emoji string) {
	note := Outgoing{
		Chat: l.TGChat, ReplyTo: l.TGMsg, Kind: KindText, Text: emoji,
		Header: b.senderHeader(ctx, sender, info, false),
	}
	if l.TGChat == b.overflow {
		if thread, err := b.store.Topic(ctx, l.WAChat); err == nil {
			note.Thread = thread
		}
	}
	if _, err := b.service.Send(ctx, note); err != nil {
		b.log.Warn("mirror reaction", "err", err)
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
	if _, posted := b.posterOf(ctx, l); posted {
		return ""
	}
	if in.Location != nil {
		return "" // a live location moving, not an edit
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
	if r.From != b.owner || r.Other {
		return
	}
	if !b.servesChat(bot, r.Chat) {
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

// servesChat reports whether bot is the one that acts on the owner's input in chat:
// a person's bot in its private chat, the service bot in mirrored groups and the forum.
func (b *Bridge) servesChat(bot Telegram, chat int64) bool {
	if _, ok := b.byBot[bot.ID()]; ok {
		return chat == b.owner
	}
	if bot.ID() != b.service.ID() {
		return false
	}
	_, group := b.byTGChat[chat]
	return group || b.overflow != 0 && chat == b.overflow
}

// deleteOwn deletes the owner's message that a /del command replies to, in
// WhatsApp for everyone and in Telegram.
func (b *Bridge) deleteOwn(ctx context.Context, bot Telegram, in Incoming) string {
	if in.ReplyTo == 0 {
		return "Reply with /del to your own message to delete it in WhatsApp."
	}
	l, err := b.store.ByTelegram(ctx, bot.ID(), in.Chat, in.ReplyTo)
	if errors.Is(err, store.ErrNotFound) || err == nil && !b.ownLink(l) {
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

// ownLink reports whether a link is to a message the owner sent.
func (b *Bridge) ownLink(l store.Link) bool {
	sender, err := types.ParseJID(l.WASender)
	return err == nil && b.dir.IsOwn(sender)
}

// notice posts a line about a chat event (a call, members joining) where the chat
// is mirrored: the person's bot for a direct chat, the service bot for groups.
func (b *Bridge) notice(ctx context.Context, chat types.JID, isGroup bool, text string) {
	_, person := b.byPhone[chat.User]
	_, group := b.groups[chat.String()]
	if !person && !group {
		// Elsewhere only in a chat that already has a topic: a call or a join in a
		// chat nobody wrote in must not open one.
		if _, err := b.store.Topic(ctx, chat.String()); err != nil {
			return
		}
	}
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

// voted notes a WhatsApp poll vote under the Telegram copy of the poll. Bots cannot
// vote for others in Telegram, so votes arrive as replies naming the voter.
func (b *Bridge) voted(ctx context.Context, evt *events.Message, chat, sender types.JID) {
	if evt.Info.IsFromMe {
		return // the owner's votes from the phone
	}
	target := evt.Message.GetPollUpdateMessage().GetPollCreationMessageKey().GetID()
	poll, err := b.store.PollByWhatsApp(ctx, chat.String(), target)
	if err != nil {
		return
	}
	vote, err := b.wa.DecryptPollVote(ctx, evt)
	if err != nil {
		b.log.Warn("decrypt poll vote", "chat", chat.User, "err", err)
		return
	}
	hashes := whatsmeow.HashPollOptions(poll.Options)
	var chosen []string
	for _, h := range vote.GetSelectedOptions() {
		for i, oh := range hashes {
			if bytes.Equal(h, oh) {
				chosen = append(chosen, poll.Options[i])
			}
		}
	}
	text := "🗳 " + strings.Join(chosen, ", ")
	if len(chosen) == 0 {
		text = "🗳 vote withdrawn"
	}
	links, err := b.store.LinksOf(ctx, chat.String(), target)
	if err != nil || len(links) == 0 {
		return
	}
	for _, l := range links {
		bot := b.service
		out := Outgoing{Chat: l.TGChat, ReplyTo: l.TGMsg, Kind: KindText, Text: text}
		switch p, has := b.byPhone[sender.User]; {
		case has && (l.TGChat < 0 || l.Bot == p.bot.ID()):
			bot = p.bot
		case l.TGChat > 0:
			continue // a private chat that is not the voter's
		default:
			out.Header = b.senderHeader(ctx, sender, evt.Info, false)
		}
		if l.TGChat == b.overflow {
			if thread, err := b.store.Topic(ctx, l.WAChat); err == nil {
				out.Thread = thread
			}
		}
		if _, err := bot.Send(ctx, out); err != nil {
			b.log.Warn("mirror poll vote", "chat", chat.User, "err", err)
		}
	}
}

// HandlePollAnswer sends the owner's vote in a mirrored poll to WhatsApp.
func (b *Bridge) HandlePollAnswer(ctx context.Context, a PollAnswer) {
	if a.From != b.owner {
		return
	}
	poll, err := b.store.PollByTelegram(ctx, a.PollID)
	if err != nil {
		return
	}
	chat, err := types.ParseJID(poll.WAChat)
	if err != nil {
		return
	}
	sender, err := types.ParseJID(poll.WASender)
	if err != nil {
		return
	}
	names := make([]string, 0, len(a.Options))
	for _, i := range a.Options {
		if i >= 0 && i < len(poll.Options) {
			names = append(names, poll.Options[i])
		}
	}
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: b.dir.IsOwn(sender), IsGroup: chat.Server == types.GroupServer},
		ID:            poll.WAID,
	}
	msg, err := b.wa.BuildPollVote(ctx, info, names)
	if err == nil {
		_, err = b.wa.SendMessage(ctx, chat, msg)
	}
	if err != nil {
		b.log.Warn("send poll vote", "chat", chat.User, "err", err)
	}
}
