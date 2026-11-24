// Copyright 2023 Cloudbase Solutions SRL
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
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"

	"github.com/cloudbase/garm-provider-azure/config"
	"github.com/cloudbase/garm-provider-azure/internal/client"
	"github.com/cloudbase/garm-provider-azure/internal/placement"
	"github.com/cloudbase/garm-provider-azure/internal/spec"
	"github.com/cloudbase/garm-provider-azure/internal/util"

	execution "github.com/cloudbase/garm-provider-common/execution/v0.1.0"
	"github.com/cloudbase/garm-provider-common/params"
)

var _ execution.ExternalProvider = &azureProvider{}

var Version = "v0.0.0-unknown"

func NewAzureProvider(configPath, controllerID string) (execution.ExternalProvider, error) {
	conf, err := config.NewConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("error loading config: %w", err)
	}
	azCli, err := client.NewAzCLI(conf)
	if err != nil {
		return nil, fmt.Errorf("failed to get azure CLI: %w", err)
	}
	return &azureProvider{
		controllerID: controllerID,
		azCli:        azCli,
		cfg:          conf,
	}, nil
}

type azureProvider struct {
	controllerID string
	azCli        *client.AzureCli
	cfg          *config.Config
	prices       placement.PriceSource
}

// CreateInstance creates a new compute instance in the provider.
func (a *azureProvider) CreateInstance(ctx context.Context, bootstrapParams params.BootstrapInstance) (_ params.ProviderInstance, err error) {
	runnerSpec, err := spec.GetRunnerSpecFromBootstrapParams(bootstrapParams, a.controllerID, a.cfg)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to generate spec: %w", err)
	}

	imgDetails, err := runnerSpec.ImageDetails()
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to get image details: %w", err)
	}

	spotRegion, err := a.applyRegionSelection(ctx, runnerSpec)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to select a region: %w", err)
	}
	resourceGroupCreated := false
	defer func() {
		if err == nil {
			return
		}

		// The skip marker goes first. The rollback waits until Azure has deleted
		// the resource group, and concurrent creates must avoid the region in the meantime.
		if spotRegion != nil {
			if skipErr := spotRegion.skip(err); skipErr != nil {
				err = errors.Join(err, fmt.Errorf("failed to skip region %s: %w", spotRegion.name, skipErr))
			}
		}
		if !resourceGroupCreated {
			return
		}
		if cleanupErr := a.azCli.DeleteResourceGroup(ctx, runnerSpec.BootstrapParams.Name, true); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("failed to clean up resource group: %w", cleanupErr))
		}
	}()

	var sizeSpec spec.VMSizeEphemeralDiskSizeLimits
	if runnerSpec.UseEphemeralStorage {
		sizeSpec, err = a.azCli.GetMaxEphemeralDiskSize(ctx, runnerSpec.VMSize)
		if err != nil {
			return params.ProviderInstance{}, fmt.Errorf("failed to get max ephemeral disk size: %w", err)
		}

		diskSize := sizeSpec.CacheDiskSizeGB
		if diskSize == 0 {
			diskSize = sizeSpec.ResourceDiskSizeGB
		}

		// If confidential VMs are used with ephemeral storage, 1 GB is reserved.
		// See: https://learn.microsoft.com/en-us/azure/virtual-machines/ephemeral-os-disks#confidential-vms-using-ephemeral-os-disks
		// However, we disable confidential VMs for now, when ephemeral storage is used. We'll leave this recalculation of available
		// space, in case we enable it in the future.
		if runnerSpec.Confidential {
			diskSize = diskSize - 1
		}

		if diskSize < runnerSpec.DiskSizeGB {
			return params.ProviderInstance{}, fmt.Errorf("maximul ephemeral disk size for %s is %d GB (requested %d)", runnerSpec.VMSize, diskSize, runnerSpec.DiskSizeGB)
		}
	}

	_, err = a.azCli.CreateResourceGroup(ctx, runnerSpec.BootstrapParams.Name, runnerSpec.Tags)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to create resource group: %w", err)
	}
	resourceGroupCreated = true

	var pubIPID string
	var pubIP string
	if runnerSpec.AllocatePublicIP {
		publicIP, err := a.azCli.CreatePublicIP(ctx, runnerSpec.BootstrapParams.Name)
		if err != nil {
			return params.ProviderInstance{}, fmt.Errorf("failed to create public IP: %w", err)
		}
		if publicIP.Properties != nil && publicIP.Properties.IPAddress != nil {
			pubIP = *publicIP.Properties.IPAddress
		}
		pubIPID = *publicIP.ID
	}

	nsg, err := a.azCli.CreateNetworkSecurityGroup(ctx, runnerSpec.BootstrapParams.Name, runnerSpec)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to create network security group: %w", err)
	}

	subnetID, err := a.resolveSubnetID(ctx, runnerSpec)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to resolve subnet ID: %w", err)
	}

	nic, err := a.azCli.CreateNetWorkInterface(ctx, runnerSpec.BootstrapParams.Name, subnetID, *nsg.ID, pubIPID, runnerSpec.UseAcceleratedNetworking)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to create NIC: %w", err)
	}

	var provisioningWait time.Duration
	if spotRegion != nil {
		provisioningWait = a.cfg.ProvisioningWait()
	}
	err = a.azCli.CreateVirtualMachineAndWait(ctx, runnerSpec, *nic.ID, sizeSpec, provisioningWait)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to create VM: %w", err)
	}

	// We're lying here. It takes longer for the client to finish polling than for the VM to
	// start running the userdata. Just return that the instance is running once the request
	// to create it goes through.
	instance := params.ProviderInstance{
		ProviderID: runnerSpec.BootstrapParams.Name,
		Name:       runnerSpec.BootstrapParams.Name,
		OSType:     runnerSpec.BootstrapParams.OSType,
		OSArch:     runnerSpec.BootstrapParams.OSArch,
		OSName:     imgDetails.SKU,
		OSVersion:  imgDetails.Version,
		Status:     "running",
	}

	if pubIP != "" {
		instance.Addresses = append(instance.Addresses, params.Address{
			Address: pubIP,
			Type:    params.PublicAddress,
		})
	}
	return instance, nil
}

type selectedSpotRegion struct {
	name    string
	vmSize  string
	state   *placement.State
	skipFor time.Duration
}

func (s *selectedSpotRegion) skip(cause error) error {
	return s.state.SkipRegion(s.name, s.vmSize, time.Now().Add(s.skipFor), cause.Error())
}

func (a *azureProvider) applyRegionSelection(ctx context.Context, runnerSpec *spec.RunnerSpec) (*selectedSpotRegion, error) {
	cheapest := runnerSpec.CheapestRegion
	if cheapest == nil {
		return nil, nil
	}

	state, err := placement.OpenState(a.cfg.StateDir)
	if err != nil {
		return nil, err
	}
	osType := placement.Linux
	if runnerSpec.BootstrapParams.OSType == params.Windows {
		osType = placement.Windows
	}
	selector := placement.Selector{Prices: a.prices, State: state, PriceTTL: a.cfg.PriceCacheTTL()}
	placed, err := selector.Select(ctx, placement.Request{
		VMSize:           runnerSpec.VMSize,
		OS:               osType,
		CandidateRegions: cheapest.CandidateRegions,
		OnDemandFallback: cheapest.OnDemandFallback,
		HomeRegion:       cheapest.HomeRegion,
	}, a.spotRegionCheck(runnerSpec.VMSize))
	if err != nil {
		return nil, err
	}

	switch placed := placed.(type) {
	case placement.Spot:
		a.azCli.SetLocation(placed.Region)
		runnerSpec.Tags[util.RegionTagName] = to.Ptr(placed.Region)
		runnerSpec.Tags[util.PlacementTagName] = to.Ptr(util.PlacementSpot)
		runnerSpec.Tags[util.SpotPriceTagName] = to.Ptr(strconv.FormatFloat(placed.USDPerHour, 'f', -1, 64))
		return &selectedSpotRegion{name: placed.Region, vmSize: runnerSpec.VMSize, state: state, skipFor: a.cfg.RegionSkip()}, nil
	case placement.OnDemand:
		a.azCli.SetLocation(placed.Region)
		runnerSpec.Spot = false
		runnerSpec.Tags[util.RegionTagName] = to.Ptr(placed.Region)
		runnerSpec.Tags[util.PlacementTagName] = to.Ptr(util.PlacementOnDemand)
		return nil, nil
	}
	return nil, fmt.Errorf("unknown placement %T", placed)
}

func (a *azureProvider) spotRegionCheck(vmSize string) placement.RegionCheck {
	return func(ctx context.Context, region string) (string, error) {
		free, err := a.azCli.FreeSpotCores(ctx, region)
		if err != nil {
			return "", err
		}
		if free <= 0 {
			return "no free Spot vCPU quota", nil
		}

		size, offered, err := a.azCli.GetVMSizeInLocation(ctx, region, vmSize)
		if err != nil {
			return "", err
		}
		switch {
		case !offered:
			return "the VM size is not offered", nil
		case size.LocationRestricted:
			return "the VM size is restricted for this subscription", nil
		case size.VCPUs > free:
			return fmt.Sprintf("%d Spot vCPUs free, %d needed", free, size.VCPUs), nil
		}
		return "", nil
	}
}

func (a *azureProvider) resolveSubnetID(ctx context.Context, runnerSpec *spec.RunnerSpec) (string, error) {
	if runnerSpec.VnetSubnetID != "" && runnerSpec.DisableIsolatedNetworks {
		return runnerSpec.VnetSubnetID, nil
	}

	_, err := a.azCli.CreateVirtualNetwork(ctx, runnerSpec.BootstrapParams.Name, runnerSpec.VirtualNetworkCIDR)
	if err != nil {
		return "", fmt.Errorf("failed to create virtual network: %w", err)
	}

	subnet, err := a.azCli.CreateSubnet(ctx, runnerSpec.BootstrapParams.Name, runnerSpec.VirtualNetworkCIDR)
	if err != nil {
		return "", fmt.Errorf("failed to create subnet: %w", err)
	}

	return *subnet.ID, nil
}

// Delete instance will delete the instance in a provider.
func (a *azureProvider) DeleteInstance(ctx context.Context, instance string) error {
	err := a.azCli.DeleteResourceGroup(ctx, instance, true)
	if err != nil {
		return fmt.Errorf("failed to delete instance: %w", err)
	}
	return nil
}

// GetInstance will return details about one instance.
func (a *azureProvider) GetInstance(ctx context.Context, instance string) (params.ProviderInstance, error) {
	vm, err := a.azCli.GetInstance(ctx, instance)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to get VM details: %w", err)
	}
	details, err := util.AzureInstanceToParamsInstance(vm)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to convert VM details: %w", err)
	}
	return details, nil
}

// ListInstances will list all instances for a provider.
func (a *azureProvider) ListInstances(ctx context.Context, poolID string) ([]params.ProviderInstance, error) {
	instances, err := a.azCli.ListVirtualMachines(ctx, poolID)
	if err != nil {
		return nil, fmt.Errorf("failed to list instances: %w", err)
	}

	if instances == nil {
		return []params.ProviderInstance{}, nil
	}

	resp := make([]params.ProviderInstance, len(instances))
	for idx, val := range instances {
		if val == nil {
			return nil, fmt.Errorf("nil vm object in response")
		}
		details, err := util.AzureInstanceToParamsInstance(*val)
		if err != nil {
			return nil, fmt.Errorf("failed to convert VM details: %w", err)
		}
		resp[idx] = details
	}
	return resp, nil
}

// RemoveAllInstances will remove all instances created by this provider.
func (a *azureProvider) RemoveAllInstances(ctx context.Context) error {
	return nil
}

// Stop shuts down the instance.
func (a *azureProvider) Stop(ctx context.Context, instance string, force bool) error {
	return a.azCli.DealocateVM(ctx, instance)
}

// Start boots up an instance.
func (a *azureProvider) Start(ctx context.Context, instance string) error {
	return a.azCli.StartVM(ctx, instance)
}

// GetVersion returns the version of the provider.
func (a *azureProvider) GetVersion(ctx context.Context) string {
	return Version
}
