// This file is not part of upstream mautrix-go. It belongs to the preserve
// feature maintained in the fork Kharakan/mautrix-go (branch "preserve").
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func parsedEvent(t *testing.T, evtType event.Type, contentJSON string) *event.Event {
	t.Helper()
	evt := &event.Event{Type: evtType}
	require.NoError(t, json.Unmarshal([]byte(contentJSON), &evt.Content))
	require.NoError(t, evt.Content.ParseRaw(evtType))
	return evt
}

func TestClassifyForPreserve(t *testing.T) {
	tests := []struct {
		name    string
		evtType event.Type
		content string
		want    preserveAction
	}{
		{"text", event.EventMessage, `{"msgtype":"m.text","body":"hello"}`, preserveActionEditText},
		{"formatted text", event.EventMessage, `{"msgtype":"m.text","body":"hello","format":"org.matrix.custom.html","formatted_body":"<b>hello</b>"}`, preserveActionEditText},
		{"notice", event.EventMessage, `{"msgtype":"m.notice","body":"hello"}`, preserveActionEditText},
		{"emote", event.EventMessage, `{"msgtype":"m.emote","body":"waves"}`, preserveActionEditText},
		{"image", event.EventMessage, `{"msgtype":"m.image","body":"cat.png","url":"mxc://example.org/abc"}`, preserveActionUnsupported},
		{"image with caption", event.EventMessage, `{"msgtype":"m.image","body":"look","filename":"cat.png","url":"mxc://example.org/abc"}`, preserveActionUnsupported},
		{"video", event.EventMessage, `{"msgtype":"m.video","body":"clip.mp4","url":"mxc://example.org/abc"}`, preserveActionUnsupported},
		{"audio", event.EventMessage, `{"msgtype":"m.audio","body":"voice.ogg","url":"mxc://example.org/abc"}`, preserveActionUnsupported},
		{"file", event.EventMessage, `{"msgtype":"m.file","body":"doc.pdf","url":"mxc://example.org/abc"}`, preserveActionUnsupported},
		{"location", event.EventMessage, `{"msgtype":"m.location","body":"here","geo_uri":"geo:1,2"}`, preserveActionUnsupported},
		{"already redacted", event.EventMessage, `{}`, preserveActionUnsupported},
		{"sticker", event.EventSticker, `{"body":"sticker","url":"mxc://example.org/abc"}`, preserveActionUnsupported},
		{"reaction", event.EventReaction, `{"m.relates_to":{"rel_type":"m.annotation","event_id":"$abc","key":"👍"}}`, preserveActionUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyForPreserve(parsedEvent(t, tt.evtType, tt.content)))
		})
	}
	t.Run("nil event", func(t *testing.T) {
		assert.Equal(t, preserveActionUnsupported, classifyForPreserve(nil))
	})
	t.Run("unparsed content", func(t *testing.T) {
		assert.Equal(t, preserveActionUnsupported, classifyForPreserve(&event.Event{Type: event.EventMessage}))
	})
}

func TestMakePreserveEdit(t *testing.T) {
	target := id.EventID("$original:example.org")
	ts := time.UnixMilli(1795465260000)
	content := makePreserveEdit(target, bridgeconfig.DefaultPreserveMarker, ts)

	data, err := json.Marshal(content)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))

	// The edit points at the original event.
	assert.Equal(t, map[string]any{"rel_type": "m.replace", "event_id": string(target)}, got["m.relates_to"])
	// The new content is only the marker, as plain text without mentions.
	assert.Equal(t, map[string]any{
		"msgtype":    "m.text",
		"body":       bridgeconfig.DefaultPreserveMarker,
		"m.mentions": map[string]any{},
	}, got["m.new_content"])
	// The fallback for clients without edit support carries the marker too.
	assert.Equal(t, "m.text", got["msgtype"])
	assert.Equal(t, "* "+bridgeconfig.DefaultPreserveMarker, got["body"])
	assert.Equal(t, map[string]any{}, got["m.mentions"])
	// The machine-readable marker is a top-level field.
	assert.Equal(t, map[string]any{"deleted": true, "ts": float64(1795465260000)}, got[PreserveMarkerKey])
	// Nothing else is sent, in particular no part of the original text.
	assert.Len(t, got, 6)
}

func TestMakePreserveEditWithoutTimestamp(t *testing.T) {
	content := makePreserveEdit("$original:example.org", "deleted", time.Time{})
	data, err := json.Marshal(content)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, map[string]any{"deleted": true}, got[PreserveMarkerKey])
	assert.Equal(t, "* deleted", got["body"])
}

func TestPreserveMarkerDefault(t *testing.T) {
	assert.Equal(t, "🗑️⁠", bridgeconfig.PreserveDeletesConfig{}.GetMarker())
	assert.Equal(t, "🗑️ gelöscht", bridgeconfig.PreserveDeletesConfig{Marker: "🗑️ gelöscht"}.GetMarker())
	// The default must end with the invisible word joiner, otherwise clients enlarge the lone emoji.
	assert.Equal(t, '⁠', []rune(bridgeconfig.DefaultPreserveMarker)[len([]rune(bridgeconfig.DefaultPreserveMarker))-1])
}

// fakePreserveAPI implements the few MatrixAPI methods used when marking messages.
// Calling any other method panics because the embedded interface is nil.
type fakePreserveAPI struct {
	MatrixAPI
	mxid     id.UserID
	events   map[id.EventID]*event.Event
	getErr   error
	sendErr  error
	sentType []event.Type
	sentTo   []id.EventID
}

func (f *fakePreserveAPI) GetMXID() id.UserID { return f.mxid }

func (f *fakePreserveAPI) GetEvent(_ context.Context, _ id.RoomID, eventID id.EventID) (*event.Event, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	evt, ok := f.events[eventID]
	if !ok {
		return nil, errors.New("event not found")
	}
	return evt, nil
}

func (f *fakePreserveAPI) SendMessage(_ context.Context, _ id.RoomID, evtType event.Type, content *event.Content, _ *MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	if evtType == event.EventMessage && f.sendErr != nil {
		return nil, f.sendErr
	}
	f.sentType = append(f.sentType, evtType)
	switch parsed := content.Parsed.(type) {
	case *event.MessageEventContent:
		f.sentTo = append(f.sentTo, parsed.RelatesTo.GetReplaceID())
	case *event.RedactionEventContent:
		f.sentTo = append(f.sentTo, parsed.Redacts)
	}
	return &mautrix.RespSendEvent{EventID: id.EventID(fmt.Sprintf("$sent%d", len(f.sentType)))}, nil
}

func newPreserveTestPortal(t *testing.T, bot *fakePreserveAPI) *Portal {
	t.Helper()
	bot.mxid = "@bot:example.org"
	bot.events = map[id.EventID]*event.Event{
		"$text":    parsedEvent(t, event.EventMessage, `{"msgtype":"m.text","body":"hello"}`),
		"$image":   parsedEvent(t, event.EventMessage, `{"msgtype":"m.image","body":"cat.png","url":"mxc://example.org/abc"}`),
		"$sticker": parsedEvent(t, event.EventSticker, `{"body":"sticker","url":"mxc://example.org/abc"}`),
	}
	return &Portal{
		Portal: &database.Portal{MXID: "!room:example.org"},
		Bridge: &Bridge{
			Bot:    bot,
			Config: &bridgeconfig.BridgeConfig{PreserveDeletes: bridgeconfig.PreserveDeletesConfig{Enabled: true}},
		},
	}
}

func botPart(mxid id.EventID) *database.Message {
	return &database.Message{ID: networkid.MessageID(mxid), MXID: mxid, SenderMXID: "@bot:example.org"}
}

func TestPreserveMessageParts(t *testing.T) {
	ctx := context.Background()
	ts := time.UnixMilli(1795465260000)

	t.Run("text is edited, not redacted", func(t *testing.T) {
		bot, redactor := &fakePreserveAPI{}, &fakePreserveAPI{}
		portal := newPreserveTestPortal(t, bot)
		res := portal.preserveMessageParts(ctx, []*database.Message{botPart("$text")}, redactor, ts, false)
		assert.True(t, res.Success)
		assert.Equal(t, []event.Type{event.EventMessage}, bot.sentType)
		assert.Equal(t, []id.EventID{"$text"}, bot.sentTo)
		assert.Empty(t, redactor.sentType)
	})
	t.Run("image and sticker are redacted like upstream", func(t *testing.T) {
		bot, redactor := &fakePreserveAPI{}, &fakePreserveAPI{}
		portal := newPreserveTestPortal(t, bot)
		res := portal.preserveMessageParts(ctx, []*database.Message{botPart("$image"), botPart("$sticker")}, redactor, ts, false)
		assert.True(t, res.Success)
		assert.Empty(t, bot.sentType)
		assert.Equal(t, []event.Type{event.EventRedaction, event.EventRedaction}, redactor.sentType)
		assert.Equal(t, []id.EventID{"$image", "$sticker"}, redactor.sentTo)
	})
	t.Run("mixed parts", func(t *testing.T) {
		bot, redactor := &fakePreserveAPI{}, &fakePreserveAPI{}
		portal := newPreserveTestPortal(t, bot)
		res := portal.preserveMessageParts(ctx, []*database.Message{botPart("$image"), botPart("$text")}, redactor, ts, false)
		assert.True(t, res.Success)
		assert.Equal(t, []id.EventID{"$text"}, bot.sentTo)
		assert.Equal(t, []id.EventID{"$image"}, redactor.sentTo)
	})
	t.Run("unknown original sender is redacted like upstream", func(t *testing.T) {
		bot, redactor := &fakePreserveAPI{}, &fakePreserveAPI{}
		portal := newPreserveTestPortal(t, bot)
		part := botPart("$text")
		part.SenderMXID = ""
		res := portal.preserveMessageParts(ctx, []*database.Message{part}, redactor, ts, false)
		assert.True(t, res.Success)
		assert.Empty(t, bot.sentType)
		assert.Equal(t, []id.EventID{"$text"}, redactor.sentTo)
	})
	t.Run("fetch failure leaves the message untouched", func(t *testing.T) {
		bot, redactor := &fakePreserveAPI{getErr: errors.New("can't decrypt the event")}, &fakePreserveAPI{}
		portal := newPreserveTestPortal(t, bot)
		res := portal.preserveMessageParts(ctx, []*database.Message{botPart("$text")}, redactor, ts, false)
		assert.False(t, res.Success)
		assert.Error(t, res.Error)
		assert.Empty(t, bot.sentType)
		assert.Empty(t, redactor.sentType)
	})
	t.Run("send failure leaves the message untouched", func(t *testing.T) {
		bot, redactor := &fakePreserveAPI{sendErr: errors.New("M_FORBIDDEN")}, &fakePreserveAPI{}
		portal := newPreserveTestPortal(t, bot)
		res := portal.preserveMessageParts(ctx, []*database.Message{botPart("$text"), botPart("$image")}, redactor, ts, false)
		assert.False(t, res.Success)
		assert.Empty(t, bot.sentType)
		assert.Empty(t, redactor.sentType, "nothing may be redacted when marking another part failed")
	})
	t.Run("parts with fake event IDs are skipped", func(t *testing.T) {
		bot, redactor := &fakePreserveAPI{}, &fakePreserveAPI{}
		portal := newPreserveTestPortal(t, bot)
		fake := botPart("$text")
		fake.SetFakeMXID()
		res := portal.preserveMessageParts(ctx, []*database.Message{fake}, redactor, ts, false)
		assert.True(t, res.Success)
		assert.Empty(t, bot.sentType)
		assert.Empty(t, redactor.sentType)
	})
}
