// This file is not part of upstream mautrix-go. It belongs to the preserve
// feature maintained in the fork Kharakan/mautrix-go (branch "preserve").
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgev2

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// PreserveMarkerKey is a top-level field in events that mark a message as deleted on the remote network.
// Current clients ignore it. It lets future tooling tell a deletion marker apart from a normal edit.
const PreserveMarkerKey = "im.sattler.preserve"

// PreserveMarkerContent is the value of the PreserveMarkerKey field.
type PreserveMarkerContent struct {
	// Deleted is always true.
	Deleted bool `json:"deleted"`
	// Timestamp is the time of the deletion in milliseconds since the Unix epoch.
	Timestamp int64 `json:"ts,omitempty"`
}

type preserveAction int

const (
	// preserveActionUnsupported means this kind of event can't be preserved (yet).
	preserveActionUnsupported preserveAction = iota
	// preserveActionEditText means the text of the message is replaced with the marker.
	// The original text stays available in the edit history.
	preserveActionEditText
)

// errPreserveUnsupported is returned for message parts that can't be preserved (yet).
// Those parts are redacted like upstream does.
var errPreserveUnsupported = errors.New("message part can't be preserved")

// classifyForPreserve decides how a message part that was deleted on the remote network is preserved.
// It expects the original event as returned by MatrixAPI.GetEvent (decrypted and with parsed content).
func classifyForPreserve(evt *event.Event) preserveAction {
	if evt == nil || evt.Type != event.EventMessage {
		// Stickers, polls and anything else that isn't m.room.message can't be edited into a text message.
		return preserveActionUnsupported
	}
	content, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok || content == nil {
		return preserveActionUnsupported
	}
	switch content.MsgType {
	case event.MsgText, event.MsgNotice, event.MsgEmote:
		return preserveActionEditText
	default:
		// Media and location messages, and events whose content is gone (already redacted).
		return preserveActionUnsupported
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
	markerContent := PreserveMarkerContent{Deleted: true}
	if !ts.IsZero() {
		markerContent.Timestamp = ts.UnixMilli()
	}
	return &event.Content{
		Parsed: content,
		Raw: map[string]any{
			PreserveMarkerKey: markerContent,
		},
	}
}

// preserveMessageParts marks the given message parts as deleted instead of redacting them.
//
// Parts that can't be preserved (yet) are redacted with redactIntent like upstream does.
// If marking a part fails for another reason, nothing is redacted and the result is a failure.
// The message then stays untouched on Matrix, which is the safer outcome for this feature.
func (portal *Portal) preserveMessageParts(
	ctx context.Context,
	parts []*database.Message,
	redactIntent MatrixAPI,
	ts time.Time,
	dontRenderPlaceholder bool,
) EventHandlingResult {
	log := zerolog.Ctx(ctx)
	marker := portal.Bridge.Config.PreserveDeletes.GetMarker()
	var errorList []error
	var unsupported []*database.Message
	for _, part := range parts {
		if part.HasFakeMXID() {
			continue
		}
		err := portal.preserveMessagePart(ctx, part, marker, ts)
		if errors.Is(err, errPreserveUnsupported) {
			log.Debug().Err(err).
				Stringer("part_mxid", part.MXID).
				Str("part_id", string(part.ID)).
				Msg("Message part can't be preserved, redacting it instead")
			unsupported = append(unsupported, part)
		} else if err != nil {
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
	if len(unsupported) > 0 {
		return portal.redactMessageParts(ctx, unsupported, redactIntent, ts, "", dontRenderPlaceholder)
	}
	return EventHandlingResultSuccess
}

func (portal *Portal) preserveMessagePart(ctx context.Context, part *database.Message, marker string, ts time.Time) error {
	log := zerolog.Ctx(ctx)
	evt, err := portal.Bridge.Bot.GetEvent(ctx, portal.MXID, part.MXID)
	if err != nil {
		return fmt.Errorf("failed to get original event: %w", err)
	}
	if classifyForPreserve(evt) != preserveActionEditText {
		return fmt.Errorf("%w: unsupported event type %s", errPreserveUnsupported, evt.Type.Type)
	}
	// An edit is only valid if it comes from the sender of the original event.
	intent, err := portal.getIntentForMXID(ctx, part.SenderMXID)
	if err != nil {
		return fmt.Errorf("failed to get intent of original sender: %w", err)
	} else if intent == nil {
		return fmt.Errorf("%w: no intent for original sender %s", errPreserveUnsupported, part.SenderMXID)
	}
	resp, err := intent.SendMessage(ctx, portal.MXID, event.EventMessage, makePreserveEdit(part.MXID, marker, ts), &MatrixSendExtra{
		Timestamp:   ts,
		MessageMeta: part,
	})
	if err != nil {
		return fmt.Errorf("failed to send deletion marker: %w", err)
	}
	log.Debug().
		Stringer("marker_event_id", resp.EventID).
		Stringer("marked_event_id", part.MXID).
		Str("part_id", string(part.ID)).
		Msg("Marked message part as deleted on Matrix")
	return nil
}
