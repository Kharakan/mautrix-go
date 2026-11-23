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
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// PreserveMarkerKey is a top-level field in events that mark a message as deleted on the remote network.
// Current clients ignore it. It lets future tooling tell a deletion marker apart from a normal edit or reply.
const PreserveMarkerKey = "im.sattler.preserve"

// PreserveMarkerContent is the value of the PreserveMarkerKey field.
type PreserveMarkerContent struct {
	// Deleted is always true.
	Deleted bool `json:"deleted"`
	// Timestamp is the time of the deletion in milliseconds since the Unix epoch.
	Timestamp int64 `json:"ts,omitempty"`
}

func makePreserveMarkerContent(ts time.Time) PreserveMarkerContent {
	content := PreserveMarkerContent{Deleted: true}
	if !ts.IsZero() {
		content.Timestamp = ts.UnixMilli()
	}
	return content
}

type preserveAction int

const (
	// preserveActionReply leaves the event untouched and sends the marker as a reply to it.
	// This is used for everything that can't be turned into a marker with an edit (stickers, polls, locations)
	// and whenever the original event can't be read.
	preserveActionReply preserveAction = iota
	// preserveActionEditText replaces the text of the message with the marker.
	// The original text stays available in the edit history.
	preserveActionEditText
	// preserveActionEditCaption keeps the media of the message and replaces its caption with the marker.
	// The edit history of clients doesn't show media, so the media itself has to stay visible.
	preserveActionEditCaption
	// preserveActionSkip does nothing, because there is nothing left to preserve.
	preserveActionSkip
)

// classifyForPreserve decides how a message part that was deleted on the remote network is preserved.
// It expects the original event as returned by MatrixAPI.GetEvent (decrypted and with parsed content).
func classifyForPreserve(evt *event.Event) preserveAction {
	if evt == nil {
		return preserveActionReply
	}
	if evt.Unsigned.RedactedBecause != nil || len(evt.Content.Raw) == 0 {
		// Already redacted on Matrix, the content is gone.
		return preserveActionSkip
	}
	if evt.Type != event.EventMessage {
		// An edit must have the same type as the original event,
		// so stickers and polls can't be edited into a text message.
		return preserveActionReply
	}
	content, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok || content == nil {
		return preserveActionReply
	}
	switch content.MsgType {
	case event.MsgText, event.MsgNotice, event.MsgEmote:
		return preserveActionEditText
	case event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile:
		return preserveActionEditCaption
	default:
		return preserveActionReply
	}
}

// makePreserveEdit builds an edit that replaces the text of the target event with the marker.
func makePreserveEdit(target id.EventID, marker string, ts time.Time) *event.Content {
	content := &event.MessageEventContent{
		MsgType:  event.MsgText,
		Body:     marker,
		Mentions: &event.Mentions{},
	}
	content.SetEdit(target)
	return &event.Content{
		Parsed: content,
		Raw: map[string]any{
			PreserveMarkerKey: makePreserveMarkerContent(ts),
		},
	}
}

// preserveCaptionDropKeys are the fields of a media message that are replaced or removed when
// the marker becomes its caption. All other fields are carried over unchanged, including ones
// this library doesn't know about.
var preserveCaptionDropKeys = []string{
	"body", "filename", "format", "formatted_body", "m.mentions", "m.relates_to", "m.new_content",
}

// makePreserveCaptionEdit builds an edit that keeps the media of the original event and sets the marker as its caption.
func makePreserveCaptionEdit(orig *event.Event, marker string, ts time.Time) (*event.Content, error) {
	origContent, ok := orig.Content.Parsed.(*event.MessageEventContent)
	if !ok || origContent == nil {
		return nil, fmt.Errorf("unexpected content type %T", orig.Content.Parsed)
	}
	// Without a filename field, the body of a media message is the file name and there is no caption.
	fileName := origContent.FileName
	if fileName == "" {
		fileName = origContent.Body
	}
	newContent := *origContent
	newContent.Body = marker
	newContent.FileName = fileName
	newContent.Format = ""
	newContent.FormattedBody = ""
	newContent.Mentions = &event.Mentions{}
	newContent.RelatesTo = nil
	newContent.NewContent = nil
	newContent.SetEdit(orig.ID)

	// The parsed content is applied on top of the raw content when the event is serialized.
	// Putting the original fields into the raw content keeps fields that have no place in the parsed struct.
	topLevel, err := preserveCleanRawCopy(orig)
	if err != nil {
		return nil, err
	}
	inner, err := preserveCleanRawCopy(orig)
	if err != nil {
		return nil, err
	}
	topLevel["m.new_content"] = inner
	topLevel[PreserveMarkerKey] = makePreserveMarkerContent(ts)
	return &event.Content{Parsed: &newContent, Raw: topLevel}, nil
}

func preserveCleanRawCopy(orig *event.Event) (map[string]any, error) {
	data := orig.Content.VeryRaw
	if len(data) == 0 {
		var err error
		data, err = json.Marshal(orig.Content.Raw)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize original content: %w", err)
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse original content: %w", err)
	}
	for _, key := range preserveCaptionDropKeys {
		delete(raw, key)
	}
	return raw, nil
}

// makePreserveReply builds a message that contains only the marker and replies to the target event.
// If the original event is known and is part of a thread, the reply stays in that thread.
func makePreserveReply(target id.EventID, orig *event.Event, marker string, ts time.Time) *event.Content {
	relatesTo := (&event.RelatesTo{}).SetReplyTo(target)
	if orig != nil {
		if relatable, ok := orig.Content.Parsed.(event.Relatable); ok {
			if threadRoot := relatable.OptionalGetRelatesTo().GetThreadParent(); threadRoot != "" {
				relatesTo = (&event.RelatesTo{}).SetThread(threadRoot, target)
			}
		}
	}
	return &event.Content{
		Parsed: &event.MessageEventContent{
			MsgType:   event.MsgText,
			Body:      marker,
			Mentions:  &event.Mentions{},
			RelatesTo: relatesTo,
		},
		Raw: map[string]any{
			PreserveMarkerKey: makePreserveMarkerContent(ts),
		},
	}
}

// preserveMessageParts marks the given message parts as deleted instead of redacting them.
//
// Nothing is ever redacted here. If a part can't be marked at all, the result is a failure and
// the message stays untouched on Matrix, which is the safer outcome for this feature.
func (portal *Portal) preserveMessageParts(ctx context.Context, parts []*database.Message, ts time.Time) EventHandlingResult {
	log := zerolog.Ctx(ctx)
	marker := portal.Bridge.Config.PreserveDeletes.GetMarker()
	var errorList []error
	for _, part := range parts {
		if part.HasFakeMXID() {
			continue
		}
		err := portal.preserveMessagePart(ctx, part, marker, ts)
		if err != nil {
			log.Err(err).
				Stringer("part_mxid", part.MXID).
				Str("part_id", string(part.ID)).
				Msg("Failed to mark message part as deleted")
			errorList = append(errorList, err)
		}
	}
	if len(errorList) > 0 {
		return EventHandlingResultFailed.WithError(errors.Join(errorList...))
	}
	return EventHandlingResultSuccess
}

func (portal *Portal) preserveMessagePart(ctx context.Context, part *database.Message, marker string, ts time.Time) error {
	log := zerolog.Ctx(ctx).With().
		Stringer("part_mxid", part.MXID).
		Str("part_id", string(part.ID)).
		Logger()
	sendExtra := &MatrixSendExtra{Timestamp: ts, MessageMeta: part}

	orig, err := portal.Bridge.Bot.GetEvent(ctx, portal.MXID, part.MXID)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get original event of deleted message part, sending the marker as a reply")
		orig = nil
	}
	action := classifyForPreserve(orig)
	if action == preserveActionSkip {
		log.Debug().Msg("Deleted message part is already redacted on Matrix, nothing to preserve")
		return nil
	}

	// An edit is only valid if it comes from the sender of the original event.
	senderIntent, err := portal.getIntentForMXID(ctx, part.SenderMXID)
	if err != nil {
		return fmt.Errorf("failed to get intent of original sender: %w", err)
	}

	if senderIntent != nil && (action == preserveActionEditText || action == preserveActionEditCaption) {
		var edit *event.Content
		if action == preserveActionEditText {
			edit = makePreserveEdit(part.MXID, marker, ts)
		} else {
			edit, err = makePreserveCaptionEdit(orig, marker, ts)
		}
		if err == nil {
			var resp *mautrix.RespSendEvent
			resp, err = senderIntent.SendMessage(ctx, portal.MXID, event.EventMessage, edit, sendExtra)
			if err == nil {
				log.Debug().
					Stringer("marker_event_id", resp.EventID).
					Bool("caption", action == preserveActionEditCaption).
					Msg("Marked message part as deleted with an edit")
				return nil
			}
		}
		log.Warn().Err(err).Msg("Failed to mark message part as deleted with an edit, sending the marker as a reply")
		// The original sender couldn't edit, so don't try to reply as them either.
		senderIntent = nil
	}

	reply := func(intent MatrixAPI) (id.EventID, error) {
		resp, err := intent.SendMessage(ctx, portal.MXID, event.EventMessage, makePreserveReply(part.MXID, orig, marker, ts), sendExtra)
		if err != nil {
			return "", err
		}
		return resp.EventID, nil
	}
	var markerID id.EventID
	if senderIntent != nil && senderIntent != portal.Bridge.Bot {
		markerID, err = reply(senderIntent)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to send deletion marker reply as original sender, trying as the bridge bot")
		}
	}
	if markerID == "" {
		markerID, err = reply(portal.Bridge.Bot)
		if err != nil {
			return fmt.Errorf("failed to send deletion marker: %w", err)
		}
	}
	log.Debug().Stringer("marker_event_id", markerID).Msg("Marked message part as deleted with a reply")
	return nil
}
