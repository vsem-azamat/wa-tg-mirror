package bridge

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	"github.com/vsem-azamat/wa-tg-mirror/internal/config"
	"github.com/vsem-azamat/wa-tg-mirror/internal/store"
)

const (
	owner      int64 = 1000
	groupChat  int64 = -1001111111111
	forumChat  int64 = -100777
	alicePhone       = "111"
	bobPhone         = "222"
	ownPhone         = "999"
	groupJID         = "123-456@g.us"
	strangerPN       = "333"
)

type fakeWA struct {
	mu       sync.Mutex
	sent     []sentWA
	read     []types.MessageID
	download []byte
	nextID   int
	// voteFor and voteOpts record the last BuildPollVote; voteChoice is what DecryptPollVote returns.
	voteFor    string
	voteOpts   []string
	voteChoice []string
	// presence records SendPresence states; subscribed records SubscribePresence.
	presence   []types.Presence
	subscribed []types.JID
}

func (f *fakeWA) SendPresence(_ context.Context, state types.Presence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presence = append(f.presence, state)
	return nil
}

func (f *fakeWA) SubscribePresence(_ context.Context, jid types.JID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribed = append(f.subscribed, jid)
	return nil
}

type sentWA struct {
	to  types.JID
	msg *waE2E.Message
}

func (f *fakeWA) SendMessage(_ context.Context, to types.JID, msg *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.sent = append(f.sent, sentWA{to, msg})
	return whatsmeow.SendResponse{ID: "OUT" + strconv.Itoa(f.nextID)}, nil
}

func (f *fakeWA) BuildEdit(_ types.JID, id types.MessageID, c *waE2E.Message) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String(id)}, EditedMessage: c,
	}}
}

func (f *fakeWA) BuildRevoke(_, _ types.JID, id types.MessageID) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String(id)},
	}}
}

func (f *fakeWA) BuildReaction(_, sender types.JID, id types.MessageID, r string) *waE2E.Message {
	return &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: &waCommon.MessageKey{ID: proto.String(id), Participant: proto.String(sender.String())}, Text: proto.String(r),
	}}
}

func (f *fakeWA) BuildPollCreation(name string, opts []string, selectable int) *waE2E.Message {
	pc := &waE2E.PollCreationMessage{Name: proto.String(name), SelectableOptionsCount: proto.Uint32(uint32(selectable))}
	for _, o := range opts {
		pc.Options = append(pc.Options, &waE2E.PollCreationMessage_Option{OptionName: proto.String(o)})
	}
	return &waE2E.Message{PollCreationMessage: pc}
}

func (f *fakeWA) BuildPollVote(_ context.Context, info *types.MessageInfo, opts []string) (*waE2E.Message, error) {
	f.voteFor, f.voteOpts = info.ID, opts
	return &waE2E.Message{Conversation: proto.String("vote")}, nil
}

func (f *fakeWA) DecryptPollVote(context.Context, *events.Message) (*waE2E.PollVoteMessage, error) {
	return &waE2E.PollVoteMessage{SelectedOptions: whatsmeow.HashPollOptions(f.voteChoice)}, nil
}

func (f *fakeWA) Upload(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	return whatsmeow.UploadResponse{URL: "u", DirectPath: "d", FileLength: 3}, nil
}

func (f *fakeWA) Download(context.Context, whatsmeow.DownloadableMessage) ([]byte, error) {
	return f.download, nil
}

func (f *fakeWA) MarkRead(_ context.Context, ids []types.MessageID, _ time.Time, _, _ types.JID, _ ...types.ReceiptType) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read = append(f.read, ids...)
	return nil
}

type fakeDir struct{ names map[string]string }

func (fakeDir) Own() types.JID { return types.NewJID(ownPhone, types.DefaultUserServer) }

func (fakeDir) IsOwn(jid types.JID) bool { return jid.User == ownPhone }

func (fakeDir) PhoneJID(_ context.Context, jid types.JID) types.JID {
	if jid.Server == types.HiddenUserServer && jid.User == "lid-alice" {
		return types.NewJID(alicePhone, types.DefaultUserServer)
	}
	return jid
}

func (d fakeDir) Name(_ context.Context, jid types.JID) string {
	return d.names[jid.ToNonAD().String()]
}

type fakeBot struct {
	id      int64
	mu      sync.Mutex
	out     []Outgoing
	topics  []string
	nextID  int
	edits   []Outgoing
	deleted []int
	reacted []string // "msg:emoji"
	typed   []string // "chat/thread/typing" or ".../voice"
}

func (b *fakeBot) Typing(_ context.Context, chat int64, thread int, voice bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	action := "typing"
	if voice {
		action = "voice"
	}
	b.typed = append(b.typed, strconv.FormatInt(chat, 10)+"/"+strconv.Itoa(thread)+"/"+action)
	return nil
}

func (b *fakeBot) typedCopy() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.typed...)
}

func (b *fakeBot) SendPoll(ctx context.Context, out Outgoing) (int, string, error) {
	id, err := b.Send(ctx, out)
	return id, "poll-" + strconv.Itoa(id), err
}

func (b *fakeBot) Edit(_ context.Context, _ int64, msg int, out Outgoing) error {
	out.ReplyTo = msg
	b.edits = append(b.edits, out)
	return nil
}

func (b *fakeBot) Delete(_ context.Context, _ int64, msg int) error {
	b.deleted = append(b.deleted, msg)
	return nil
}

func (b *fakeBot) React(_ context.Context, _ int64, msg int, emoji string) error {
	b.reacted = append(b.reacted, strconv.Itoa(msg)+":"+emoji)
	return nil
}

func (b *fakeBot) ID() int64 { return b.id }

func (b *fakeBot) Send(_ context.Context, out Outgoing) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	b.out = append(b.out, out)
	return b.nextID, nil
}

func (b *fakeBot) Download(context.Context, string) ([]byte, error) { return []byte("img"), nil }

func (b *fakeBot) CreateTopic(_ context.Context, _ int64, name string) (int, error) {
	b.topics = append(b.topics, name)
	return 40 + len(b.topics), nil
}

type fixture struct {
	b       *Bridge
	wa      *fakeWA
	service *fakeBot
	alice   *fakeBot
	bob     *fakeBot
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := store.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		OwnerID: owner, MarkRead: true,
		People:   []config.Person{{Name: "Alice", Phone: alicePhone}, {Name: "Bob", Phone: bobPhone}},
		Groups:   []config.Group{{Name: "Family", WAJID: groupJID, TGChatID: groupChat}},
		Overflow: &config.Overflow{TGChatID: forumChat, Ignore: []string{"0@s.whatsapp.net"}},
	}
	f := &fixture{wa: &fakeWA{download: []byte("jpg")}, service: &fakeBot{id: 1}, alice: &fakeBot{id: 2}, bob: &fakeBot{id: 3}}
	dir := fakeDir{names: map[string]string{strangerPN + "@s.whatsapp.net": "Stranger <x>"}}
	f.b, err = New(slog.New(slog.DiscardHandler), cfg, f.wa, dir, st, f.service,
		map[string]Telegram{alicePhone: f.alice, bobPhone: f.bob})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func pn(user string) types.JID { return types.NewJID(user, types.DefaultUserServer) }

func waText(id, text string, chat, sender types.JID, fromMe bool) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: fromMe, IsGroup: chat.Server == types.GroupServer},
			ID:            id, Timestamp: time.Unix(100, 0),
		},
		Message: &waE2E.Message{Conversation: proto.String(text)},
	}
}

func last(t *testing.T, b *fakeBot) Outgoing {
	t.Helper()
	if len(b.out) == 0 {
		t.Fatal("bot sent nothing")
	}
	return b.out[len(b.out)-1]
}

func TestDirectMessageGoesThroughThePersonsBot(t *testing.T) {
	f := newFixture(t)
	// WhatsApp addresses Alice by her LID; the bridge resolves it to her phone number.
	lid := types.NewJID("lid-alice", types.HiddenUserServer)
	f.b.HandleWhatsApp(t.Context(), waText("A1", "hi", lid, lid, false))

	out := last(t, f.alice)
	if out.Chat != owner || out.Text != "hi" || out.Header != "" {
		t.Fatalf("got %+v", out)
	}
	if len(f.service.out) != 0 || len(f.bob.out) != 0 {
		t.Fatal("other bots must stay silent")
	}
}

func TestGroupMessageFromPersonUsesTheirBot(t *testing.T) {
	f := newFixture(t)
	g, _ := types.ParseJID(groupJID)
	f.b.HandleWhatsApp(t.Context(), waText("G1", "hello all", g, pn(bobPhone), false))

	out := last(t, f.bob)
	if out.Chat != groupChat || out.Header != "" {
		t.Fatalf("got %+v", out)
	}
}

func TestGroupMessageFromStrangerNamesThemThroughServiceBot(t *testing.T) {
	f := newFixture(t)
	g, _ := types.ParseJID(groupJID)
	f.b.HandleWhatsApp(t.Context(), waText("G2", "hey", g, pn(strangerPN), false))

	out := last(t, f.service)
	if out.Chat != groupChat || out.Header != "<b>Stranger &lt;x&gt;</b>" {
		t.Fatalf("got %+v", out)
	}
}

func TestOverflowChatGetsOneTopic(t *testing.T) {
	f := newFixture(t)
	s := pn(strangerPN)
	f.b.HandleWhatsApp(t.Context(), waText("S1", "one", s, s, false))
	f.b.HandleWhatsApp(t.Context(), waText("S2", "two", s, s, false))

	if len(f.service.topics) != 1 || f.service.topics[0] != "Stranger <x>" {
		t.Fatalf("topics %v", f.service.topics)
	}
	out := last(t, f.service)
	if out.Chat != forumChat || out.Thread != 41 || out.Header != "" {
		t.Fatalf("got %+v", out)
	}
}

func TestIgnoredChatIsDropped(t *testing.T) {
	f := newFixture(t)
	f.b.HandleWhatsApp(t.Context(), waText("X", "promo", pn("0"), pn("0"), false))
	if len(f.service.out) != 0 || len(f.service.topics) != 0 {
		t.Fatal("ignored chat was mirrored")
	}
}

func TestOwnerReplyInBotChatReachesWhatsAppAsQuote(t *testing.T) {
	f := newFixture(t)
	a := pn(alicePhone)
	f.b.HandleWhatsApp(t.Context(), waText("A1", "question?", a, a, false))
	tgID := last(t, f.alice).Chat // owner chat
	_ = tgID

	problem := f.b.HandleTelegram(t.Context(), f.alice, Incoming{
		Chat: owner, Private: true, ID: 77, From: owner, ReplyTo: 1, Kind: KindText, Text: "answer",
	})
	if problem != "" {
		t.Fatal(problem)
	}
	if len(f.wa.sent) != 1 {
		t.Fatalf("sent %d", len(f.wa.sent))
	}
	s := f.wa.sent[0]
	if s.to != a {
		t.Fatalf("sent to %s", s.to)
	}
	ci := s.msg.GetExtendedTextMessage().GetContextInfo()
	if s.msg.GetExtendedTextMessage().GetText() != "answer" || ci.GetStanzaID() != "A1" || ci.GetQuotedMessage().GetConversation() != "question?" {
		t.Fatalf("message %v", s.msg)
	}
	if len(f.wa.read) != 1 || f.wa.read[0] != "A1" {
		t.Fatalf("read receipts %v", f.wa.read)
	}
}

func TestEchoOfOwnSendIsNotMirroredBack(t *testing.T) {
	f := newFixture(t)
	f.b.HandleTelegram(t.Context(), f.alice, Incoming{Chat: owner, Private: true, ID: 5, From: owner, Kind: KindText, Text: "x"})
	a := pn(alicePhone)
	f.b.HandleWhatsApp(t.Context(), waText("OUT1", "x", a, pn(ownPhone), true))
	if len(f.alice.out) != 0 {
		t.Fatal("own message echoed")
	}
	// A message the owner typed on the phone is shown, marked as theirs.
	f.b.HandleWhatsApp(t.Context(), waText("PHONE1", "from phone", a, pn(ownPhone), true))
	if out := last(t, f.alice); out.Header != ownHeader {
		t.Fatalf("got %+v", out)
	}
}

func TestServiceBotSendsGroupAndTopicMessages(t *testing.T) {
	f := newFixture(t)
	if p := f.b.HandleTelegram(t.Context(), f.service, Incoming{Chat: groupChat, ID: 1, From: owner, Kind: KindText, Text: "to group"}); p != "" {
		t.Fatal(p)
	}
	s := pn(strangerPN)
	f.b.HandleWhatsApp(t.Context(), waText("S1", "one", s, s, false))
	if p := f.b.HandleTelegram(t.Context(), f.service, Incoming{Chat: forumChat, Thread: 41, ID: 2, From: owner, Kind: KindPhoto, File: &IncomingFile{FileID: "f"}}); p != "" {
		t.Fatal(p)
	}
	if len(f.wa.sent) != 2 || f.wa.sent[0].to.String() != groupJID || f.wa.sent[1].to != s || f.wa.sent[1].msg.GetImageMessage() == nil {
		t.Fatalf("sent %+v", f.wa.sent)
	}
	if p := f.b.HandleTelegram(t.Context(), f.service, Incoming{Chat: forumChat, Thread: 99, ID: 3, From: owner, Kind: KindText, Text: "?"}); p == "" {
		t.Fatal("unknown topic must be reported")
	}
}

func TestOthersAndPersonBotsInGroupsAreIgnored(t *testing.T) {
	f := newFixture(t)
	f.b.HandleTelegram(t.Context(), f.alice, Incoming{Chat: owner, Private: true, ID: 1, From: 4242, Kind: KindText, Text: "spam"})
	f.b.HandleTelegram(t.Context(), f.alice, Incoming{Chat: groupChat, ID: 2, From: owner, Kind: KindText, Text: "dup"})
	if len(f.wa.sent) != 0 {
		t.Fatalf("sent %+v", f.wa.sent)
	}
}

func TestMediaIsDownloadedAndKindKept(t *testing.T) {
	f := newFixture(t)
	a := pn(alicePhone)
	evt := waText("V1", "", a, a, false)
	evt.Message = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true), Mimetype: proto.String("audio/ogg; codecs=opus")}}
	f.b.HandleWhatsApp(t.Context(), evt)
	out := last(t, f.alice)
	if out.Kind != KindVoice || string(out.Data) != "jpg" {
		t.Fatalf("got %+v", out)
	}
}

// failingBot fails its first sends with err, then behaves like fakeBot.
type failingBot struct {
	fakeBot
	err   error
	fails int
}

func (b *failingBot) Send(ctx context.Context, out Outgoing) (int, error) {
	if b.fails > 0 {
		b.fails--
		return 0, b.err
	}
	return b.fakeBot.Send(ctx, out)
}

func TestForbiddenPersonBotFallsBackToServiceBot(t *testing.T) {
	f := newFixture(t)
	blocked := &failingBot{fakeBot: fakeBot{id: 2}, err: ErrForbidden, fails: 99}
	f.b.byPhone[alicePhone].bot = blocked
	f.b.byBot[2].bot = blocked
	g, _ := types.ParseJID(groupJID)
	f.b.HandleWhatsApp(t.Context(), waText("G1", "hi", g, pn(alicePhone), false))

	out := last(t, f.service)
	if out.Chat != groupChat || out.Text != "hi" || out.Header == "" {
		t.Fatalf("got %+v", out)
	}

	a := pn(alicePhone)
	f.b.HandleWhatsApp(t.Context(), waText("A1", "dm", a, a, false))
	f.b.HandleWhatsApp(t.Context(), waText("A2", "dm2", a, a, false))
	notes := 0
	for _, o := range f.service.out {
		if o.Chat == owner && strings.Contains(o.Text, "press Start") {
			notes++
		}
	}
	if notes != 1 || last(t, f.service).Text != "dm2" {
		t.Fatalf("notes=%d out=%+v", notes, f.service.out)
	}
}

func TestDeletedTopicIsCreatedAgain(t *testing.T) {
	f := newFixture(t)
	s := pn(strangerPN)
	f.b.HandleWhatsApp(t.Context(), waText("S1", "one", s, s, false))
	svc := &failingBot{fakeBot: fakeBot{id: f.service.id, topics: f.service.topics}, err: ErrTopicGone, fails: 1}
	f.b.service = svc
	f.b.HandleWhatsApp(t.Context(), waText("S2", "two", s, s, false))

	if len(svc.topics) != 2 {
		t.Fatalf("topics %v", svc.topics)
	}
	if out := last(t, &svc.fakeBot); out.Thread != 42 || out.Text != "two" {
		t.Fatalf("got %+v", out)
	}
}

func TestLargeMediaIsNotDownloaded(t *testing.T) {
	f := newFixture(t)
	a := pn(alicePhone)
	evt := waText("D1", "", a, a, false)
	evt.Message = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileLength: proto.Uint64(1 << 31)}}
	f.wa.download = nil
	f.b.HandleWhatsApp(t.Context(), evt)
	if out := last(t, f.alice); out.Kind != KindText || !strings.Contains(out.Text, "too large") {
		t.Fatalf("got %+v", out)
	}
}

func TestOwnerServiceMessagesAreIgnored(t *testing.T) {
	f := newFixture(t)
	if p := f.b.HandleTelegram(t.Context(), f.service, Incoming{Chat: groupChat, ID: 1, From: owner, Service: true}); p != "" {
		t.Fatal(p)
	}
	if p := f.b.HandleTelegram(t.Context(), f.alice, Incoming{Chat: owner, Private: true, ID: 2, From: owner, Command: "start", Text: "/start"}); p != "" {
		t.Fatal(p)
	}
	if p := f.b.HandleTelegram(t.Context(), f.alice, Incoming{Chat: owner, Private: true, ID: 3, From: owner, Command: "shrug", Text: "/shrug"}); p == "" {
		t.Fatal("an unknown command must be reported")
	}
	if len(f.wa.sent) != 0 {
		t.Fatal("service message sent to WhatsApp")
	}
}

func TestHistoryIsStampedMarkedByDayAndNotRepeated(t *testing.T) {
	f := newFixture(t)
	a := pn(alicePhone)
	day1 := waText("H1", "old", a, a, false)
	day1.Info.Timestamp = time.Date(2026, 10, 20, 9, 5, 0, 0, time.UTC)
	same := waText("H2", "same day", a, a, false)
	same.Info.Timestamp = time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC)
	day2 := waText("H3", "next", a, a, false)
	day2.Info.Timestamp = time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC)

	for _, m := range []*events.Message{day1, same, day2} {
		if !f.b.HandleHistory(t.Context(), m) {
			t.Fatalf("%s not mirrored", m.Info.ID)
		}
	}
	var texts []string
	for _, o := range f.alice.out {
		texts = append(texts, o.Stamp+"|"+o.Text)
	}
	want := []string{
		"|📅 Tue, 20 Oct 2026", "20.10 09:05|old", "20.10 18:00|same day",
		"|📅 Wed, 21 Oct 2026", "21.10 07:00|next",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s", strings.Join(texts, "\n"))
	}
	if f.b.HandleHistory(t.Context(), day1) {
		t.Fatal("a mirrored message was sent again")
	}
	if len(f.wa.read) != 0 {
		t.Fatal("history must not send read receipts")
	}
}

func protocol(id string, t waE2E.ProtocolMessage_Type, target string, chat, sender types.JID, edited *waE2E.Message) *events.Message {
	m := waText(id, "", chat, sender, false)
	m.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: t.Enum(), Key: &waCommon.MessageKey{ID: proto.String(target)}, EditedMessage: edited,
	}}
	return m
}

func TestWhatsAppEditAndDeleteReachTheMirroredMessage(t *testing.T) {
	f := newFixture(t)
	a := pn(alicePhone)
	f.b.HandleWhatsApp(t.Context(), waText("A1", "hello", a, a, false))
	f.b.HandleWhatsApp(t.Context(), protocol("E1", waE2E.ProtocolMessage_MESSAGE_EDIT, "A1", a, a,
		&waE2E.Message{Conversation: proto.String("hello!")}))
	if len(f.alice.edits) != 1 || f.alice.edits[0].Text != "hello!" || f.alice.edits[0].ReplyTo != 1 {
		t.Fatalf("edits %+v", f.alice.edits)
	}
	if len(f.alice.out) != 1 {
		t.Fatal("an edit must not post a new message")
	}
	f.b.HandleWhatsApp(t.Context(), protocol("R1", waE2E.ProtocolMessage_REVOKE, "A1", a, a, nil))
	if len(f.alice.deleted) != 1 || f.alice.deleted[0] != 1 {
		t.Fatalf("deleted %v", f.alice.deleted)
	}
}

func TestWhatsAppReactionIsSetByTheReactorsBot(t *testing.T) {
	f := newFixture(t)
	g, _ := types.ParseJID(groupJID)
	f.b.HandleWhatsApp(t.Context(), waText("G1", "news", g, pn(bobPhone), false))
	react := waText("X1", "", g, pn(alicePhone), false)
	react.Message = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: &waCommon.MessageKey{ID: proto.String("G1")}, Text: proto.String("❤️"),
	}}
	f.b.HandleWhatsApp(t.Context(), react)
	if len(f.alice.reacted) != 1 || f.alice.reacted[0] != "1:❤️" {
		t.Fatalf("reactions %v", f.alice.reacted)
	}
}

func TestOwnerReactionEditAndDeleteGoToWhatsApp(t *testing.T) {
	f := newFixture(t)
	a := pn(alicePhone)
	f.b.HandleWhatsApp(t.Context(), waText("A1", "hi", a, a, false))
	f.b.HandleTelegramReaction(t.Context(), f.alice, Reaction{Chat: owner, Msg: 1, From: owner, Emoji: "❤"})
	r := f.wa.sent[len(f.wa.sent)-1].msg.GetReactionMessage()
	if r.GetKey().GetID() != "A1" || r.GetText() != "❤️" {
		t.Fatalf("reaction %v", r)
	}

	f.b.HandleTelegram(t.Context(), f.alice, Incoming{Chat: owner, Private: true, ID: 50, From: owner, Kind: KindText, Text: "typo"})
	sentID := "OUT" + strconv.Itoa(f.wa.nextID)
	if p := f.b.HandleTelegramEdit(t.Context(), f.alice, Incoming{Chat: owner, Private: true, ID: 50, From: owner, Kind: KindText, Text: "fixed"}); p != "" {
		t.Fatal(p)
	}
	e := f.wa.sent[len(f.wa.sent)-1].msg.GetProtocolMessage()
	if e.GetType() != waE2E.ProtocolMessage_MESSAGE_EDIT || e.GetKey().GetID() != sentID || e.GetEditedMessage().GetConversation() != "fixed" {
		t.Fatalf("edit %v", e)
	}

	if p := f.b.HandleTelegram(t.Context(), f.alice, Incoming{Chat: owner, Private: true, ID: 51, From: owner, Command: "del", ReplyTo: 1}); p == "" {
		t.Fatal("someone else's message must not be deletable")
	}
	if p := f.b.HandleTelegram(t.Context(), f.alice, Incoming{Chat: owner, Private: true, ID: 52, From: owner, Command: "del", ReplyTo: 50}); p != "" {
		t.Fatal(p)
	}
	d := f.wa.sent[len(f.wa.sent)-1].msg.GetProtocolMessage()
	if d.GetType() != waE2E.ProtocolMessage_REVOKE || d.GetKey().GetID() != sentID {
		t.Fatalf("revoke %v", d)
	}
	if len(f.alice.deleted) != 2 || f.alice.deleted[0] != 50 || f.alice.deleted[1] != 52 {
		t.Fatalf("deleted in Telegram %v", f.alice.deleted)
	}
}

func TestGroupChangesAndCallsAreNoted(t *testing.T) {
	f := newFixture(t)
	g, _ := types.ParseJID(groupJID)
	f.b.HandleGroupInfo(t.Context(), GroupChange{Chat: g, Name: "New name", Join: []types.JID{pn(strangerPN)}})
	if len(f.service.out) != 2 || f.service.out[0].Chat != groupChat || !strings.Contains(f.service.out[1].Text, "Stranger") {
		t.Fatalf("notices %+v", f.service.out)
	}
	f.b.HandleCall(t.Context(), pn(alicePhone), types.EmptyJID, true)
	if out := last(t, f.alice); out.Chat != owner || !strings.Contains(out.Text, "video call") {
		t.Fatalf("call %+v", out)
	}
}

func TestEditsReachLinksStoredBeforeSenderBotExisted(t *testing.T) {
	f := newFixture(t)
	a := pn(alicePhone)
	// A link as the migration leaves it: the posting bot unknown.
	if err := f.b.store.PutLink(t.Context(), store.Link{
		Bot: f.alice.id, TGChat: owner, TGMsg: 9, WAChat: a.String(), WAID: "OLD", WASender: a.String(), SenderBot: store.UnknownBot,
	}); err != nil {
		t.Fatal(err)
	}
	f.b.HandleWhatsApp(t.Context(), protocol("E1", waE2E.ProtocolMessage_MESSAGE_EDIT, "OLD", a, a,
		&waE2E.Message{Conversation: proto.String("new")}))
	if len(f.alice.edits) != 1 || f.alice.edits[0].ReplyTo != 9 {
		t.Fatalf("edits %+v", f.alice.edits)
	}
}

func TestLiveLocationAndUnshowableReactionsAreIgnored(t *testing.T) {
	f := newFixture(t)
	f.b.HandleTelegram(t.Context(), f.alice, Incoming{Chat: owner, Private: true, ID: 7, From: owner, Location: &Location{Lat: 1, Lon: 2}})
	n := len(f.wa.sent)
	f.b.HandleTelegramEdit(t.Context(), f.alice, Incoming{Chat: owner, Private: true, ID: 7, From: owner, Location: &Location{Lat: 1, Lon: 3}})
	f.b.HandleTelegramReaction(t.Context(), f.alice, Reaction{Chat: owner, Msg: 7, From: owner, Other: true})
	if len(f.wa.sent) != n {
		t.Fatalf("sent %d more", len(f.wa.sent)-n)
	}
}

func TestOwnerReactionInForumReachesWhatsApp(t *testing.T) {
	f := newFixture(t)
	s := pn(strangerPN)
	f.b.HandleWhatsApp(t.Context(), waText("S1", "hey", s, s, false))
	f.b.HandleTelegramReaction(t.Context(), f.service, Reaction{Chat: forumChat, Msg: 1, From: owner, Emoji: "👍"})
	if r := f.wa.sent[len(f.wa.sent)-1].msg.GetReactionMessage(); r.GetKey().GetID() != "S1" {
		t.Fatalf("sent %+v", f.wa.sent)
	}
}

func TestReactionsOfPeopleWithoutBotAreNamedNotes(t *testing.T) {
	f := newFixture(t)
	g, _ := types.ParseJID(groupJID)
	f.b.HandleWhatsApp(t.Context(), waText("G1", "news", g, pn(bobPhone), false))
	react := waText("X1", "", g, pn(strangerPN), false)
	react.Message = &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: &waCommon.MessageKey{ID: proto.String("G1")}, Text: proto.String("👍"),
	}}
	f.b.HandleWhatsApp(t.Context(), react)
	out := last(t, f.service)
	if len(f.service.reacted) != 0 || out.ReplyTo != 1 || out.Text != "👍" || !strings.Contains(out.Header, "Stranger") {
		t.Fatalf("reacted %v out %+v", f.service.reacted, out)
	}
}

func TestUnknownCommandInGroupIsAnsweredOnce(t *testing.T) {
	f := newFixture(t)
	if p := f.b.HandleTelegram(t.Context(), f.alice, Incoming{Chat: groupChat, ID: 1, From: owner, Command: "foo"}); p != "" {
		t.Fatalf("person bot answered: %s", p)
	}
	if p := f.b.HandleTelegram(t.Context(), f.service, Incoming{Chat: groupChat, ID: 1, From: owner, Command: "foo"}); p == "" {
		t.Fatal("service bot must answer")
	}
}

func TestPollsVotesContactsAndLocations(t *testing.T) {
	f := newFixture(t)
	g, _ := types.ParseJID(groupJID)
	poll := waText("P1", "", g, pn(bobPhone), false)
	poll.Message = &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{
		Name: proto.String("Dinner?"), SelectableOptionsCount: proto.Uint32(1),
		Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String("Fri")}, {OptionName: proto.String("Sat")}},
	}}
	f.b.HandleWhatsApp(t.Context(), poll)
	if out := last(t, f.bob); out.Kind != KindPoll || out.Poll.Question != "Dinner?" || out.Poll.Multi || len(out.Poll.Options) != 2 {
		t.Fatalf("poll %+v", out)
	}

	// A WhatsApp vote becomes a reply naming the choice, from the voter's bot.
	f.wa.voteChoice = []string{"Sat"}
	vote := waText("V1", "", g, pn(alicePhone), false)
	vote.Message = &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{
		PollCreationMessageKey: &waCommon.MessageKey{ID: proto.String("P1")},
	}}
	f.b.HandleWhatsApp(t.Context(), vote)
	if out := last(t, f.alice); out.Text != "🗳 Sat" || out.ReplyTo != 1 {
		t.Fatalf("vote note %+v", out)
	}

	// The owner's vote in Telegram reaches WhatsApp with the option names.
	f.b.HandlePollAnswer(t.Context(), PollAnswer{PollID: "poll-1", From: owner, Options: []int{0}})
	if f.wa.voteFor != "P1" || len(f.wa.voteOpts) != 1 || f.wa.voteOpts[0] != "Fri" {
		t.Fatalf("vote %q %v", f.wa.voteFor, f.wa.voteOpts)
	}

	a := pn(alicePhone)
	contact := waText("C1", "", a, a, false)
	contact.Message = &waE2E.Message{ContactMessage: &waE2E.ContactMessage{
		DisplayName: proto.String("Uncle"), Vcard: proto.String("BEGIN:VCARD\nTEL;type=CELL;waid=420111222333:+420 111 222 333\nEND:VCARD"),
	}}
	f.b.HandleWhatsApp(t.Context(), contact)
	if out := last(t, f.alice); out.Kind != KindContact || out.Contacts[0].Phone != "420111222333" {
		t.Fatalf("contact %+v", out)
	}

	f.b.HandleTelegram(t.Context(), f.alice, Incoming{
		Chat: owner, Private: true, ID: 90, From: owner,
		Location: &Location{Lat: 50.1, Lon: 14.4, Title: "Cafe"},
	})
	if l := f.wa.sent[len(f.wa.sent)-1].msg.GetLocationMessage(); l.GetName() != "Cafe" || l.GetDegreesLatitude() != 50.1 {
		t.Fatalf("location %v", l)
	}
	f.b.HandleTelegram(t.Context(), f.alice, Incoming{
		Chat: owner, Private: true, ID: 91, From: owner,
		Poll: &Poll{Question: "Q", Options: []string{"a", "b"}, Multi: true},
	})
	if pc := f.wa.sent[len(f.wa.sent)-1].msg.GetPollCreationMessage(); pc.GetSelectableOptionsCount() != 0 || len(pc.GetOptions()) != 2 {
		t.Fatalf("poll %v", pc)
	}
}

func TestLiveLocationUpdatesAreShownOnce(t *testing.T) {
	f := newFixture(t)
	a := pn(alicePhone)
	for i, ts := range []time.Time{time.Unix(1000, 0), time.Unix(1060, 0)} {
		m := waText("L"+strconv.Itoa(i), "", a, a, false)
		m.Info.Timestamp = ts
		m.Message = &waE2E.Message{LiveLocationMessage: &waE2E.LiveLocationMessage{
			DegreesLatitude: proto.Float64(1), DegreesLongitude: proto.Float64(2), SequenceNumber: proto.Int64(int64(i)),
		}}
		f.b.HandleWhatsApp(t.Context(), m)
	}
	if len(f.alice.out) != 1 || f.alice.out[0].Kind != KindLocation {
		t.Fatalf("out %+v", f.alice.out)
	}
}

func chatPresence(chat, sender types.JID, state types.ChatPresence, media types.ChatPresenceMedia) *events.ChatPresence {
	return &events.ChatPresence{
		MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: chat.Server == types.GroupServer},
		State:         state, Media: media,
	}
}

func waitTyped(t *testing.T, b *fakeBot, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, got := range b.typedCopy() {
			if got == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("typed %v, want %q", b.typedCopy(), want)
}

func TestTypingIsShownThroughTheBotThatWouldMirror(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	group := types.NewJID("123-456", types.GroupServer)

	f.b.HandleChatPresence(ctx, chatPresence(pn(alicePhone), pn(alicePhone), types.ChatPresenceComposing, ""))
	waitTyped(t, f.alice, "1000/0/typing")
	f.b.HandleChatPresence(ctx, chatPresence(group, pn(bobPhone), types.ChatPresenceComposing, types.ChatPresenceMediaAudio))
	waitTyped(t, f.bob, "-1001111111111/0/voice")
	f.b.HandleChatPresence(ctx, chatPresence(group, pn(strangerPN), types.ChatPresenceComposing, ""))
	waitTyped(t, f.service, "-1001111111111/0/typing")

	// A message from the typist ends their typing; "paused" does too.
	f.b.HandleWhatsApp(ctx, waText("T1", "hi", pn(alicePhone), pn(alicePhone), false))
	f.b.HandleChatPresence(ctx, chatPresence(group, pn(bobPhone), types.ChatPresencePaused, ""))
	f.b.typingMu.Lock()
	left := len(f.b.typing)
	f.b.typingMu.Unlock()
	if left != 1 {
		t.Fatalf("%d typers left, want only the stranger's", left)
	}

	// A chat without a forum topic gets none created just for typing.
	f.b.HandleChatPresence(ctx, chatPresence(pn("444"), pn("444"), types.ChatPresenceComposing, ""))
	if len(f.service.topics) != 0 {
		t.Fatalf("topics %v created for typing", f.service.topics)
	}
}

func TestOwnerActionKeepsThemOnlineAndConnectRestoresPresence(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.b.Connected(ctx)
	in := Incoming{Chat: owner, Private: true, ID: 7, From: owner, Kind: KindText, Text: "hello"}
	for range 2 {
		if p := f.b.HandleTelegram(ctx, f.alice, in); p != "" {
			t.Fatal(p)
		}
	}
	f.b.Connected(ctx)
	want := []types.Presence{types.PresenceUnavailable, types.PresenceAvailable, types.PresenceAvailable}
	if fmt.Sprint(f.wa.presence) != fmt.Sprint(want) {
		t.Fatalf("presence %v, want %v", f.wa.presence, want)
	}
	if len(f.wa.subscribed) != 2 || f.wa.subscribed[0] != pn(alicePhone) {
		t.Fatalf("subscribed %v, want Alice first, then Bob once", f.wa.subscribed)
	}

	// Once the window has passed, the owner goes offline.
	f.b.online.mu.Lock()
	f.b.online.until = time.Now().Add(-time.Second)
	f.b.online.mu.Unlock()
	f.b.goOffline()
	if got := f.wa.presence[len(f.wa.presence)-1]; got != types.PresenceUnavailable {
		t.Fatalf("last presence %v, want unavailable", got)
	}
}
