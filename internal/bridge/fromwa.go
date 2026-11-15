package bridge

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// maxBotUpload is the largest file a bot may send through the public Bot API.
const maxBotUpload = 50 << 20

// waContent is a WhatsApp message turned into the shape of a Telegram message.
type waContent struct {
	kind  Kind
	text  string
	media whatsmeow.DownloadableMessage
	// size is the file size WhatsApp announces, checked before downloading.
	size     uint64
	fileName string
	mimeType string
	// quoted is the ID of the WhatsApp message this one replies to.
	quoted string
	// skip marks messages that carry nothing to show, such as protocol messages.
	skip bool
}

// describeWA reads the content of a WhatsApp message without downloading media.
func describeWA(m *waE2E.Message) waContent {
	switch {
	case m == nil:
		return waContent{skip: true}
	case m.Conversation != nil:
		return waContent{kind: KindText, text: m.GetConversation()}
	case m.ExtendedTextMessage != nil:
		e := m.GetExtendedTextMessage()
		return waContent{kind: KindText, text: e.GetText(), quoted: e.GetContextInfo().GetStanzaID()}
	case m.ImageMessage != nil:
		i := m.GetImageMessage()
		return waContent{kind: KindPhoto, text: i.GetCaption(), media: i, size: i.GetFileLength(), mimeType: i.GetMimetype(), quoted: i.GetContextInfo().GetStanzaID()}
	case m.VideoMessage != nil:
		v := m.GetVideoMessage()
		kind := KindVideo
		if v.GetGifPlayback() {
			kind = KindAnimation
		}
		return waContent{kind: kind, text: v.GetCaption(), media: v, size: v.GetFileLength(), mimeType: v.GetMimetype(), quoted: v.GetContextInfo().GetStanzaID()}
	case m.PtvMessage != nil:
		v := m.GetPtvMessage()
		return waContent{kind: KindVideo, media: v, size: v.GetFileLength(), mimeType: v.GetMimetype(), quoted: v.GetContextInfo().GetStanzaID()}
	case m.AudioMessage != nil:
		a := m.GetAudioMessage()
		kind := KindAudio
		if a.GetPTT() {
			kind = KindVoice
		}
		return waContent{kind: kind, media: a, size: a.GetFileLength(), mimeType: a.GetMimetype(), quoted: a.GetContextInfo().GetStanzaID()}
	case m.DocumentMessage != nil:
		d := m.GetDocumentMessage()
		return waContent{
			kind: KindDocument, text: d.GetCaption(), media: d, size: d.GetFileLength(), mimeType: d.GetMimetype(),
			fileName: cmp.Or(d.GetFileName(), d.GetTitle(), "file"), quoted: d.GetContextInfo().GetStanzaID(),
		}
	case m.DocumentWithCaptionMessage != nil:
		return describeWA(m.GetDocumentWithCaptionMessage().GetMessage())
	case m.StickerMessage != nil:
		s := m.GetStickerMessage()
		if s.GetIsAnimated() {
			return waContent{kind: KindDocument, media: s, mimeType: "image/webp", fileName: "sticker.webp", quoted: s.GetContextInfo().GetStanzaID()}
		}
		return waContent{kind: KindSticker, media: s, mimeType: "image/webp", fileName: "sticker.webp", quoted: s.GetContextInfo().GetStanzaID()}
	case m.LocationMessage != nil:
		l := m.GetLocationMessage()
		return waContent{kind: KindText, text: fmt.Sprintf("📍 %s\nhttps://maps.google.com/?q=%f,%f",
			cmp.Or(l.GetName(), l.GetAddress()), l.GetDegreesLatitude(), l.GetDegreesLongitude())}
	case m.LiveLocationMessage != nil:
		l := m.GetLiveLocationMessage()
		return waContent{kind: KindText, text: fmt.Sprintf("📍 live location\nhttps://maps.google.com/?q=%f,%f",
			l.GetDegreesLatitude(), l.GetDegreesLongitude())}
	case m.ContactMessage != nil:
		return waContent{kind: KindText, text: "👤 " + m.GetContactMessage().GetDisplayName()}
	case m.PollCreationMessageV4 != nil:
		return describeWA(m.GetPollCreationMessageV4().GetMessage())
	case m.PollCreationMessage != nil, m.PollCreationMessageV2 != nil, m.PollCreationMessageV3 != nil,
		m.PollCreationMessageV5 != nil, m.PollCreationMessageV6 != nil:
		p := cmp.Or(m.GetPollCreationMessage(), m.GetPollCreationMessageV2(), m.GetPollCreationMessageV3(),
			m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6())
		var text strings.Builder
		text.WriteString("📊 " + p.GetName())
		for _, o := range p.GetOptions() {
			text.WriteString("\n• " + o.GetOptionName())
		}
		return waContent{kind: KindText, text: text.String()}
	case m.EventMessage != nil:
		e := m.GetEventMessage()
		return waContent{kind: KindText, text: "📅 " + e.GetName() + "\n" + e.GetDescription()}
	case m.ProtocolMessage != nil, m.ReactionMessage != nil, m.AlbumMessage != nil, m.SenderKeyDistributionMessage != nil,
		m.PollUpdateMessage != nil, m.EncReactionMessage != nil, m.KeepInChatMessage != nil,
		m.PinInChatMessage != nil, m.MessageContextInfo != nil && onlyContextInfo(m):
		return waContent{skip: true}
	default:
		return waContent{kind: KindText, text: "[unsupported WhatsApp message]"}
	}
}

// onlyContextInfo reports whether m carries nothing but its context info.
func onlyContextInfo(m *waE2E.Message) bool {
	return proto.Equal(m, &waE2E.Message{MessageContextInfo: m.GetMessageContextInfo()})
}

// fetchMedia downloads the media of c into out, or turns out into a text note when
// the file cannot be mirrored.
func fetchMedia(ctx context.Context, wa WhatsApp, c waContent, out *Outgoing) error {
	if c.media == nil {
		return nil
	}
	if c.size > maxBotUpload {
		tooLarge(c, c.size, out)
		return nil
	}
	data, err := wa.Download(ctx, c.media)
	if err != nil {
		return fmt.Errorf("download %s: %w", kindName(c.kind), err)
	}
	if len(data) > maxBotUpload {
		tooLarge(c, uint64(len(data)), out)
		return nil
	}
	out.Data = data
	return nil
}

// tooLarge turns out into a note about a file Telegram would refuse.
func tooLarge(c waContent, size uint64, out *Outgoing) {
	out.Kind = KindText
	out.Text = fmt.Sprintf("[%s of %d MB is too large for Telegram; open it in WhatsApp]\n%s",
		kindName(c.kind), size>>20, c.text)
}

func kindName(k Kind) string {
	switch k {
	case KindText:
		return "text"
	case KindPhoto:
		return "photo"
	case KindVideo:
		return "video"
	case KindAnimation:
		return "GIF"
	case KindVoice:
		return "voice message"
	case KindAudio:
		return "audio"
	case KindDocument:
		return "file"
	case KindSticker:
		return "sticker"
	default:
		return "message"
	}
}
