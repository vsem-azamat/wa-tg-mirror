package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type fakeParser struct{}

func (fakeParser) ParseWebMessage(chat types.JID, m *waWeb.WebMessageInfo) (*events.Message, error) {
	return &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat},
		ID:            m.GetKey().GetID(), Timestamp: time.Unix(int64(m.GetMessageTimestamp()), 0),
	}}, nil
}

func webMsg(id string, ts time.Time) *waHistorySync.HistorySyncMsg {
	return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
		Key: &waCommon.MessageKey{ID: proto.String(id)}, MessageTimestamp: proto.Uint64(uint64(ts.Unix())),
	}}
}

func TestHistoryBufferKeepsRecentMessagesInOrder(t *testing.T) {
	now := time.Now()
	h := newHistoryBuffer(fakeParser{}, now.AddDate(0, 0, -30))
	h.handle(&events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{{
		ID: proto.String("111@s.whatsapp.net"),
		Messages: []*waHistorySync.HistorySyncMsg{
			webMsg("new", now.Add(-time.Hour)), webMsg("ancient", now.AddDate(0, -3, 0)), webMsg("mid", now.AddDate(0, 0, -3)),
		},
	}}}})
	// A live message that arrives during the sync joins the same timeline; a repeat is kept once.
	h.handle(&events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: types.NewJID("111", types.DefaultUserServer)}, ID: "live", Timestamp: now,
	}})
	h.handle(&events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: types.NewJID("111", types.DefaultUserServer)}, ID: "live", Timestamp: now,
	}})

	var ids []string
	for _, m := range h.take() {
		ids = append(ids, m.Info.ID)
	}
	if len(ids) != 3 || ids[0] != "mid" || ids[1] != "new" || ids[2] != "live" {
		t.Fatalf("ids %v", ids)
	}
	// A message handed out once is not handed out again, while a new one is.
	h.handle(&events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: types.NewJID("111", types.DefaultUserServer)}, ID: "live", Timestamp: now,
	}})
	h.handle(&events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: types.NewJID("111", types.DefaultUserServer)}, ID: "later", Timestamp: now,
	}})
	if again := h.take(); len(again) != 1 || again[0].Info.ID != "later" {
		t.Fatalf("second take %v", again)
	}
}
