// This file is not part of upstream mautrix-go. It belongs to the preserve
// feature maintained in the fork Kharakan/mautrix-go (branch "preserve").
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package bridgeconfig

// DefaultPreserveMarker is the text that replaces a message that was deleted on the remote network.
// The trailing U+2060 (word joiner) is invisible. It keeps clients from rendering
// the lone emoji in the enlarged "emoji-only message" style.
const DefaultPreserveMarker = "🗑️⁠"

// PreserveDeletesConfig contains the settings for keeping messages
// that were deleted on the remote network readable on Matrix.
type PreserveDeletesConfig struct {
	// Enabled turns remote deletions into edits instead of redactions.
	Enabled bool `yaml:"enabled"`
	// Marker is the new text of a deleted message. Empty means DefaultPreserveMarker.
	Marker string `yaml:"marker"`
}

// GetMarker returns the configured marker or the default if none is set.
func (pdc PreserveDeletesConfig) GetMarker() string {
	if pdc.Marker == "" {
		return DefaultPreserveMarker
	}
	return pdc.Marker
}
