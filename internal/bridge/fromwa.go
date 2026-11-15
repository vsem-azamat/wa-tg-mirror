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
	skip     bool
	location *Location
	contacts []Contact
	poll     *Poll
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
		return waContent{kind: KindLocation, quoted: l.GetContextInfo().GetStanzaID(), location: &Location{
			Lat: l.GetDegreesLatitude(), Lon: l.GetDegreesLongitude(), Title: l.GetName(), Address: l.GetAddress(),
		}}
	case m.LiveLocationMessage != nil:
		l := m.GetLiveLocationMessage()
		return waContent{kind: KindLocation, text: l.GetCaption(), quoted: l.GetContextInfo().GetStanzaID(), location: &Location{
			Lat: l.GetDegreesLatitude(), Lon: l.GetDegreesLongitude(), Live: true,
		}}
	case m.ContactMessage != nil:
		c := m.GetContactMessage()
		return waContent{kind: KindContact, quoted: c.GetContextInfo().GetStanzaID(), contacts: []Contact{contactOf(c)}}
	case m.ContactsArrayMessage != nil:
		a := m.GetContactsArrayMessage()
		out := waContent{kind: KindContact, quoted: a.GetContextInfo().GetStanzaID()}
		for _, c := range a.GetContacts() {
			out.contacts = append(out.contacts, contactOf(c))
		}
		return out
	case m.PollCreationMessageV4 != nil:
		return describeWA(m.GetPollCreationMessageV4().GetMessage())
	case m.PollCreationMessage != nil, m.PollCreationMessageV2 != nil, m.PollCreationMessageV3 != nil,
		m.PollCreationMessageV5 != nil, m.PollCreationMessageV6 != nil:
		p := cmp.Or(m.GetPollCreationMessage(), m.GetPollCreationMessageV2(), m.GetPollCreationMessageV3(),
			m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6())
		poll := &Poll{Question: p.GetName(), Multi: p.GetSelectableOptionsCount() != 1}
		for _, o := range p.GetOptions() {
			poll.Options = append(poll.Options, o.GetOptionName())
		}
		return waContent{kind: KindPoll, quoted: p.GetContextInfo().GetStanzaID(), poll: poll}
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

// contactOf reads a WhatsApp contact card; the phone number comes from the vCard.
func contactOf(c *waE2E.ContactMessage) Contact {
	return Contact{Name: c.GetDisplayName(), Phone: vcardPhone(c.GetVcard()), VCard: c.GetVcard()}
}

// vcardPhone returns the digits of the first TEL entry of a vCard, preferring the
// WhatsApp ID that WhatsApp puts in it.
func vcardPhone(vcard string) string {
	for _, line := range strings.Split(vcard, "\n") {
		line = strings.TrimSpace(line)
		head, value, ok := strings.Cut(line, ":")
		if !ok || !strings.HasPrefix(strings.ToUpper(head), "TEL") && !strings.Contains(strings.ToUpper(head), ".TEL") {
			continue
		}
		if _, waid, ok := strings.Cut(head, "waid="); ok {
			value = waid
		}
		var digits strings.Builder
		for _, r := range value {
			if r >= '0' && r <= '9' {
				digits.WriteRune(r)
			}
		}
		if digits.Len() > 0 {
			return digits.String()
		}
	}
	return ""
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
	case KindLocation:
		return "location"
	case KindContact:
		return "contact"
	case KindPoll:
		return "poll"
	default:
		return "message"
	}
}
