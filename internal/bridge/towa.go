package bridge

import (
	"cmp"
	"context"
	"fmt"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// quote is the WhatsApp message a Telegram reply points at.
type quote struct {
	id     string
	sender string
	text   string
}

func (q *quote) contextInfo() *waE2E.ContextInfo {
	if q == nil {
		return nil
	}
	return &waE2E.ContextInfo{
		StanzaID:      proto.String(q.id),
		Participant:   proto.String(q.sender),
		QuotedMessage: &waE2E.Message{Conversation: proto.String(q.text)},
	}
}

// buildWA turns a Telegram message into a WhatsApp message, uploading its file.
func buildWA(ctx context.Context, wa WhatsApp, tg Telegram, in Incoming, q *quote) (*waE2E.Message, error) {
	if in.Unsupported != "" {
		return nil, fmt.Errorf("%s cannot be sent to WhatsApp", in.Unsupported)
	}
	ci := q.contextInfo()
	if l := in.Location; l != nil {
		return &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
			DegreesLatitude: proto.Float64(l.Lat), DegreesLongitude: proto.Float64(l.Lon),
			Name: optional(l.Title), Address: optional(l.Address), ContextInfo: ci,
		}}, nil
	}
	if c := in.Contact; c != nil {
		return &waE2E.Message{ContactMessage: &waE2E.ContactMessage{
			DisplayName: proto.String(c.Name), Vcard: proto.String(cmp.Or(c.VCard, vcardOf(*c))), ContextInfo: ci,
		}}, nil
	}
	if p := in.Poll; p != nil {
		selectable := 1
		if p.Multi {
			selectable = 0 // any number
		}
		msg := wa.BuildPollCreation(p.Question, p.Options, selectable)
		if ci != nil {
			if pc := msg.GetPollCreationMessage(); pc != nil {
				pc.ContextInfo = ci
			}
		}
		return msg, nil
	}
	if in.Kind == KindText {
		if ci == nil {
			return &waE2E.Message{Conversation: proto.String(in.Text)}, nil
		}
		return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(in.Text), ContextInfo: ci}}, nil
	}
	if in.File == nil {
		return nil, fmt.Errorf("%s without a file", kindName(in.Kind))
	}
	data, err := tg.Download(ctx, in.File.FileID)
	if err != nil {
		return nil, fmt.Errorf("download from Telegram: %w", err)
	}
	up, err := wa.Upload(ctx, data, mediaType(in.Kind))
	if err != nil {
		return nil, fmt.Errorf("upload to WhatsApp: %w", err)
	}
	caption := optional(in.Text)
	switch in.Kind {
	case KindPhoto:
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: &up.FileLength, Mimetype: proto.String("image/jpeg"),
			Caption: caption, ContextInfo: ci,
		}}, nil
	case KindVideo, KindAnimation:
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: &up.FileLength, Mimetype: proto.String(orDefault(in.File.MimeType, "video/mp4")),
			Seconds: seconds(in.File.Seconds), GifPlayback: proto.Bool(in.Kind == KindAnimation), Caption: caption, ContextInfo: ci,
		}}, nil
	case KindVoice:
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: &up.FileLength, Mimetype: proto.String("audio/ogg; codecs=opus"),
			Seconds: seconds(in.File.Seconds), PTT: proto.Bool(true), ContextInfo: ci,
		}}, nil
	case KindAudio:
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: &up.FileLength, Mimetype: proto.String(orDefault(in.File.MimeType, "audio/mpeg")),
			Seconds: seconds(in.File.Seconds), ContextInfo: ci,
		}}, nil
	case KindSticker:
		return &waE2E.Message{StickerMessage: &waE2E.StickerMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: &up.FileLength, Mimetype: proto.String("image/webp"), ContextInfo: ci,
		}}, nil
	case KindDocument:
		name := orDefault(in.File.FileName, "file")
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
			FileSHA256: up.FileSHA256, FileLength: &up.FileLength, Mimetype: proto.String(orDefault(in.File.MimeType, "application/octet-stream")),
			FileName: proto.String(name), Title: proto.String(name), Caption: caption, ContextInfo: ci,
		}}, nil
	case KindText, KindLocation, KindContact, KindPoll:
	}
	return nil, fmt.Errorf("unexpected message kind %d", in.Kind)
}

// vcardOf builds a minimal vCard for a contact Telegram sent without one.
func vcardOf(c Contact) string {
	tel := ""
	if c.Phone != "" {
		tel = fmt.Sprintf("TEL;type=CELL;waid=%s:+%s\n", c.Phone, c.Phone)
	}
	return "BEGIN:VCARD\nVERSION:3.0\nFN:" + c.Name + "\n" + tel + "END:VCARD"
}

func mediaType(k Kind) whatsmeow.MediaType {
	switch k {
	case KindPhoto, KindSticker:
		return whatsmeow.MediaImage
	case KindVideo, KindAnimation:
		return whatsmeow.MediaVideo
	case KindVoice, KindAudio:
		return whatsmeow.MediaAudio
	case KindText, KindDocument, KindLocation, KindContact, KindPoll:
	}
	return whatsmeow.MediaDocument
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func seconds(s int) *uint32 {
	if s <= 0 {
		return nil
	}
	return proto.Uint32(uint32(s)) //nolint:gosec // durations of Telegram media fit uint32
}
