package bridge

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
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
	id     int64
	mu     sync.Mutex
	out    []Outgoing
	topics []string
	nextID int
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
