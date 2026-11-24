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

package spec

import (
	"encoding/json"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/cloudbase/garm-provider-azure/config"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const existingSubnetID = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-Network/providers/Microsoft.Network/virtualNetworks/vnet-Default/subnets/snet-default"

func TestCheapestRegionSettings(t *testing.T) {
	bootstrapParams := params.BootstrapInstance{
		Name:          "test-instance",
		InstanceToken: "test-token",
		OSArch:        params.Amd64,
		OSType:        params.Linux,
		Image:         "Canonical:ubuntu-24_04-lts:server:latest",
		Flavor:        "Standard_D2ads_v5",
		Tools: []params.RunnerApplicationDownload{
			{
				OS:                to.Ptr("linux"),
				Architecture:      to.Ptr("x64"),
				DownloadURL:       to.Ptr("http://test.com"),
				Filename:          to.Ptr("runner.tar.gz"),
				SHA256Checksum:    to.Ptr("sha256:1123"),
				TempDownloadToken: to.Ptr("test-token"),
			},
		},
	}

	tests := []struct {
		name       string
		cfg        config.Config
		extraSpecs string
		want       *CheapestRegion
		errString  string
	}{
		{
			name:       "spot stays in the configured location by default",
			cfg:        config.Config{Location: "westeurope"},
			extraSpecs: `{"spot": true}`,
			want:       nil,
		},
		{
			name: "the config turns on cheapest selection for spot pools",
			cfg: config.Config{
				Location:         "westeurope",
				RegionSelection:  config.RegionSelectionCheapest,
				StateDir:         "/var/lib/garm-azure",
				CandidateRegions: []string{"East US 2", "swedencentral"},
				OnDemandFallback: true,
			},
			extraSpecs: `{"spot": true}`,
			want: &CheapestRegion{
				CandidateRegions: []string{"eastus2", "swedencentral"},
				OnDemandFallback: true,
				HomeRegion:       "westeurope",
			},
		},
		{
			name: "home_region replaces the location for the fallback",
			cfg: config.Config{
				Location:        "westeurope",
				HomeRegion:      "northeurope",
				RegionSelection: config.RegionSelectionCheapest,
				StateDir:        "/var/lib/garm-azure",
			},
			extraSpecs: `{"spot": true}`,
			want:       &CheapestRegion{HomeRegion: "northeurope"},
		},
		{
			name: "extra specs override the config",
			cfg: config.Config{
				Location:         "westeurope",
				StateDir:         "/var/lib/garm-azure",
				CandidateRegions: []string{"eastus2"},
				OnDemandFallback: true,
			},
			extraSpecs: `{
				"spot": true,
				"region_selection": "cheapest",
				"candidate_regions": ["uksouth"],
				"on_demand_fallback": false
			}`,
			want: &CheapestRegion{
				CandidateRegions: []string{"uksouth"},
				OnDemandFallback: false,
				HomeRegion:       "westeurope",
			},
		},
		{
			name: "extra specs can pin a pool to the configured location",
			cfg: config.Config{
				Location:        "westeurope",
				RegionSelection: config.RegionSelectionCheapest,
				StateDir:        "/var/lib/garm-azure",
			},
			extraSpecs: `{"spot": true, "region_selection": "fixed"}`,
			want:       nil,
		},
		{
			name: "cheapest selection does not apply to regular VMs",
			cfg: config.Config{
				Location:        "westeurope",
				RegionSelection: config.RegionSelectionCheapest,
				StateDir:        "/var/lib/garm-azure",
			},
			extraSpecs: `{}`,
			want:       nil,
		},
		{
			name:       "cheapest selection needs a state directory",
			cfg:        config.Config{Location: "westeurope"},
			extraSpecs: `{"spot": true, "region_selection": "cheapest"}`,
			errString:  "requires state_dir",
		},
		{
			name: "cheapest selection cannot use an existing subnet",
			cfg: config.Config{
				Location:                "westeurope",
				RegionSelection:         config.RegionSelectionCheapest,
				StateDir:                "/var/lib/garm-azure",
				DisableIsolatedNetworks: true,
				VnetSubnetID:            existingSubnetID,
			},
			extraSpecs: `{"spot": true}`,
			errString:  "cannot use vnet_subnet_id",
		},
		{
			name:       "an unknown region_selection is rejected",
			cfg:        config.Config{Location: "westeurope", StateDir: "/var/lib/garm-azure"},
			extraSpecs: `{"spot": true, "region_selection": "nearest"}`,
			errString:  "region_selection",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bootstrapParams.ExtraSpecs = json.RawMessage(tt.extraSpecs)

			got, err := GetRunnerSpecFromBootstrapParams(bootstrapParams, "test-controller", &tt.cfg)
			if tt.errString != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errString)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.CheapestRegion)
		})
	}
}
