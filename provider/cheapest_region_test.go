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

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm/policy"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	azpolicy "github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	armcomputefake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5/fake"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v5"
	armnetworkfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v5/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	armresourcesfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources/fake"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudbase/garm-provider-azure/config"
	azclient "github.com/cloudbase/garm-provider-azure/internal/client"
	"github.com/cloudbase/garm-provider-azure/internal/placement"
	"github.com/cloudbase/garm-provider-azure/internal/placement/placementtest"
)

const (
	spotVMSize         = "Standard_D2ads_v5"
	spotVMSizeVCPUs    = 2
	configuredLocation = "westeurope"
	fallbackRegion     = "northeurope"
	resourceIDPrefix   = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/test-instance/providers/"
)

type vmResponder = azfake.PollerResponder[armcompute.VirtualMachinesClientCreateOrUpdateResponse]

type fakeAzure struct {
	freeSpotCores map[string]int64
	restricted    map[string]bool
	notOffered    map[string]bool
	noEphemeral   map[string]bool
	rejected      map[string]string
	provisioning  func(resp *vmResponder)
	onDelete      func()

	locations      map[string][]string
	resourceGroups []armresources.ResourceGroup
	vms            []armcompute.VirtualMachine
	extensions     int
	deletedGroups  []string
	skuFilters     []string
	usageLocations []string
}

func newFakeAzure(regionsWithQuota ...string) *fakeAzure {
	azure := &fakeAzure{
		freeSpotCores: map[string]int64{},
		restricted:    map[string]bool{},
		notOffered:    map[string]bool{},
		noEphemeral:   map[string]bool{},
		rejected:      map[string]string{},
		locations:     map[string][]string{},
	}
	for _, region := range regionsWithQuota {
		azure.freeSpotCores[region] = 10
	}
	return azure
}

func (f *fakeAzure) received(kind string, location *string, errResp *azfake.ErrorResponder) bool {
	f.locations[kind] = append(f.locations[kind], *location)
	if code, rejected := f.rejected[kind]; rejected {
		errResp.SetResponseError(http.StatusConflict, code)
		return false
	}
	return true
}

func (f *fakeAzure) client(t *testing.T) *azclient.AzureCli {
	t.Helper()
	options := func(transport azpolicy.Transporter) *policy.ClientOptions {
		return &policy.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: transport}}
	}
	credential := &azfake.TokenCredential{}

	rgCli, err := armresources.NewResourceGroupsClient("fake-id", credential, options(armresourcesfake.NewResourceGroupsServerTransport(&armresourcesfake.ResourceGroupsServer{
		CreateOrUpdate: func(_ context.Context, _ string, parameters armresources.ResourceGroup, _ *armresources.ResourceGroupsClientCreateOrUpdateOptions) (resp azfake.Responder[armresources.ResourceGroupsClientCreateOrUpdateResponse], errResp azfake.ErrorResponder) {
			f.resourceGroups = append(f.resourceGroups, parameters)
			if f.received("resourceGroup", parameters.Location, &errResp) {
				resp.SetResponse(http.StatusOK, armresources.ResourceGroupsClientCreateOrUpdateResponse{ResourceGroup: parameters}, nil)
			}
			return resp, errResp
		},
		BeginDelete: func(_ context.Context, resourceGroupName string, _ *armresources.ResourceGroupsClientBeginDeleteOptions) (resp azfake.PollerResponder[armresources.ResourceGroupsClientDeleteResponse], errResp azfake.ErrorResponder) {
			if f.onDelete != nil {
				f.onDelete()
			}
			f.deletedGroups = append(f.deletedGroups, resourceGroupName)
			resp.SetTerminalResponse(http.StatusOK, armresources.ResourceGroupsClientDeleteResponse{}, nil)
			return resp, errResp
		},
	})))
	require.NoError(t, err)

	pubIPCli, err := armnetwork.NewPublicIPAddressesClient("fake-id", credential, options(armnetworkfake.NewPublicIPAddressesServerTransport(&armnetworkfake.PublicIPAddressesServer{
		BeginCreateOrUpdate: func(_ context.Context, _ string, _ string, parameters armnetwork.PublicIPAddress, _ *armnetwork.PublicIPAddressesClientBeginCreateOrUpdateOptions) (resp azfake.PollerResponder[armnetwork.PublicIPAddressesClientCreateOrUpdateResponse], errResp azfake.ErrorResponder) {
			if f.received("publicIP", parameters.Location, &errResp) {
				parameters.ID = to.Ptr(resourceIDPrefix + "Microsoft.Network/publicIPAddresses/test-instance")
				resp.SetTerminalResponse(http.StatusOK, armnetwork.PublicIPAddressesClientCreateOrUpdateResponse{PublicIPAddress: parameters}, nil)
			}
			return resp, errResp
		},
	})))
	require.NoError(t, err)

	nsgCli, err := armnetwork.NewSecurityGroupsClient("fake-id", credential, options(armnetworkfake.NewSecurityGroupsServerTransport(&armnetworkfake.SecurityGroupsServer{
		BeginCreateOrUpdate: func(_ context.Context, _ string, _ string, parameters armnetwork.SecurityGroup, _ *armnetwork.SecurityGroupsClientBeginCreateOrUpdateOptions) (resp azfake.PollerResponder[armnetwork.SecurityGroupsClientCreateOrUpdateResponse], errResp azfake.ErrorResponder) {
			if f.received("securityGroup", parameters.Location, &errResp) {
				parameters.ID = to.Ptr(resourceIDPrefix + "Microsoft.Network/networkSecurityGroups/test-instance")
				resp.SetTerminalResponse(http.StatusOK, armnetwork.SecurityGroupsClientCreateOrUpdateResponse{SecurityGroup: parameters}, nil)
			}
			return resp, errResp
		},
	})))
	require.NoError(t, err)

	vnetCli, err := armnetwork.NewVirtualNetworksClient("fake-id", credential, options(armnetworkfake.NewVirtualNetworksServerTransport(&armnetworkfake.VirtualNetworksServer{
		BeginCreateOrUpdate: func(_ context.Context, _ string, _ string, parameters armnetwork.VirtualNetwork, _ *armnetwork.VirtualNetworksClientBeginCreateOrUpdateOptions) (resp azfake.PollerResponder[armnetwork.VirtualNetworksClientCreateOrUpdateResponse], errResp azfake.ErrorResponder) {
			if f.received("virtualNetwork", parameters.Location, &errResp) {
				resp.SetTerminalResponse(http.StatusOK, armnetwork.VirtualNetworksClientCreateOrUpdateResponse{VirtualNetwork: parameters}, nil)
			}
			return resp, errResp
		},
	})))
	require.NoError(t, err)

	subnetCli, err := armnetwork.NewSubnetsClient("fake-id", credential, options(armnetworkfake.NewSubnetsServerTransport(&armnetworkfake.SubnetsServer{
		BeginCreateOrUpdate: func(_ context.Context, _ string, _ string, _ string, parameters armnetwork.Subnet, _ *armnetwork.SubnetsClientBeginCreateOrUpdateOptions) (resp azfake.PollerResponder[armnetwork.SubnetsClientCreateOrUpdateResponse], errResp azfake.ErrorResponder) {
			parameters.ID = to.Ptr(resourceIDPrefix + "Microsoft.Network/virtualNetworks/test-instance/subnets/test-instance")
			resp.SetTerminalResponse(http.StatusOK, armnetwork.SubnetsClientCreateOrUpdateResponse{Subnet: parameters}, nil)
			return resp, errResp
		},
	})))
	require.NoError(t, err)

	nicCli, err := armnetwork.NewInterfacesClient("fake-id", credential, options(armnetworkfake.NewInterfacesServerTransport(&armnetworkfake.InterfacesServer{
		BeginCreateOrUpdate: func(_ context.Context, _ string, _ string, parameters armnetwork.Interface, _ *armnetwork.InterfacesClientBeginCreateOrUpdateOptions) (resp azfake.PollerResponder[armnetwork.InterfacesClientCreateOrUpdateResponse], errResp azfake.ErrorResponder) {
			if f.received("networkInterface", parameters.Location, &errResp) {
				parameters.ID = to.Ptr(resourceIDPrefix + "Microsoft.Network/networkInterfaces/test-instance")
				resp.SetTerminalResponse(http.StatusOK, armnetwork.InterfacesClientCreateOrUpdateResponse{Interface: parameters}, nil)
			}
			return resp, errResp
		},
	})))
	require.NoError(t, err)

	vmCli, err := armcompute.NewVirtualMachinesClient("fake-id", credential, options(armcomputefake.NewVirtualMachinesServerTransport(&armcomputefake.VirtualMachinesServer{
		BeginCreateOrUpdate: func(_ context.Context, _ string, _ string, parameters armcompute.VirtualMachine, _ *armcompute.VirtualMachinesClientBeginCreateOrUpdateOptions) (resp vmResponder, errResp azfake.ErrorResponder) {
			f.vms = append(f.vms, parameters)
			if !f.received("virtualMachine", parameters.Location, &errResp) {
				return resp, errResp
			}
			if f.provisioning != nil {
				f.provisioning(&resp)
				return resp, errResp
			}
			resp.SetTerminalResponse(http.StatusOK, armcompute.VirtualMachinesClientCreateOrUpdateResponse{VirtualMachine: parameters}, nil)
			return resp, errResp
		},
	})))
	require.NoError(t, err)

	extCli, err := armcompute.NewVirtualMachineExtensionsClient("fake-id", credential, options(armcomputefake.NewVirtualMachineExtensionsServerTransport(&armcomputefake.VirtualMachineExtensionsServer{
		BeginCreateOrUpdate: func(_ context.Context, _ string, _ string, _ string, parameters armcompute.VirtualMachineExtension, _ *armcompute.VirtualMachineExtensionsClientBeginCreateOrUpdateOptions) (resp azfake.PollerResponder[armcompute.VirtualMachineExtensionsClientCreateOrUpdateResponse], errResp azfake.ErrorResponder) {
			f.extensions++
			if f.received("extension", parameters.Location, &errResp) {
				resp.SetTerminalResponse(http.StatusOK, armcompute.VirtualMachineExtensionsClientCreateOrUpdateResponse{VirtualMachineExtension: parameters}, nil)
			}
			return resp, errResp
		},
	})))
	require.NoError(t, err)

	skuCli, err := armcompute.NewResourceSKUsClient("fake-id", credential, options(armcomputefake.NewResourceSKUsServerTransport(&armcomputefake.ResourceSKUsServer{
		NewListPager: func(options *armcompute.ResourceSKUsClientListOptions) (resp azfake.PagerResponder[armcompute.ResourceSKUsClientListResponse]) {
			f.skuFilters = append(f.skuFilters, *options.Filter)
			region := strings.TrimSuffix(strings.TrimPrefix(*options.Filter, "location eq '"), "'")
			resp.AddPage(http.StatusOK, armcompute.ResourceSKUsClientListResponse{
				ResourceSKUsResult: armcompute.ResourceSKUsResult{Value: f.skus(region)},
			}, nil)
			return resp
		},
	})))
	require.NoError(t, err)

	usageCli, err := armcompute.NewUsageClient("fake-id", credential, options(armcomputefake.NewUsageServerTransport(&armcomputefake.UsageServer{
		NewListPager: func(location string, _ *armcompute.UsageClientListOptions) (resp azfake.PagerResponder[armcompute.UsageClientListResponse]) {
			f.usageLocations = append(f.usageLocations, location)
			free, known := f.freeSpotCores[location]
			if !known {
				resp.AddResponseError(http.StatusForbidden, "AuthorizationFailed")
				return resp
			}
			resp.AddPage(http.StatusOK, armcompute.UsageClientListResponse{
				ListUsagesResult: armcompute.ListUsagesResult{Value: []*armcompute.Usage{{
					Name:         &armcompute.UsageName{Value: to.Ptr("lowPriorityCores")},
					CurrentValue: to.Ptr(int32(4)),
					Limit:        to.Ptr(free + 4),
					Unit:         to.Ptr("Count"),
				}}},
			}, nil)
			return resp
		},
	})))
	require.NoError(t, err)

	azCli := &azclient.AzureCli{}
	azCli.SetLocation(configuredLocation)
	azCli.SetClients(rgCli, vnetCli, subnetCli, nsgCli, nicCli, vmCli, pubIPCli, extCli, skuCli)
	azCli.SetUsageClient(usageCli)
	azCli.SetPollFrequency(time.Millisecond)
	return azCli
}

func (f *fakeAzure) skus(region string) []*armcompute.ResourceSKU {
	if f.notOffered[region] {
		return nil
	}
	sku := &armcompute.ResourceSKU{
		Name:         to.Ptr(spotVMSize),
		ResourceType: to.Ptr("virtualMachines"),
		Capabilities: []*armcompute.ResourceSKUCapabilities{
			{Name: to.Ptr("vCPUs"), Value: to.Ptr("2")},
			{Name: to.Ptr("EphemeralOSDiskSupported"), Value: to.Ptr("True")},
			{Name: to.Ptr("MaxResourceVolumeMB"), Value: to.Ptr("76800")},
		},
	}
	if f.noEphemeral[region] {
		sku.Capabilities[1].Value = to.Ptr("False")
	}
	if f.restricted[region] {
		sku.Restrictions = []*armcompute.ResourceSKURestrictions{{
			Type:       to.Ptr(armcompute.ResourceSKURestrictionsTypeLocation),
			ReasonCode: to.Ptr(armcompute.ResourceSKURestrictionsReasonCodeNotAvailableForSubscription),
		}}
	}
	return []*armcompute.ResourceSKU{sku}
}

func (f *fakeAzure) requireEverythingIn(t *testing.T, region string) {
	t.Helper()
	for _, kind := range []string{"resourceGroup", "publicIP", "securityGroup", "virtualNetwork", "networkInterface", "virtualMachine"} {
		require.Equal(t, []string{region}, f.locations[kind], kind)
	}
}

type cheapestRegionTest struct {
	cfg      *config.Config
	azure    *fakeAzure
	prices   *placementtest.PriceAPI
	state    *placement.State
	provider *azureProvider
}

func newCheapestRegionTest(t *testing.T) *cheapestRegionTest {
	t.Helper()
	cfg := &config.Config{
		Location:            configuredLocation,
		HomeRegion:          fallbackRegion,
		UseEphemeralStorage: true,
		RegionSelection:     config.RegionSelectionCheapest,
		StateDir:            t.TempDir(),
	}
	azure := newFakeAzure("westus2", "eastus", "swedencentral")
	prices := placementtest.NewPriceAPI(t,
		placementtest.LinuxSpot("eastus", 0.02),
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("swedencentral", 0.03),
	)
	state, err := placement.OpenState(cfg.StateDir)
	require.NoError(t, err)
	return &cheapestRegionTest{
		cfg:    cfg,
		azure:  azure,
		prices: prices,
		state:  state,
		provider: &azureProvider{
			controllerID: "controllerID",
			azCli:        azure.client(t),
			cfg:          cfg,
			prices:       placement.PriceSource{BaseURL: prices.URL},
		},
	}
}

func (c *cheapestRegionTest) create(extraSpecs string) (params.ProviderInstance, error) {
	return c.provider.CreateInstance(context.Background(), params.BootstrapInstance{
		Name:          "test-instance",
		InstanceToken: "test-token",
		PoolID:        "pool-id",
		OSArch:        params.Amd64,
		OSType:        params.Linux,
		Image:         "Canonical:ubuntu-24_04-lts:server:latest",
		Flavor:        spotVMSize,
		Tools: []params.RunnerApplicationDownload{{
			OS:                to.Ptr("linux"),
			Architecture:      to.Ptr("x64"),
			DownloadURL:       to.Ptr("http://test.com"),
			Filename:          to.Ptr("runner.tar.gz"),
			SHA256Checksum:    to.Ptr("sha256:1123"),
			TempDownloadToken: to.Ptr("test-token"),
		}},
		ExtraSpecs: json.RawMessage(extraSpecs),
	})
}

func (c *cheapestRegionTest) stateFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(c.cfg.StateDir)
	require.NoError(t, err)
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

const spotPool = `{"spot": true, "allocate_public_ip": true}`

func TestCheapestRegionCreatesTheSpotVMInTheCheapestRegion(t *testing.T) {
	c := newCheapestRegionTest(t)

	instance, err := c.create(spotPool)
	require.NoError(t, err)

	assert.Equal(t, "test-instance", instance.ProviderID)
	assert.Equal(t, params.InstanceStatus("running"), instance.Status)
	c.azure.requireEverythingIn(t, "westus2")
	vm := c.azure.vms[0]
	require.NotNil(t, vm.Properties.Priority)
	assert.Equal(t, armcompute.VirtualMachinePriorityTypesSpot, *vm.Properties.Priority)
	for name, tags := range map[string]map[string]*string{"resource group": c.azure.resourceGroups[0].Tags, "VM": vm.Tags} {
		require.Contains(t, tags, "garm-region", name)
		assert.Equal(t, "westus2", *tags["garm-region"], name)
		require.Contains(t, tags, "garm-placement", name)
		assert.Equal(t, "spot", *tags["garm-placement"], name)
		require.Contains(t, tags, "garm-spot-usd-per-hour", name)
		assert.Equal(t, "0.01", *tags["garm-spot-usd-per-hour"], name)
		require.Contains(t, tags, "garm-pool-id", name)
		assert.Equal(t, "pool-id", *tags["garm-pool-id"], name)
	}
	assert.Empty(t, c.azure.deletedGroups)
}

func TestCheapestRegionLooksUpTheEphemeralDiskInThePickedRegion(t *testing.T) {
	c := newCheapestRegionTest(t)

	_, err := c.create(spotPool)
	require.NoError(t, err)

	require.NotEmpty(t, c.azure.skuFilters)
	for _, filter := range c.azure.skuFilters {
		assert.Equal(t, "location eq 'westus2'", filter)
	}
}

func TestCheapestRegionCanBeTurnedOnByPoolExtraSpecs(t *testing.T) {
	c := newCheapestRegionTest(t)
	c.cfg.RegionSelection = config.RegionSelectionFixed

	_, err := c.create(`{"spot": true, "allocate_public_ip": true, "region_selection": "cheapest", "candidate_regions": ["swedencentral", "eastus"]}`)
	require.NoError(t, err)

	c.azure.requireEverythingIn(t, "eastus")
}

func TestCheapestRegionHonoursASkipMarker(t *testing.T) {
	c := newCheapestRegionTest(t)
	require.NoError(t, c.state.SkipRegion("westus2", spotVMSize, time.Now().Add(30*time.Minute), "AllocationFailed"))

	_, err := c.create(spotPool)
	require.NoError(t, err)

	c.azure.requireEverythingIn(t, "eastus")
	assert.NotContains(t, c.azure.usageLocations, "westus2")
}

func TestCheapestRegionIgnoresAnExpiredSkipMarker(t *testing.T) {
	c := newCheapestRegionTest(t)
	require.NoError(t, c.state.SkipRegion("westus2", spotVMSize, time.Now().Add(-time.Second), "AllocationFailed"))

	_, err := c.create(spotPool)
	require.NoError(t, err)

	c.azure.requireEverythingIn(t, "westus2")
}

func TestCheapestRegionSkipsARegionThatCannotTakeTheVM(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(azure *fakeAzure)
	}{
		{
			name:    "no free Spot quota",
			prepare: func(azure *fakeAzure) { azure.freeSpotCores["westus2"] = 0 },
		},
		{
			name:    "less free Spot quota than the size has vCPUs",
			prepare: func(azure *fakeAzure) { azure.freeSpotCores["westus2"] = spotVMSizeVCPUs - 1 },
		},
		{
			name:    "the size is restricted for the subscription",
			prepare: func(azure *fakeAzure) { azure.restricted["westus2"] = true },
		},
		{
			name:    "the size is not offered",
			prepare: func(azure *fakeAzure) { azure.notOffered["westus2"] = true },
		},
		{
			name:    "the subscription cannot read the region",
			prepare: func(azure *fakeAzure) { delete(azure.freeSpotCores, "westus2") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCheapestRegionTest(t)
			tt.prepare(c.azure)

			_, err := c.create(spotPool)
			require.NoError(t, err)

			c.azure.requireEverythingIn(t, "eastus")
			assert.Equal(t, "0.02", *c.azure.vms[0].Tags["garm-spot-usd-per-hour"])
		})
	}
}

func TestCheapestRegionDoesNotListTheVMSizesOfARegionWithoutSpotQuota(t *testing.T) {
	c := newCheapestRegionTest(t)
	c.azure.freeSpotCores["westus2"] = 0

	_, err := c.create(spotPool)
	require.NoError(t, err)

	assert.NotContains(t, c.azure.skuFilters, "location eq 'westus2'")
}

func TestCheapestRegionAcceptsExactlyEnoughSpotQuota(t *testing.T) {
	c := newCheapestRegionTest(t)
	c.azure.freeSpotCores["westus2"] = spotVMSizeVCPUs

	_, err := c.create(spotPool)
	require.NoError(t, err)

	c.azure.requireEverythingIn(t, "westus2")
}

func TestCheapestRegionFailsFastWhenTheCreateFails(t *testing.T) {
	tests := []struct {
		name            string
		prepare         func(azure *fakeAzure)
		errString       string
		expectedDeletes []string
	}{
		{
			name:            "Azure rejects the VM",
			prepare:         func(azure *fakeAzure) { azure.rejected["virtualMachine"] = "SkuNotAvailable" },
			errString:       "SkuNotAvailable",
			expectedDeletes: []string{"test-instance"},
		},
		{
			name: "provisioning fails after Azure accepted the VM",
			prepare: func(azure *fakeAzure) {
				azure.provisioning = func(resp *vmResponder) {
					resp.AddNonTerminalResponse(http.StatusCreated, nil)
					resp.SetTerminalError(http.StatusConflict, "AllocationFailed")
				}
			},
			errString:       "AllocationFailed",
			expectedDeletes: []string{"test-instance"},
		},
		{
			name:            "Azure rejects the public IP",
			prepare:         func(azure *fakeAzure) { azure.rejected["publicIP"] = "PublicIPCountLimitReached" },
			errString:       "PublicIPCountLimitReached",
			expectedDeletes: []string{"test-instance"},
		},
		{
			name:            "Azure rejects the resource group",
			prepare:         func(azure *fakeAzure) { azure.rejected["resourceGroup"] = "LocationNotAvailableForResourceGroup" },
			errString:       "LocationNotAvailableForResourceGroup",
			expectedDeletes: nil,
		},
		{
			name:            "the region has no ephemeral OS disk for the size",
			prepare:         func(azure *fakeAzure) { azure.noEphemeral["westus2"] = true },
			errString:       "failed to get max ephemeral disk size",
			expectedDeletes: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCheapestRegionTest(t)
			tt.prepare(c.azure)
			markerPath := filepath.Join(c.cfg.StateDir, "skip-westus2-"+spotVMSize+".json")
			markerExistedBeforeRollback := false
			c.azure.onDelete = func() {
				_, err := os.Stat(markerPath)
				markerExistedBeforeRollback = err == nil
			}

			before := time.Now()
			_, err := c.create(spotPool)
			require.ErrorContains(t, err, tt.errString)

			raw, err := os.ReadFile(markerPath)
			require.NoError(t, err)
			var marker struct {
				Region    string    `json:"region"`
				VMSize    string    `json:"vm_size"`
				ExpiresAt time.Time `json:"expires_at"`
				Reason    string    `json:"reason"`
			}
			require.NoError(t, json.Unmarshal(raw, &marker))
			assert.Equal(t, "westus2", marker.Region)
			assert.Equal(t, spotVMSize, marker.VMSize)
			assert.Contains(t, marker.Reason, tt.errString)
			assert.WithinDuration(t, before.Add(30*time.Minute), marker.ExpiresAt, time.Minute)

			assert.Equal(t, tt.expectedDeletes, c.azure.deletedGroups)
			if len(tt.expectedDeletes) > 0 {
				assert.True(t, markerExistedBeforeRollback, "the marker must exist before the rollback starts")
			}
			for kind, locations := range c.azure.locations {
				for _, location := range locations {
					assert.Equal(t, "westus2", location, "%s was created in another region during the same call", kind)
				}
			}
		})
	}
}

func TestCheapestRegionUsesTheNextRegionAfterAFailedCreate(t *testing.T) {
	c := newCheapestRegionTest(t)
	c.azure.rejected["virtualMachine"] = "SkuNotAvailable"
	_, err := c.create(spotPool)
	require.Error(t, err)

	replacement := newCheapestRegionTest(t)
	replacement.cfg.StateDir = c.cfg.StateDir
	_, err = replacement.create(spotPool)
	require.NoError(t, err)

	replacement.azure.requireEverythingIn(t, "eastus")
}

func TestCheapestRegionSkipsTheRegionForTheConfiguredTime(t *testing.T) {
	c := newCheapestRegionTest(t)
	c.cfg.RegionSkipMinutes = 5
	c.azure.rejected["virtualMachine"] = "SkuNotAvailable"

	before := time.Now()
	_, err := c.create(spotPool)
	require.Error(t, err)

	raw, err := os.ReadFile(filepath.Join(c.cfg.StateDir, "skip-westus2-"+spotVMSize+".json"))
	require.NoError(t, err)
	var marker struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	require.NoError(t, json.Unmarshal(raw, &marker))
	assert.WithinDuration(t, before.Add(5*time.Minute), marker.ExpiresAt, time.Minute)
}

func TestCheapestRegionAcceptsAVMThatIsStillProvisioningWhenTheWaitEnds(t *testing.T) {
	c := newCheapestRegionTest(t)
	c.cfg.ProvisioningWaitSeconds = 1
	c.azure.provisioning = func(resp *vmResponder) {
		for range 100000 {
			resp.AddNonTerminalResponse(http.StatusCreated, nil)
		}
		resp.SetTerminalError(http.StatusConflict, "AllocationFailed")
	}

	instance, err := c.create(spotPool)
	require.NoError(t, err)

	assert.Equal(t, params.InstanceStatus("running"), instance.Status)
	assert.Empty(t, c.azure.deletedGroups)
	assert.NotContains(t, c.stateFiles(t), "skip-westus2-"+spotVMSize+".json")
}

func TestCheapestRegionRecordsThatNoRegionIsUsable(t *testing.T) {
	c := newCheapestRegionTest(t)
	c.azure.freeSpotCores["westus2"] = 0
	c.azure.restricted["eastus"] = true
	c.azure.notOffered["swedencentral"] = true

	before := time.Now()
	_, err := c.create(spotPool)

	var noSpot *placement.NoSpotRegionError
	require.ErrorAs(t, err, &noSpot)
	assert.Equal(t, spotVMSize, noSpot.VMSize)
	assert.Equal(t, placement.Linux, noSpot.OS)
	assert.Empty(t, c.azure.locations)

	raw, err := os.ReadFile(filepath.Join(c.cfg.StateDir, "no-spot-linux.json"))
	require.NoError(t, err)
	var record struct {
		OS             string `json:"os"`
		VMSize         string `json:"vm_size"`
		RecordedAtUnix int64  `json:"recorded_at_unix"`
		Rejected       []struct {
			Region string `json:"region"`
		} `json:"rejected"`
	}
	require.NoError(t, json.Unmarshal(raw, &record))
	assert.Equal(t, "linux", record.OS)
	assert.Equal(t, spotVMSize, record.VMSize)
	assert.InDelta(t, before.Unix(), record.RecordedAtUnix, 60)
	assert.Len(t, record.Rejected, 3)
}

func TestCheapestRegionFallsBackToARegularVMInTheHomeRegion(t *testing.T) {
	c := newCheapestRegionTest(t)
	c.cfg.OnDemandFallback = true
	for region := range c.azure.freeSpotCores {
		c.azure.freeSpotCores[region] = 0
	}

	_, err := c.create(spotPool)
	require.NoError(t, err)

	c.azure.requireEverythingIn(t, fallbackRegion)
	vm := c.azure.vms[0]
	assert.Nil(t, vm.Properties.Priority)
	assert.Nil(t, vm.Properties.EvictionPolicy)
	assert.Nil(t, vm.Properties.BillingProfile)
	assert.Equal(t, fallbackRegion, *vm.Tags["garm-region"])
	assert.Equal(t, "on-demand-fallback", *vm.Tags["garm-placement"])
	assert.NotContains(t, vm.Tags, "garm-spot-usd-per-hour")
	assert.NotContains(t, c.stateFiles(t), "no-spot-linux.json")
}

func TestCheapestRegionRecordsNoSpotWhenTheRegionsCannotBeJudged(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(c *cheapestRegionTest)
		reason  string
	}{
		{
			name:    "the price API fails and nothing is cached",
			prepare: func(c *cheapestRegionTest) { c.prices.SetStatus(http.StatusServiceUnavailable) },
			reason:  "the price API failed: the price API answered 503 Service Unavailable",
		},
		{
			name:    "the price API has no Spot price for the size and OS",
			prepare: func(c *cheapestRegionTest) { c.prices.SetPrices(placementtest.WindowsSpot("eastus", 0.05)) },
			reason:  "the price API has no Spot price for Standard_D2ads_v5 (linux)",
		},
		{
			name: "every region check fails",
			prepare: func(c *cheapestRegionTest) {
				for region := range c.azure.freeSpotCores {
					delete(c.azure.freeSpotCores, region)
				}
			},
			reason: "failed to check any region: westus2: failed to list compute usage in westus2",
		},
	}

	for _, tt := range tests {
		for _, fallback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, on_demand_fallback %t", tt.name, fallback), func(t *testing.T) {
				c := newCheapestRegionTest(t)
				c.cfg.OnDemandFallback = fallback
				tt.prepare(c)

				before := time.Now()
				_, err := c.create(spotPool)

				require.Error(t, err)
				var noSpot *placement.NoSpotRegionError
				assert.False(t, errors.As(err, &noSpot))
				assert.Empty(t, c.azure.locations)
				raw, err := os.ReadFile(filepath.Join(c.cfg.StateDir, "no-spot-linux.json"))
				require.NoError(t, err)
				var record struct {
					OS             string                `json:"os"`
					VMSize         string                `json:"vm_size"`
					RecordedAt     time.Time             `json:"recorded_at"`
					RecordedAtUnix int64                 `json:"recorded_at_unix"`
					Rejected       []placement.Rejection `json:"rejected"`
				}
				require.NoError(t, json.Unmarshal(raw, &record))
				assert.Equal(t, "linux", record.OS)
				assert.Equal(t, spotVMSize, record.VMSize)
				assert.InDelta(t, before.Unix(), record.RecordedAtUnix, 60)
				assert.Equal(t, record.RecordedAtUnix, record.RecordedAt.Unix())
				require.Len(t, record.Rejected, 1)
				assert.Empty(t, record.Rejected[0].Region)
				assert.Contains(t, record.Rejected[0].Reason, tt.reason)
			})
		}
	}
}

func TestCheapestRegionRanksWindowsPoolsByWindowsPrices(t *testing.T) {
	c := newCheapestRegionTest(t)
	c.prices.SetPrices(
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("eastus", 0.02),
		placementtest.WindowsSpot("westus2", 0.09),
		placementtest.WindowsSpot("eastus", 0.05),
	)

	_, err := c.provider.CreateInstance(context.Background(), params.BootstrapInstance{
		Name:          "test-instance",
		InstanceToken: "test-token",
		OSArch:        params.Amd64,
		OSType:        params.Windows,
		Image:         "MicrosoftWindowsServer:WindowsServer:2022-Datacenter:latest",
		Flavor:        spotVMSize,
		Tools: []params.RunnerApplicationDownload{{
			OS:                to.Ptr("win"),
			Architecture:      to.Ptr("x64"),
			DownloadURL:       to.Ptr("http://test.com"),
			Filename:          to.Ptr("runner.zip"),
			SHA256Checksum:    to.Ptr("sha256:1123"),
			TempDownloadToken: to.Ptr("test-token"),
		}},
		ExtraSpecs: json.RawMessage(spotPool),
	})
	require.NoError(t, err)

	c.azure.requireEverythingIn(t, "eastus")
	assert.Equal(t, []string{"eastus"}, c.azure.locations["extension"])
	assert.Equal(t, "0.05", *c.azure.vms[0].Tags["garm-spot-usd-per-hour"])
}

func TestFixedRegionSelectionKeepsSpotVMsInTheConfiguredLocation(t *testing.T) {
	for _, selection := range []config.RegionSelection{"", config.RegionSelectionFixed} {
		t.Run("region_selection "+string(selection), func(t *testing.T) {
			c := newCheapestRegionTest(t)
			c.cfg.RegionSelection = selection
			c.azure.provisioning = func(resp *vmResponder) {
				resp.AddNonTerminalResponse(http.StatusCreated, nil)
				resp.SetTerminalError(http.StatusConflict, "AllocationFailed")
			}

			_, err := c.create(spotPool)
			require.NoError(t, err)

			c.azure.requireEverythingIn(t, configuredLocation)
			vm := c.azure.vms[0]
			require.NotNil(t, vm.Properties.Priority)
			assert.Equal(t, armcompute.VirtualMachinePriorityTypesSpot, *vm.Properties.Priority)
			assert.NotContains(t, vm.Tags, "garm-region")
			assert.NotContains(t, vm.Tags, "garm-placement")
			assert.NotContains(t, vm.Tags, "garm-spot-usd-per-hour")
			assert.Empty(t, c.prices.FirstPageFilters())
			assert.Empty(t, c.azure.usageLocations)
			assert.Empty(t, c.stateFiles(t))
			assert.Empty(t, c.azure.deletedGroups)
		})
	}
}

func TestCheapestRegionSelectionLeavesRegularVMsInTheConfiguredLocation(t *testing.T) {
	c := newCheapestRegionTest(t)

	_, err := c.create(`{"allocate_public_ip": true}`)
	require.NoError(t, err)

	c.azure.requireEverythingIn(t, configuredLocation)
	assert.Nil(t, c.azure.vms[0].Properties.Priority)
	assert.Empty(t, c.prices.FirstPageFilters())
	assert.Empty(t, c.stateFiles(t))
}
