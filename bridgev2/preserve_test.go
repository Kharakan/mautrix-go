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

const preserveTestTS = 1795465260000

func parsedEvent(t *testing.T, evtID id.EventID, evtType event.Type, contentJSON string) *event.Event {
	t.Helper()
	evt := &event.Event{ID: evtID, Type: evtType}
	require.NoError(t, json.Unmarshal([]byte(contentJSON), &evt.Content))
	require.NoError(t, evt.Content.ParseRaw(evtType))
	return evt
}

func toMap(t *testing.T, content *event.Content) map[string]any {
	t.Helper()
	data, err := json.Marshal(content)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	return got
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
		{"image", event.EventMessage, `{"msgtype":"m.image","body":"cat.png","url":"mxc://example.org/abc"}`, preserveActionEditCaption},
		{"image with caption", event.EventMessage, `{"msgtype":"m.image","body":"look","filename":"cat.png","url":"mxc://example.org/abc"}`, preserveActionEditCaption},
		{"video", event.EventMessage, `{"msgtype":"m.video","body":"clip.mp4","url":"mxc://example.org/abc"}`, preserveActionEditCaption},
		{"audio", event.EventMessage, `{"msgtype":"m.audio","body":"voice.ogg","url":"mxc://example.org/abc"}`, preserveActionEditCaption},
		{"file", event.EventMessage, `{"msgtype":"m.file","body":"doc.pdf","url":"mxc://example.org/abc"}`, preserveActionEditCaption},
		{"location", event.EventMessage, `{"msgtype":"m.location","body":"here","geo_uri":"geo:1,2"}`, preserveActionReply},
		{"unknown msgtype", event.EventMessage, `{"msgtype":"com.example.custom","body":"x"}`, preserveActionReply},
		{"sticker", event.EventSticker, `{"body":"sticker","url":"mxc://example.org/abc"}`, preserveActionReply},
		{"poll", event.Type{Type: "org.matrix.msc3381.poll.start", Class: event.MessageEventType}, `{"org.matrix.msc3381.poll.start":{"question":{"org.matrix.msc1767.text":"?"}}}`, preserveActionReply},
		{"already redacted", event.EventMessage, `{}`, preserveActionSkip},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyForPreserve(parsedEvent(t, "$evt", tt.evtType, tt.content)))
		})
	}
	t.Run("original could not be fetched", func(t *testing.T) {
		assert.Equal(t, preserveActionReply, classifyForPreserve(nil))
	})
	t.Run("redacted_because set", func(t *testing.T) {
		evt := parsedEvent(t, "$evt", event.EventMessage, `{"msgtype":"m.text","body":"hello"}`)
		evt.Unsigned.RedactedBecause = &event.Event{ID: "$redaction"}
		assert.Equal(t, preserveActionSkip, classifyForPreserve(evt))
	})
}

func TestMakePreserveEdit(t *testing.T) {
	target := id.EventID("$original:example.org")
	got := toMap(t, makePreserveEdit(target, bridgeconfig.DefaultPreserveMarker, time.UnixMilli(preserveTestTS)))

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
	assert.Equal(t, map[string]any{"deleted": true, "ts": float64(preserveTestTS)}, got[PreserveMarkerKey])
	// Nothing else is sent, in particular no part of the original text.
	assert.Len(t, got, 6)
}

func TestMakePreserveEditWithoutTimestamp(t *testing.T) {
	got := toMap(t, makePreserveEdit("$original:example.org", "deleted", time.Time{}))
	assert.Equal(t, map[string]any{"deleted": true}, got[PreserveMarkerKey])
	assert.Equal(t, "* deleted", got["body"])
}

func TestMakePreserveCaptionEdit(t *testing.T) {
	ts := time.UnixMilli(preserveTestTS)

	t.Run("image with formatted caption and unknown fields", func(t *testing.T) {
		orig := parsedEvent(t, "$img", event.EventMessage, `{
			"msgtype": "m.image",
			"body": "look at this",
			"format": "org.matrix.custom.html",
			"formatted_body": "look at <b>this</b>",
			"filename": "cat.png",
			"url": "mxc://example.org/abc",
			"info": {"mimetype": "image/png", "w": 320, "h": 200, "size": 1234, "xyz.amorgan.blurhash": "LKO2"},
			"m.mentions": {"user_ids": ["@dom:example.org"]},
			"m.relates_to": {"m.in_reply_to": {"event_id": "$earlier"}},
			"com.example.custom": {"keep": true}
		}`)
		content, err := makePreserveCaptionEdit(orig, "MARK", ts)
		require.NoError(t, err)
		got := toMap(t, content)

		assert.Equal(t, map[string]any{"rel_type": "m.replace", "event_id": "$img"}, got["m.relates_to"])
		assert.Equal(t, map[string]any{"deleted": true, "ts": float64(preserveTestTS)}, got[PreserveMarkerKey])
		newContent := got["m.new_content"].(map[string]any)
		// The media stays exactly as it was.
		assert.Equal(t, "m.image", newContent["msgtype"])
		assert.Equal(t, "mxc://example.org/abc", newContent["url"])
		assert.Equal(t, map[string]any{"mimetype": "image/png", "w": float64(320), "h": float64(200), "size": float64(1234), "xyz.amorgan.blurhash": "LKO2"}, newContent["info"])
		assert.Equal(t, map[string]any{"keep": true}, newContent["com.example.custom"])
		// The caption is replaced with the marker, the file name is kept.
		assert.Equal(t, "MARK", newContent["body"])
		assert.Equal(t, "cat.png", newContent["filename"])
		// No trace of the old caption, its formatting, its mentions or its reply relation in the new content.
		assert.NotContains(t, newContent, "format")
		assert.NotContains(t, newContent, "formatted_body")
		assert.NotContains(t, newContent, "m.relates_to")
		assert.NotContains(t, newContent, "m.new_content")
		assert.Equal(t, map[string]any{}, newContent["m.mentions"])
		data, _ := json.Marshal(content)
		assert.NotContains(t, string(data), "look at")
		assert.NotContains(t, string(data), "@dom:example.org")
		// The fallback content for clients without edit support still shows the media.
		assert.Equal(t, "m.image", got["msgtype"])
		assert.Equal(t, "mxc://example.org/abc", got["url"])
		assert.Equal(t, "MARK", got["body"])
		assert.Equal(t, "cat.png", got["filename"])
	})

	t.Run("image without caption gets one", func(t *testing.T) {
		orig := parsedEvent(t, "$img", event.EventMessage, `{"msgtype":"m.image","body":"cat.png","url":"mxc://example.org/abc"}`)
		content, err := makePreserveCaptionEdit(orig, "MARK", ts)
		require.NoError(t, err)
		newContent := toMap(t, content)["m.new_content"].(map[string]any)
		assert.Equal(t, "MARK", newContent["body"])
		assert.Equal(t, "cat.png", newContent["filename"], "the old body was the file name and must become the filename field")
	})

	t.Run("encrypted voice message keeps its key and voice flags", func(t *testing.T) {
		orig := parsedEvent(t, "$voice", event.EventMessage, `{
			"msgtype": "m.audio",
			"body": "voice.ogg",
			"file": {"url": "mxc://example.org/enc", "key": {"kty": "oct", "alg": "A256CTR", "k": "c2VjcmV0", "key_ops": ["encrypt","decrypt"], "ext": true}, "iv": "aXY", "hashes": {"sha256": "aGFzaA"}, "v": "v2"},
			"info": {"mimetype": "audio/ogg", "duration": 4200},
			"org.matrix.msc3245.voice": {},
			"org.matrix.msc1767.audio": {"duration": 4200, "waveform": [1, 2, 3]}
		}`)
		content, err := makePreserveCaptionEdit(orig, "MARK", ts)
		require.NoError(t, err)
		newContent := toMap(t, content)["m.new_content"].(map[string]any)
		file := newContent["file"].(map[string]any)
		assert.Equal(t, "mxc://example.org/enc", file["url"])
		assert.Equal(t, "c2VjcmV0", file["key"].(map[string]any)["k"])
		assert.Equal(t, "aXY", file["iv"])
		assert.Contains(t, newContent, "org.matrix.msc3245.voice")
		assert.Equal(t, []any{float64(1), float64(2), float64(3)}, newContent["org.matrix.msc1767.audio"].(map[string]any)["waveform"])
		assert.Equal(t, "MARK", newContent["body"])
		assert.Equal(t, "voice.ogg", newContent["filename"])
	})

	t.Run("does not modify the original event", func(t *testing.T) {
		orig := parsedEvent(t, "$img", event.EventMessage, `{"msgtype":"m.image","body":"look","filename":"cat.png","url":"mxc://example.org/abc"}`)
		_, err := makePreserveCaptionEdit(orig, "MARK", ts)
		require.NoError(t, err)
		assert.Equal(t, "look", orig.Content.AsMessage().Body)
		assert.Equal(t, "look", orig.Content.Raw["body"])
		assert.Nil(t, orig.Content.AsMessage().NewContent)
	})
}

func TestMakePreserveReply(t *testing.T) {
	ts := time.UnixMilli(preserveTestTS)

	t.Run("plain reply", func(t *testing.T) {
		orig := parsedEvent(t, "$sticker", event.EventSticker, `{"body":"sticker","url":"mxc://example.org/abc"}`)
		got := toMap(t, makePreserveReply("$sticker", orig, "MARK", ts))
		assert.Equal(t, "m.text", got["msgtype"])
		assert.Equal(t, "MARK", got["body"])
		assert.Equal(t, map[string]any{"m.in_reply_to": map[string]any{"event_id": "$sticker"}}, got["m.relates_to"])
		assert.Equal(t, map[string]any{}, got["m.mentions"])
		assert.Equal(t, map[string]any{"deleted": true, "ts": float64(preserveTestTS)}, got[PreserveMarkerKey])
		assert.Len(t, got, 5)
	})
	t.Run("unknown original", func(t *testing.T) {
		got := toMap(t, makePreserveReply("$unknown", nil, "MARK", ts))
		assert.Equal(t, map[string]any{"m.in_reply_to": map[string]any{"event_id": "$unknown"}}, got["m.relates_to"])
	})
	t.Run("reply stays in the thread of the original", func(t *testing.T) {
		orig := parsedEvent(t, "$inthread", event.EventMessage, `{"msgtype":"m.location","body":"here","geo_uri":"geo:1,2","m.relates_to":{"rel_type":"m.thread","event_id":"$root","is_falling_back":true,"m.in_reply_to":{"event_id":"$prev"}}}`)
		relatesTo := toMap(t, makePreserveReply("$inthread", orig, "MARK", ts))["m.relates_to"].(map[string]any)
		assert.Equal(t, "m.thread", relatesTo["rel_type"])
		assert.Equal(t, "$root", relatesTo["event_id"])
		assert.Equal(t, map[string]any{"event_id": "$inthread"}, relatesTo["m.in_reply_to"])
	})
}

func TestPreserveMarkerDefault(t *testing.T) {
	assert.Equal(t, "🗑️⁠", bridgeconfig.PreserveDeletesConfig{}.GetMarker())
	assert.Equal(t, "🗑️ gelöscht", bridgeconfig.PreserveDeletesConfig{Marker: "🗑️ gelöscht"}.GetMarker())
	// The default must end with the invisible word joiner, otherwise clients enlarge the lone emoji.
	runes := []rune(bridgeconfig.DefaultPreserveMarker)
	assert.Equal(t, '⁠', runes[len(runes)-1])
}

// fakePreserveAPI implements the few MatrixAPI methods used when marking messages.
// Calling any other method panics because the embedded interface is nil.
type fakePreserveAPI struct {
	MatrixAPI
	events    map[id.EventID]*event.Event
	getErr    error
	failEdits bool
	failAll   bool
	sent      []string
}

func (f *fakePreserveAPI) GetMXID() id.UserID { return "@bot:example.org" }

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

// SendMessage records what was sent as "<kind> <target>", e.g. "edit $text".
func (f *fakePreserveAPI) SendMessage(_ context.Context, _ id.RoomID, evtType event.Type, content *event.Content, _ *MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	var kind string
	var target id.EventID
	switch parsed := content.Parsed.(type) {
	case *event.RedactionEventContent:
		kind, target = "redaction", parsed.Redacts
	case *event.MessageEventContent:
		if replaceID := parsed.RelatesTo.GetReplaceID(); replaceID != "" && parsed.NewContent.MsgType == event.MsgText {
			kind, target = "edit", replaceID
		} else if replaceID != "" {
			kind, target = "caption", replaceID
		} else {
			kind, target = "reply", parsed.RelatesTo.GetReplyTo()
		}
	}
	if evtType != event.EventMessage && evtType != event.EventRedaction {
		kind = "unexpected-" + evtType.Type
	}
	if f.failAll || (f.failEdits && (kind == "edit" || kind == "caption")) {
		return nil, errors.New("M_FORBIDDEN")
	}
	f.sent = append(f.sent, fmt.Sprintf("%s %s", kind, target))
	return &mautrix.RespSendEvent{EventID: id.EventID(fmt.Sprintf("$sent%d", len(f.sent)))}, nil
}

func newPreserveTestPortal(t *testing.T, bot *fakePreserveAPI) *Portal {
	t.Helper()
	bot.events = map[id.EventID]*event.Event{
		"$text":     parsedEvent(t, "$text", event.EventMessage, `{"msgtype":"m.text","body":"hello"}`),
		"$image":    parsedEvent(t, "$image", event.EventMessage, `{"msgtype":"m.image","body":"cat.png","url":"mxc://example.org/abc"}`),
		"$sticker":  parsedEvent(t, "$sticker", event.EventSticker, `{"body":"sticker","url":"mxc://example.org/abc"}`),
		"$redacted": parsedEvent(t, "$redacted", event.EventMessage, `{}`),
	}
	return &Portal{
		Portal: &database.Portal{MXID: "!room:example.org"},
		Bridge: &Bridge{
			Bot:    bot,
			Config: &bridgeconfig.BridgeConfig{PreserveDeletes: bridgeconfig.PreserveDeletesConfig{Enabled: true}},
		},
	}
}

// botPart returns a message part that was sent by the bridge bot,
// so the tests don't need a database to look up ghosts and users.
func botPart(mxid id.EventID) *database.Message {
	return &database.Message{ID: networkid.MessageID(mxid), MXID: mxid, SenderMXID: "@bot:example.org"}
}

func TestPreserveMessageParts(t *testing.T) {
	ctx := context.Background()
	ts := time.UnixMilli(preserveTestTS)
	run := func(t *testing.T, bot *fakePreserveAPI, parts ...*database.Message) EventHandlingResult {
		t.Helper()
		return newPreserveTestPortal(t, bot).preserveMessageParts(ctx, parts, ts)
	}

	t.Run("text is edited", func(t *testing.T) {
		bot := &fakePreserveAPI{}
		assert.True(t, run(t, bot, botPart("$text")).Success)
		assert.Equal(t, []string{"edit $text"}, bot.sent)
	})
	t.Run("image keeps its media and gets the marker as caption", func(t *testing.T) {
		bot := &fakePreserveAPI{}
		assert.True(t, run(t, bot, botPart("$image")).Success)
		assert.Equal(t, []string{"caption $image"}, bot.sent)
	})
	t.Run("sticker stays and gets a reply", func(t *testing.T) {
		bot := &fakePreserveAPI{}
		assert.True(t, run(t, bot, botPart("$sticker")).Success)
		assert.Equal(t, []string{"reply $sticker"}, bot.sent)
	})
	t.Run("message with several parts", func(t *testing.T) {
		bot := &fakePreserveAPI{}
		assert.True(t, run(t, bot, botPart("$image"), botPart("$text"), botPart("$sticker")).Success)
		assert.Equal(t, []string{"caption $image", "edit $text", "reply $sticker"}, bot.sent)
	})
	t.Run("unknown original sender gets a reply from the bot", func(t *testing.T) {
		bot := &fakePreserveAPI{}
		part := botPart("$text")
		part.SenderMXID = ""
		assert.True(t, run(t, bot, part).Success)
		assert.Equal(t, []string{"reply $text"}, bot.sent)
	})
	t.Run("original can't be fetched or decrypted", func(t *testing.T) {
		bot := &fakePreserveAPI{getErr: errors.New("can't decrypt the event")}
		assert.True(t, run(t, bot, botPart("$text")).Success)
		assert.Equal(t, []string{"reply $text"}, bot.sent)
	})
	t.Run("edit is rejected, marker comes as reply", func(t *testing.T) {
		bot := &fakePreserveAPI{failEdits: true}
		assert.True(t, run(t, bot, botPart("$text"), botPart("$image")).Success)
		assert.Equal(t, []string{"reply $text", "reply $image"}, bot.sent)
	})
	t.Run("nothing can be sent, message stays untouched", func(t *testing.T) {
		bot := &fakePreserveAPI{failAll: true}
		res := run(t, bot, botPart("$text"), botPart("$sticker"))
		assert.False(t, res.Success)
		assert.Error(t, res.Error)
		assert.Empty(t, bot.sent)
	})
	t.Run("already redacted on Matrix", func(t *testing.T) {
		bot := &fakePreserveAPI{}
		assert.True(t, run(t, bot, botPart("$redacted")).Success)
		assert.Empty(t, bot.sent)
	})
	t.Run("parts with fake event IDs are skipped", func(t *testing.T) {
		bot := &fakePreserveAPI{}
		fake := botPart("$text")
		fake.SetFakeMXID()
		assert.True(t, run(t, bot, fake).Success)
		assert.Empty(t, bot.sent)
	})
	t.Run("never redacts", func(t *testing.T) {
		for _, bot := range []*fakePreserveAPI{{}, {failEdits: true}, {getErr: errors.New("gone")}} {
			run(t, bot, botPart("$text"), botPart("$image"), botPart("$sticker"), botPart("$redacted"))
			for _, sent := range bot.sent {
				assert.NotContains(t, sent, "redaction")
				assert.NotContains(t, sent, "unexpected")
			}
		}
	})
}
