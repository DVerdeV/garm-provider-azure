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

package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/cloudbase/garm-provider-azure/config"
	"github.com/cloudbase/garm-provider-azure/internal/spec"
	"github.com/cloudbase/garm-provider-azure/internal/util"
)

const (
	// Azure counts Spot vCPUs under the name of the older Low Priority offer.
	spotCoresUsageName = "lowPriorityCores"

	// The SDK default of 30 seconds would use up most of a short wait before the first answer.
	defaultPollFrequency = 5 * time.Second
)

func NewAzCLI(cfg *config.Config) (*AzureCli, error) {
	creds, err := cfg.Credentials.GetCredentials()
	if err != nil {
		return nil, fmt.Errorf("failed to get client: %w", err)
	}

	opts := arm.ClientOptions{
		ClientOptions: cfg.Credentials.ClientOptions,
	}
	resourceGroupClient, err := armresources.NewResourceGroupsClient(cfg.Credentials.SubscriptionID, creds, &opts)
	if err != nil {
		return nil, err
	}
	netCli, err := armnetwork.NewVirtualNetworksClient(cfg.Credentials.SubscriptionID, creds, &opts)
	if err != nil {
		return nil, err
	}

	subnetClient, err := armnetwork.NewSubnetsClient(cfg.Credentials.SubscriptionID, creds, &opts)
	if err != nil {
		return nil, err
	}

	nsgClient, err := armnetwork.NewSecurityGroupsClient(cfg.Credentials.SubscriptionID, creds, &opts)
	if err != nil {
		return nil, err
	}

	nicClient, err := armnetwork.NewInterfacesClient(cfg.Credentials.SubscriptionID, creds, &opts)
	if err != nil {
		return nil, err
	}

	vmClient, err := armcompute.NewVirtualMachinesClient(cfg.Credentials.SubscriptionID, creds, &opts)
	if err != nil {
		return nil, err
	}

	publicIPcli, err := armnetwork.NewPublicIPAddressesClient(cfg.Credentials.SubscriptionID, creds, &opts)
	if err != nil {
		return nil, err
	}

	skuCLI, err := armcompute.NewResourceSKUsClient(cfg.Credentials.SubscriptionID, creds, &opts)
	if err != nil {
		return nil, err
	}

	extClient, err := armcompute.NewVirtualMachineExtensionsClient(cfg.Credentials.SubscriptionID, creds, &opts)
	if err != nil {
		return nil, err
	}

	usageClient, err := armcompute.NewUsageClient(cfg.Credentials.SubscriptionID, creds, &opts)
	if err != nil {
		return nil, err
	}
	azCli := &AzureCli{
		cfg:            cfg,
		cred:           creds,
		rgCli:          resourceGroupClient,
		netCli:         netCli,
		subnetCli:      subnetClient,
		nsgCli:         nsgClient,
		nicCli:         nicClient,
		vmCli:          vmClient,
		pubIPCli:       publicIPcli,
		extCli:         extClient,
		location:       cfg.Location,
		resourceSKUCli: skuCLI,
		usageCli:       usageClient,
	}
	return azCli, nil
}

type AzureCli struct {
	cfg  *config.Config
	cred azcore.TokenCredential

	rgCli          *armresources.ResourceGroupsClient
	netCli         *armnetwork.VirtualNetworksClient
	subnetCli      *armnetwork.SubnetsClient
	nsgCli         *armnetwork.SecurityGroupsClient
	nicCli         *armnetwork.InterfacesClient
	vmCli          *armcompute.VirtualMachinesClient
	pubIPCli       *armnetwork.PublicIPAddressesClient
	extCli         *armcompute.VirtualMachineExtensionsClient
	resourceSKUCli *armcompute.ResourceSKUsClient
	usageCli       *armcompute.UsageClient

	location      string
	pollFrequency time.Duration
}

func (a *AzureCli) Config() *config.Config {
	return a.cfg
}

func (a *AzureCli) SetConfig(cfg *config.Config) {
	a.cfg = cfg
}

func (a *AzureCli) Location() string {
	return a.location
}

func (a *AzureCli) SetLocation(location string) {
	a.location = location
}

func (a *AzureCli) Clients() (rgCli *armresources.ResourceGroupsClient, netCli *armnetwork.VirtualNetworksClient, subnetCli *armnetwork.SubnetsClient, nsgCli *armnetwork.SecurityGroupsClient, nicCli *armnetwork.InterfacesClient, vmCli *armcompute.VirtualMachinesClient, pubIPCli *armnetwork.PublicIPAddressesClient, extCli *armcompute.VirtualMachineExtensionsClient, skuCli *armcompute.ResourceSKUsClient) {
	return a.rgCli, a.netCli, a.subnetCli, a.nsgCli, a.nicCli, a.vmCli, a.pubIPCli, a.extCli, a.resourceSKUCli
}

func (a *AzureCli) SetClients(rgCli *armresources.ResourceGroupsClient, netCli *armnetwork.VirtualNetworksClient, subnetCli *armnetwork.SubnetsClient, nsgCli *armnetwork.SecurityGroupsClient, nicCli *armnetwork.InterfacesClient, vmCli *armcompute.VirtualMachinesClient, pubIPCli *armnetwork.PublicIPAddressesClient, extCli *armcompute.VirtualMachineExtensionsClient, skuCli *armcompute.ResourceSKUsClient) {
	a.rgCli = rgCli
	a.netCli = netCli
	a.subnetCli = subnetCli
	a.nsgCli = nsgCli
	a.nicCli = nicCli
	a.vmCli = vmCli
	a.pubIPCli = pubIPCli
	a.extCli = extCli
	a.resourceSKUCli = skuCli
}

func (a *AzureCli) SetUsageClient(usageCli *armcompute.UsageClient) {
	a.usageCli = usageCli
}

func (a *AzureCli) SetPollFrequency(frequency time.Duration) {
	a.pollFrequency = frequency
}

func (a *AzureCli) CreateResourceGroup(ctx context.Context, name string, tags map[string]*string) (*armresources.ResourceGroup, error) {
	parameters := armresources.ResourceGroup{
		Location: to.Ptr(a.location),
		Tags:     tags,
	}

	resp, err := a.rgCli.CreateOrUpdate(ctx, name, parameters, nil)
	if err != nil {
		return nil, err
	}

	return &resp.ResourceGroup, nil
}

func (a *AzureCli) CreateVirtualNetwork(ctx context.Context, baseName, spaceCIDR string) (*armnetwork.VirtualNetwork, error) {
	parameters := armnetwork.VirtualNetwork{
		Location: to.Ptr(a.location),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{
				AddressPrefixes: []*string{
					to.Ptr(spaceCIDR),
				},
			},
		},
	}

	pollerResponse, err := a.netCli.BeginCreateOrUpdate(ctx, baseName, baseName, parameters, nil)
	if err != nil {
		return nil, err
	}

	resp, err := pollerResponse.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}

	return &resp.VirtualNetwork, nil
}

func (a *AzureCli) CreateSubnet(ctx context.Context, baseName, subnetCIDR string) (*armnetwork.Subnet, error) {
	parameters := armnetwork.Subnet{
		Properties: &armnetwork.SubnetPropertiesFormat{
			AddressPrefix: to.Ptr(subnetCIDR),
		},
	}

	pollerResponse, err := a.subnetCli.BeginCreateOrUpdate(ctx, baseName, baseName, baseName, parameters, nil)
	if err != nil {
		return nil, err
	}

	resp, err := pollerResponse.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}

	return &resp.Subnet, nil
}

func (a *AzureCli) CreateNetworkSecurityGroup(ctx context.Context, baseName string, spec *spec.RunnerSpec) (*armnetwork.SecurityGroup, error) {
	if spec == nil {
		return nil, fmt.Errorf("invalid nil runner spec")
	}

	rules := spec.SecurityRules()
	parameters := armnetwork.SecurityGroup{
		Location: to.Ptr(a.location),
		Properties: &armnetwork.SecurityGroupPropertiesFormat{
			SecurityRules: rules,
		},
	}

	pollerResponse, err := a.nsgCli.BeginCreateOrUpdate(ctx, baseName, baseName, parameters, nil)
	if err != nil {
		return nil, err
	}

	resp, err := pollerResponse.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &resp.SecurityGroup, nil
}

func (a *AzureCli) CreateNetWorkInterface(ctx context.Context, baseName, subnetID, networkSecurityGroupID, publicIPID string, acceletatedNetworking bool) (*armnetwork.Interface, error) {
	interfaceIPConfig := &armnetwork.InterfaceIPConfigurationPropertiesFormat{
		PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
		Subnet: &armnetwork.Subnet{
			ID: to.Ptr(subnetID),
		},
	}

	if publicIPID != "" {
		interfaceIPConfig.PublicIPAddress = &armnetwork.PublicIPAddress{
			ID: to.Ptr(publicIPID),
		}
	}

	parameters := armnetwork.Interface{
		Location: to.Ptr(a.location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			EnableAcceleratedNetworking: to.Ptr(acceletatedNetworking),
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
				{
					Name:       to.Ptr("ipConfig"),
					Properties: interfaceIPConfig,
				},
			},
			NetworkSecurityGroup: &armnetwork.SecurityGroup{
				ID: to.Ptr(networkSecurityGroupID),
			},
		},
	}

	pollerResponse, err := a.nicCli.BeginCreateOrUpdate(ctx, baseName, baseName, parameters, nil)
	if err != nil {
		return nil, err
	}

	resp, err := pollerResponse.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}

	return &resp.Interface, err
}

func (a *AzureCli) CreatePublicIP(ctx context.Context, baseName string) (*armnetwork.PublicIPAddress, error) {
	parameters := armnetwork.PublicIPAddress{
		Location: to.Ptr(a.location),
		// Azure retired Basic public IPs, so the SKU is not left to the service default.
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
		},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
	}

	pollerResponse, err := a.pubIPCli.BeginCreateOrUpdate(ctx, baseName, baseName, parameters, nil)
	if err != nil {
		return nil, err
	}

	resp, err := pollerResponse.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &resp.PublicIPAddress, err
}

func (a *AzureCli) CreateVirtualMachine(ctx context.Context, spec *spec.RunnerSpec, networkInterfaceID string, sizeSpec spec.VMSizeEphemeralDiskSizeLimits) error {
	return a.CreateVirtualMachineAndWait(ctx, spec, networkInterfaceID, sizeSpec, 0)
}

func (a *AzureCli) CreateVirtualMachineAndWait(ctx context.Context, spec *spec.RunnerSpec, networkInterfaceID string, sizeSpec spec.VMSizeEphemeralDiskSizeLimits, wait time.Duration) error {
	if spec == nil {
		return fmt.Errorf("invalid nil runner spec")
	}

	properties, err := spec.GetNewVMProperties(networkInterfaceID, sizeSpec)
	if err != nil {
		return fmt.Errorf("failed to get new VM properties: %w", err)
	}
	parameters := armcompute.VirtualMachine{
		Location: to.Ptr(a.location),
		Tags:     spec.Tags,
		Identity: &armcompute.VirtualMachineIdentity{
			Type: to.Ptr(armcompute.ResourceIdentityTypeNone),
		},
		Properties: properties,
	}

	poller, err := a.vmCli.BeginCreateOrUpdate(ctx, spec.BootstrapParams.Name, spec.BootstrapParams.Name, parameters, nil)
	if err != nil {
		return fmt.Errorf("failed to create VM: %w", err)
	}

	extName := "CustomScriptExtension"
	computeExtension, err := spec.GetVMExtension(a.location, extName)
	if err != nil {
		return fmt.Errorf("failed to get vm extension: %w", err)
	}

	if computeExtension != nil {
		_, err = a.extCli.BeginCreateOrUpdate(ctx, spec.BootstrapParams.Name, spec.BootstrapParams.Name, extName, *computeExtension, nil)
		if err != nil {
			return fmt.Errorf("failed to create vm extension: %w", err)
		}
	}

	if wait <= 0 {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	frequency := a.pollFrequency
	if frequency == 0 {
		frequency = defaultPollFrequency
	}
	_, err = poller.PollUntilDone(waitCtx, &runtime.PollUntilDoneOptions{Frequency: frequency})
	waitEndedWithoutResult := errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
	if err == nil || waitEndedWithoutResult {
		return nil
	}
	return fmt.Errorf("VM provisioning failed: %w", err)
}

func (a *AzureCli) FreeSpotCores(ctx context.Context, location string) (int64, error) {
	pager := a.usageCli.NewListPager(location, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to list compute usage in %s: %w", location, err)
		}
		for _, usage := range page.Value {
			if usage == nil || usage.Name == nil || usage.Name.Value == nil || *usage.Name.Value != spotCoresUsageName {
				continue
			}
			if usage.Limit == nil || usage.CurrentValue == nil {
				return 0, fmt.Errorf("compute usage in %s has an incomplete %s entry", location, spotCoresUsageName)
			}
			return *usage.Limit - int64(*usage.CurrentValue), nil
		}
	}
	return 0, fmt.Errorf("compute usage in %s has no %s entry", location, spotCoresUsageName)
}

type VMSizeInLocation struct {
	VCPUs              int64
	LocationRestricted bool
}

func (a *AzureCli) GetVMSizeInLocation(ctx context.Context, location, vmSize string) (size VMSizeInLocation, offered bool, err error) {
	opts := &armcompute.ResourceSKUsClientListOptions{
		Filter: to.Ptr(fmt.Sprintf("location eq '%s'", location)),
	}
	pager := a.resourceSKUCli.NewListPager(opts)
	for pager.More() {
		page, listErr := pager.NextPage(ctx)
		if listErr != nil {
			return VMSizeInLocation{}, false, fmt.Errorf("failed to list VM sizes in %s: %w", location, listErr)
		}
		for _, sku := range page.Value {
			if sku == nil || sku.ResourceType == nil || sku.Name == nil {
				continue
			}
			if *sku.ResourceType != "virtualMachines" || !strings.EqualFold(*sku.Name, vmSize) {
				continue
			}

			for _, capability := range sku.Capabilities {
				if capability == nil || capability.Name == nil || capability.Value == nil || *capability.Name != "vCPUs" {
					continue
				}
				size.VCPUs, err = strconv.ParseInt(*capability.Value, 10, 64)
				if err != nil {
					return VMSizeInLocation{}, false, fmt.Errorf("failed to parse the vCPU count of %s: %w", vmSize, err)
				}
			}
			if size.VCPUs <= 0 {
				return VMSizeInLocation{}, false, fmt.Errorf("%s reports no vCPU count in %s", vmSize, location)
			}
			for _, restriction := range sku.Restrictions {
				if restriction != nil && restriction.Type != nil && *restriction.Type == armcompute.ResourceSKURestrictionsTypeLocation {
					size.LocationRestricted = true
				}
			}
			return size, true, nil
		}
	}
	return VMSizeInLocation{}, false, nil
}

func (a *AzureCli) GetMaxEphemeralDiskSize(ctx context.Context, vmSize string) (spec.VMSizeEphemeralDiskSizeLimits, error) {
	opts := &armcompute.ResourceSKUsClientListOptions{
		Filter: to.Ptr(fmt.Sprintf("location eq '%s'", a.location)),
	}
	pager := a.resourceSKUCli.NewListPager(opts)
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return spec.VMSizeEphemeralDiskSizeLimits{}, fmt.Errorf("failed to get VM size details: %w", err)
		}
		if resp.ResourceSKUsResult.Value != nil {
			for _, val := range resp.ResourceSKUsResult.Value {
				if *val.ResourceType == "virtualMachines" && *val.Name == vmSize {
					var ephemeralSupported string
					var cacheBytes string
					var resourceDiskMB string
					for _, capability := range val.Capabilities {
						switch *capability.Name {
						case "EphemeralOSDiskSupported":
							ephemeralSupported = *capability.Value
						case "CachedDiskBytes":
							cacheBytes = *capability.Value
						case "MaxResourceVolumeMB":
							resourceDiskMB = *capability.Value
						}
					}
					var res spec.VMSizeEphemeralDiskSizeLimits
					if ephemeralSupported == "True" {
						if cacheBytes != "" {
							asInt64, err := strconv.ParseInt(cacheBytes, 10, 64)
							if err != nil {
								return spec.VMSizeEphemeralDiskSizeLimits{}, fmt.Errorf("failed to parse cache bytes: %w", err)
							}
							inGB := asInt64 / 1024 / 1024 / 1024
							res.CacheDiskSizeGB = int32(inGB)
						}

						if resourceDiskMB != "" {
							asInt64, err := strconv.ParseInt(resourceDiskMB, 10, 64)
							if err != nil {
								return spec.VMSizeEphemeralDiskSizeLimits{}, fmt.Errorf("failed to parse resource disk MB: %w", err)
							}
							inGB := asInt64 / 1024
							res.ResourceDiskSizeGB = int32(inGB)
						}
						if res.CacheDiskSizeGB != 0 || res.ResourceDiskSizeGB != 0 {
							return res, nil
						}
					}
				}
			}
		}
	}
	return spec.VMSizeEphemeralDiskSizeLimits{}, fmt.Errorf("failed to get VM size details for %s", vmSize)
}

func (a *AzureCli) DeleteResourceGroup(ctx context.Context, resourceGroup string, forceDelete bool) error {
	opts := &armresources.ResourceGroupsClientBeginDeleteOptions{}
	if forceDelete {
		opts.ForceDeletionTypes = to.Ptr("Microsoft.Compute/virtualMachines,Microsoft.Compute/virtualMachineScaleSets")
	}

	pollerResponse, err := a.rgCli.BeginDelete(ctx, resourceGroup, opts)
	if err != nil {
		asRespCode, ok := err.(*azcore.ResponseError)
		if ok {
			if asRespCode.StatusCode == http.StatusNotFound {
				return nil
			}
			// We may not have a VM created yet, so force delete will fail. Retry without force delete.
			if asRespCode.ErrorCode == "UnsupportedForceDeletionResourceTypeInQueryString" {
				return a.DeleteResourceGroup(ctx, resourceGroup, false)
			}
		}
		return fmt.Errorf("failed to delete resource group: %w", err)
	}

	_, err = pollerResponse.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to delete resource group: %w", err)
	}

	return nil
}

func (a *AzureCli) GetInstance(ctx context.Context, vmName string) (armcompute.VirtualMachine, error) {
	opts := &armcompute.VirtualMachinesClientGetOptions{
		Expand: to.Ptr(armcompute.InstanceViewTypesInstanceView),
	}
	vm, err := a.vmCli.Get(ctx, vmName, vmName, opts)
	if err != nil {
		return armcompute.VirtualMachine{}, fmt.Errorf("failed to get VM: %w", err)
	}
	return vm.VirtualMachine, nil
}

func (a *AzureCli) DealocateVM(ctx context.Context, vmName string) error {
	poller, err := a.vmCli.BeginDeallocate(ctx, vmName, vmName, nil)
	if err != nil {
		return fmt.Errorf("failed to dealocate VM: %w", err)
	}
	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to dealocate vm: %w", err)
	}
	return nil
}

func (a *AzureCli) StartVM(ctx context.Context, vmName string) error {
	poller, err := a.vmCli.BeginStart(ctx, vmName, vmName, nil)
	if err != nil {
		return fmt.Errorf("failed to start VM: %w", err)
	}

	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("failed to start VM: %w", err)
	}

	return nil
}

func (a *AzureCli) ListVirtualMachines(ctx context.Context, poolID string) ([]*armcompute.VirtualMachine, error) {
	options := &armcompute.VirtualMachinesClientListAllOptions{}
	var resp []*armcompute.VirtualMachine
	pager := a.vmCli.NewListAllPager(options)
	for pager.More() {
		nextResult, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list virtual machines: %w", err)
		}
		if nextResult.VirtualMachineListResult.Value != nil {
			for _, vm := range nextResult.VirtualMachineListResult.Value {
				// Sadly, there is no server side filter by tags on this resource.
				if vm.Tags == nil {
					continue
				}
				tag, ok := vm.Tags[util.PoolIDTagName]
				if !ok || *tag != poolID {
					continue
				}
				resp = append(resp, vm)
			}
		}
	}
	return resp, nil
}
