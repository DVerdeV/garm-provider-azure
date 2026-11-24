// Copyright 2026 garm-provider-azure contributors
//
//    Licensed under the Apache License, Version 2.0 (the "License"); you may
//    not use this file except in compliance with the License. You may obtain
//    a copy of the License at
//
//         http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
//    WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
//    License for the specific language governing permissions and limitations
//    under the License.

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const credentialsTOML = `
[credentials]
subscription_id = "sample_sub_id"

[credentials.service_principal]
tenant_id = "sample_tenant_id"
client_id = "sample_client_id"
client_secret = "super secret client secret"
`

func loadConfig(t *testing.T, settings string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(settings+credentialsTOML), 0o600))
	return NewConfig(path)
}

func TestRegionSelectionSettingsLoadFromTOML(t *testing.T) {
	cfg, err := loadConfig(t, `
location = "westeurope"
region_selection = "cheapest"
candidate_regions = ["eastus2", "swedencentral"]
on_demand_fallback = true
home_region = "northeurope"
state_dir = "/var/lib/garm-azure"
provisioning_wait_seconds = 90
price_cache_ttl_minutes = 30
region_skip_minutes = 10
`)
	require.NoError(t, err)

	require.Equal(t, RegionSelectionCheapest, cfg.RegionSelection)
	require.Equal(t, []string{"eastus2", "swedencentral"}, cfg.CandidateRegions)
	require.True(t, cfg.OnDemandFallback)
	require.Equal(t, "northeurope", cfg.HomeRegion)
	require.Equal(t, "/var/lib/garm-azure", cfg.StateDir)
	require.Equal(t, 90*time.Second, cfg.ProvisioningWait())
	require.Equal(t, 30*time.Minute, cfg.PriceCacheTTL())
	require.Equal(t, 10*time.Minute, cfg.RegionSkip())
}

func TestRegionSelectionDefaults(t *testing.T) {
	cfg, err := loadConfig(t, `location = "westeurope"`)
	require.NoError(t, err)

	require.Empty(t, cfg.RegionSelection)
	require.False(t, cfg.OnDemandFallback)
	require.Equal(t, 2*time.Minute, cfg.ProvisioningWait())
	require.Equal(t, 6*time.Hour, cfg.PriceCacheTTL())
	require.Equal(t, 30*time.Minute, cfg.RegionSkip())
}

func TestRegionSelectionValidation(t *testing.T) {
	tests := []struct {
		name      string
		settings  string
		errString string
	}{
		{
			name:     "fixed needs no state directory",
			settings: `region_selection = "fixed"`,
		},
		{
			name:      "cheapest needs a state directory",
			settings:  `region_selection = "cheapest"`,
			errString: "requires state_dir",
		},
		{
			name:      "unknown region_selection",
			settings:  `region_selection = "nearest"`,
			errString: "invalid region_selection",
		},
		{
			name:      "negative provisioning wait",
			settings:  `provisioning_wait_seconds = -1`,
			errString: "provisioning_wait_seconds",
		},
		{
			name:      "negative price cache TTL",
			settings:  `price_cache_ttl_minutes = -1`,
			errString: "price_cache_ttl_minutes",
		},
		{
			name:      "negative region skip",
			settings:  `region_skip_minutes = -1`,
			errString: "region_skip_minutes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadConfig(t, "location = \"westeurope\"\n"+tt.settings)
			if tt.errString == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.errString)
		})
	}
}
