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

package client

import (
	"context"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm/policy"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	armcomputefake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5/fake"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudbase/garm-provider-azure/internal/spec"
)

func usage(name string, current int32, limit int64) *armcompute.Usage {
	return &armcompute.Usage{
		Name:         &armcompute.UsageName{Value: to.Ptr(name)},
		CurrentValue: to.Ptr(current),
		Limit:        to.Ptr(limit),
		Unit:         to.Ptr("Count"),
	}
}

func TestFreeSpotCores(t *testing.T) {
	tests := []struct {
		name      string
		respCode  int
		usages    []*armcompute.Usage
		expected  int64
		errString string
	}{
		{
			name:     "limit minus current value of the Spot entry",
			respCode: 200,
			usages: []*armcompute.Usage{
				usage("cores", 8, 100),
				usage("lowPriorityCores", 4, 10),
				usage("standardDADSv5Family", 8, 20),
			},
			expected: 6,
		},
		{
			name:     "no quota",
			respCode: 200,
			usages:   []*armcompute.Usage{usage("lowPriorityCores", 0, 0)},
			expected: 0,
		},
		{
			name:      "no Spot entry",
			respCode:  200,
			usages:    []*armcompute.Usage{usage("cores", 8, 100)},
			errString: "has no lowPriorityCores entry",
		},
		{
			name:      "usage cannot be listed",
			respCode:  403,
			errString: "failed to list compute usage in westus2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var askedLocation string
			server := armcomputefake.UsageServer{
				NewListPager: func(location string, _ *armcompute.UsageClientListOptions) (resp azfake.PagerResponder[armcompute.UsageClientListResponse]) {
					askedLocation = location
					resp.AddPage(tt.respCode, armcompute.UsageClientListResponse{
						ListUsagesResult: armcompute.ListUsagesResult{Value: tt.usages},
					}, nil)
					return resp
				},
			}
			usageCli, err := armcompute.NewUsageClient("fake-id", &azfake.TokenCredential{}, &policy.ClientOptions{
				ClientOptions: azcore.ClientOptions{
					Transport: armcomputefake.NewUsageServerTransport(&server),
				},
			})
			require.NoError(t, err)
			a := AzureCli{location: "eastus"}
			a.SetUsageClient(usageCli)

			free, err := a.FreeSpotCores(context.Background(), "westus2")
			if tt.errString != "" {
				require.ErrorContains(t, err, tt.errString)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, free)
			assert.Equal(t, "westus2", askedLocation)
		})
	}
}

func vmSKU(name string, restrictions ...armcompute.ResourceSKURestrictionsType) *armcompute.ResourceSKU {
	sku := &armcompute.ResourceSKU{
		Name:         to.Ptr(name),
		ResourceType: to.Ptr("virtualMachines"),
		Capabilities: []*armcompute.ResourceSKUCapabilities{
			{Name: to.Ptr("MaxResourceVolumeMB"), Value: to.Ptr("76800")},
			{Name: to.Ptr("vCPUs"), Value: to.Ptr("2")},
		},
	}
	for _, restriction := range restrictions {
		sku.Restrictions = append(sku.Restrictions, &armcompute.ResourceSKURestrictions{
			Type:       to.Ptr(restriction),
			ReasonCode: to.Ptr(armcompute.ResourceSKURestrictionsReasonCodeNotAvailableForSubscription),
		})
	}
	return sku
}

func TestGetVMSizeInLocation(t *testing.T) {
	tests := []struct {
		name      string
		respCode  int
		skus      []*armcompute.ResourceSKU
		offered   bool
		expected  VMSizeInLocation
		errString string
	}{
		{
			name:     "offered without restriction",
			respCode: 200,
			skus: []*armcompute.ResourceSKU{
				{Name: to.Ptr("Standard_D2ads_v5"), ResourceType: to.Ptr("disks")},
				vmSKU("Standard_D4ads_v5"),
				vmSKU("Standard_D2ads_v5"),
			},
			offered:  true,
			expected: VMSizeInLocation{VCPUs: 2},
		},
		{
			name:     "the size name matches without regard to case",
			respCode: 200,
			skus:     []*armcompute.ResourceSKU{vmSKU("standard_d2ads_v5")},
			offered:  true,
			expected: VMSizeInLocation{VCPUs: 2},
		},
		{
			name:     "restricted in the whole location",
			respCode: 200,
			skus:     []*armcompute.ResourceSKU{vmSKU("Standard_D2ads_v5", armcompute.ResourceSKURestrictionsTypeLocation)},
			offered:  true,
			expected: VMSizeInLocation{VCPUs: 2, LocationRestricted: true},
		},
		{
			name:     "restricted in single zones only",
			respCode: 200,
			skus:     []*armcompute.ResourceSKU{vmSKU("Standard_D2ads_v5", armcompute.ResourceSKURestrictionsTypeZone)},
			offered:  true,
			expected: VMSizeInLocation{VCPUs: 2},
		},
		{
			name:     "not offered",
			respCode: 200,
			skus:     []*armcompute.ResourceSKU{vmSKU("Standard_D4ads_v5")},
			offered:  false,
		},
		{
			name:      "sizes cannot be listed",
			respCode:  403,
			errString: "failed to list VM sizes in westus2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var askedFilter string
			server := armcomputefake.ResourceSKUsServer{
				NewListPager: func(options *armcompute.ResourceSKUsClientListOptions) (resp azfake.PagerResponder[armcompute.ResourceSKUsClientListResponse]) {
					askedFilter = *options.Filter
					resp.AddPage(tt.respCode, armcompute.ResourceSKUsClientListResponse{
						ResourceSKUsResult: armcompute.ResourceSKUsResult{Value: tt.skus},
					}, nil)
					return resp
				},
			}
			skuCli, err := armcompute.NewResourceSKUsClient("fake-id", &azfake.TokenCredential{}, &policy.ClientOptions{
				ClientOptions: azcore.ClientOptions{
					Transport: armcomputefake.NewResourceSKUsServerTransport(&server),
				},
			})
			require.NoError(t, err)
			a := AzureCli{location: "eastus", resourceSKUCli: skuCli}

			size, offered, err := a.GetVMSizeInLocation(context.Background(), "westus2", "Standard_D2ads_v5")
			if tt.errString != "" {
				require.ErrorContains(t, err, tt.errString)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.offered, offered)
			assert.Equal(t, tt.expected, size)
			assert.Equal(t, "location eq 'westus2'", askedFilter)
		})
	}
}

func linuxRunnerSpec() *spec.RunnerSpec {
	tools := params.RunnerApplicationDownload{
		OS:                to.Ptr("linux"),
		Architecture:      to.Ptr("x64"),
		DownloadURL:       to.Ptr("http://test.com"),
		Filename:          to.Ptr("runner.tar.gz"),
		SHA256Checksum:    to.Ptr("sha256:1123"),
		TempDownloadToken: to.Ptr("test-token"),
	}
	return &spec.RunnerSpec{
		VMSize:             "Standard_D2ads_v5",
		Spot:               true,
		AdminUsername:      "admin",
		StorageAccountType: "Standard_LRS",
		DiskSizeGB:         128,
		BootstrapParams: params.BootstrapInstance{
			Name:          "test-instance",
			InstanceToken: "test-token",
			OSArch:        params.Amd64,
			OSType:        params.Linux,
			Image:         "Canonical:ubuntu-24_04-lts:server:latest",
			Flavor:        "Standard_D2ads_v5",
			Tools:         []params.RunnerApplicationDownload{tools},
		},
		Tools: tools,
	}
}

func TestCreateVirtualMachineAndWait(t *testing.T) {
	type vmResponder = azfake.PollerResponder[armcompute.VirtualMachinesClientCreateOrUpdateResponse]
	stillCreating := func(resp *vmResponder) {
		for range 10000 {
			resp.AddNonTerminalResponse(201, nil)
		}
	}

	tests := []struct {
		name      string
		wait      time.Duration
		respond   func(resp *vmResponder)
		errString string
	}{
		{
			name: "provisioning succeeds",
			wait: time.Minute,
			respond: func(resp *vmResponder) {
				resp.AddNonTerminalResponse(201, nil)
				resp.SetTerminalResponse(200, armcompute.VirtualMachinesClientCreateOrUpdateResponse{}, nil)
			},
		},
		{
			name: "provisioning fails after Azure accepted the VM",
			wait: time.Minute,
			respond: func(resp *vmResponder) {
				resp.AddNonTerminalResponse(201, nil)
				resp.SetTerminalError(409, "AllocationFailed")
			},
			errString: "AllocationFailed",
		},
		{
			name: "the wait ends before Azure reports a result",
			wait: 50 * time.Millisecond,
			respond: func(resp *vmResponder) {
				stillCreating(resp)
				resp.SetTerminalError(409, "AllocationFailed")
			},
		},
		{
			name: "without a wait a later failure is not seen",
			wait: 0,
			respond: func(resp *vmResponder) {
				resp.AddNonTerminalResponse(201, nil)
				resp.SetTerminalError(409, "AllocationFailed")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := armcomputefake.VirtualMachinesServer{
				BeginCreateOrUpdate: func(_ context.Context, _ string, _ string, _ armcompute.VirtualMachine, _ *armcompute.VirtualMachinesClientBeginCreateOrUpdateOptions) (resp vmResponder, errResp azfake.ErrorResponder) {
					tt.respond(&resp)
					return resp, errResp
				},
			}
			vmCli, err := armcompute.NewVirtualMachinesClient("fake-id", &azfake.TokenCredential{}, &policy.ClientOptions{
				ClientOptions: azcore.ClientOptions{
					Transport: armcomputefake.NewVirtualMachinesServerTransport(&server),
				},
			})
			require.NoError(t, err)
			a := AzureCli{location: "westus2", vmCli: vmCli}
			a.SetPollFrequency(time.Millisecond)

			started := time.Now()
			err = a.CreateVirtualMachineAndWait(context.Background(), linuxRunnerSpec(), "nic-id", spec.VMSizeEphemeralDiskSizeLimits{}, tt.wait)
			if tt.errString != "" {
				require.ErrorContains(t, err, tt.errString)
				return
			}
			require.NoError(t, err)
			assert.Less(t, time.Since(started), 30*time.Second)
		})
	}
}

type deadlineThatTheTestTriggers struct {
	context.Context
	passed chan struct{}
}

func (d deadlineThatTheTestTriggers) Done() <-chan struct{} {
	return d.passed
}

func (d deadlineThatTheTestTriggers) Err() error {
	select {
	case <-d.passed:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestCreateVirtualMachineAndWaitFailsWhenTheDeadlineOfTheCallerPasses(t *testing.T) {
	ctx := deadlineThatTheTestTriggers{Context: context.Background(), passed: make(chan struct{})}
	passCallerDeadlineOnceAzureAcceptedTheVM := func() {
		time.AfterFunc(20*time.Millisecond, func() { close(ctx.passed) })
	}
	server := armcomputefake.VirtualMachinesServer{
		BeginCreateOrUpdate: func(_ context.Context, _ string, _ string, _ armcompute.VirtualMachine, _ *armcompute.VirtualMachinesClientBeginCreateOrUpdateOptions) (resp azfake.PollerResponder[armcompute.VirtualMachinesClientCreateOrUpdateResponse], errResp azfake.ErrorResponder) {
			for range 10000 {
				resp.AddNonTerminalResponse(201, nil)
			}
			resp.SetTerminalResponse(200, armcompute.VirtualMachinesClientCreateOrUpdateResponse{}, nil)
			passCallerDeadlineOnceAzureAcceptedTheVM()
			return resp, errResp
		},
	}
	vmCli, err := armcompute.NewVirtualMachinesClient("fake-id", &azfake.TokenCredential{}, &policy.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Transport: armcomputefake.NewVirtualMachinesServerTransport(&server),
		},
	})
	require.NoError(t, err)
	a := AzureCli{location: "westus2", vmCli: vmCli}
	a.SetPollFrequency(time.Millisecond)

	err = a.CreateVirtualMachineAndWait(ctx, linuxRunnerSpec(), "nic-id", spec.VMSizeEphemeralDiskSizeLimits{}, time.Minute)

	require.ErrorContains(t, err, "VM provisioning failed")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
