// This file is not part of upstream mautrix-go. It belongs to the preserve
// feature maintained in the fork Kharakan/mautrix-go (branch "preserve").
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package mxmain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
)

// upgradePreserveTestConfig runs the same config upgrade the bridge runs on startup
// and returns the config as the bridge would load it and as it would be written back.
func upgradePreserveTestConfig(t *testing.T, userConfig string) (bridgeconfig.Config, string) {
	t.Helper()
	var buf strings.Builder
	require.NoError(t, matrixExampleConfigBaseTemplate.Execute(&buf, bridgev2.BridgeName{
		DisplayName: "Test", NetworkID: "test", BeeperBridgeType: "test",
	}))
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(userConfig), 0o600))
	upgrader := configupgrade.MergeUpgraders(buf.String(), bridgeconfig.Upgrader)
	data, _, err := configupgrade.Do(path, true, upgrader)
	require.NoError(t, err)
	var cfg bridgeconfig.Config
	require.NoError(t, yaml.Unmarshal(data, &cfg))
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	return cfg, string(written)
}

func TestPreserveConfigDefault(t *testing.T) {
	cfg, written := upgradePreserveTestConfig(t, "bridge:\n    command_prefix: '!test'\n")
	assert.False(t, cfg.Bridge.PreserveDeletes.Enabled, "the feature must be off unless enabled explicitly")
	assert.Equal(t, bridgeconfig.DefaultPreserveMarker, cfg.Bridge.PreserveDeletes.GetMarker())
	assert.Contains(t, written, "preserve_deletes:")
}

func TestPreserveConfigSurvivesUpgrade(t *testing.T) {
	cfg, written := upgradePreserveTestConfig(t, "bridge:\n    preserve_deletes:\n        enabled: true\n        marker: \"🗑️ gelöscht\"\n")
	assert.True(t, cfg.Bridge.PreserveDeletes.Enabled)
	assert.Equal(t, "🗑️ gelöscht", cfg.Bridge.PreserveDeletes.Marker)
	// The upgrade rewrites the config file. The options must still be there afterwards.
	var rewritten bridgeconfig.Config
	require.NoError(t, yaml.Unmarshal([]byte(written), &rewritten))
	assert.True(t, rewritten.Bridge.PreserveDeletes.Enabled)
	assert.Equal(t, "🗑️ gelöscht", rewritten.Bridge.PreserveDeletes.Marker)
}

func TestPreserveConfigDefaultMarkerRoundTrip(t *testing.T) {
	// The default marker ends with an invisible character. It must survive being written to the config file.
	_, written := upgradePreserveTestConfig(t, "bridge:\n    preserve_deletes:\n        enabled: true\n")
	var rewritten bridgeconfig.Config
	require.NoError(t, yaml.Unmarshal([]byte(written), &rewritten))
	assert.True(t, rewritten.Bridge.PreserveDeletes.Enabled)
	assert.Equal(t, bridgeconfig.DefaultPreserveMarker, rewritten.Bridge.PreserveDeletes.Marker)
}
