# Garm External Provider For Azure

The Azure external provider allows [garm](https://github.com/cloudbase/garm) to create Linux and Windows runners on top of Azure virtual machines.

## Build

Clone the repo:

```bash
git clone https://github.com/cloudbase/garm-provider-azure
```

Build the binary:

```bash
cd garm-provider-azure
go build .
```

Copy the binary on the same system where garm is running, and [point to it in the config](https://github.com/cloudbase/garm/blob/main/doc/config.md#provider-configuration).

## Configure

The config file for this external provider is a simple toml used to configure the azure credentials it needs to spin up virtual machines.

For now, service principals, azure managed identities and workload identities are supported. An example can be found [in the testdata folder](./testdata/config.toml).

```toml
location = "westeurope"
use_ephemeral_storage = true
virtual_network_cidr = "10.0.0.0/16"
use_accelerated_networking = true

[credentials]
subscription_id = "sample_sub_id"

    # The workload identity can be used when other methods are not available
    # or if a more precise selection of the workload on which to apply permissions is needed.
    # This seems to be the latest recommended method from Azure.
    # More information here: https://learn.microsoft.com/en-us/entra/workload-id/workload-identities-overview
    [credentials.workload_identity]
    tenant_id = "sample_tenant_id"
    client_id = "sample_client_id"
    federated_token_file = "/path/to/federated_token_file"

    # The service principal credentials can be used when azure managed identity
    # is not available.
    [credentials.service_principal]
    # you can create a SP using:
    # az ad sp create-for-rbac --scopes /subscriptions/<subscription ID> --role Contributor
    tenant_id = "sample_tenant_id"
    client_id = "sample_client_id"
    client_secret = "super secret client secret"

    # The managed identity token source is always added to the chain of possible authentication
    # sources. The client ID can be overwritten if needed.
    [credentials.managed_identity]
    # The client ID to use. This config value is optional.
    client_id = "sample_client_id"
```

## Creating a pool

After you [add it to garm as an external provider](https://github.com/cloudbase/garm/blob/main/doc/config.md#provider-configuration), you need to create a pool that uses it. Assuming you named your external provider as ```azure``` in the garm config, the following command should create a new pool:

```bash
garm-cli pool create \
    --os-type windows \
    --enabled=true \
    --flavor Standard_F2s \
    --image MicrosoftWindowsServer:WindowsServer:2022-Datacenter:latest \
    --min-idle-runners 1 \
    --repo f0b1c1c8-b605-4560-adb7-79b95e2e470c \
    --tags azure,golang,win2 \
    --provider-name azure
```

This will create a new Windows runner pool for the repo with ID ```f0b1c1c8-b605-4560-adb7-79b95e2e470c``` on azure, using the image ```MicrosoftWindowsServer:WindowsServer:2022-Datacenter:latest``` and VM size ```Standard_F2s```. You can, of course, tweak the values in the above command to suit your needs.

Here an example for a Linux pool (x86-64 architecture):

```bash
garm-cli pool create \
   --enabled=true \
   --flavor Standard_F8s \
   --min-idle-runners 1 --max-runners 8 \
   --image Canonical:0001-com-ubuntu-server-jammy:22_04-lts-gen2:22.04.202206040 \
   --org=a2f1c7c8-b605-4560-adb7-79b95e2e462d \
   --tags=azure,ubuntu,amd64 \
   --provider-name azure
```

And another example for an arm64 (aarch64) based Linux pool:

```bash
garm-cli pool create \
   --enabled=true \
   --os-arch arm64 \
   --flavor Standard_D2ps_v6 \
   --min-idle-runners 0 --max-runners 4 \
   --image Canonical:0001-com-ubuntu-server-jammy:22_04-lts-arm64:22.04.202206040 \
   --org=a2f1c7c8-b605-4560-adb7-79b95e2e462d \
   --tags=azure,ubuntu,arm64 \
   --provider-name azure
```

Always find a recent image to use. For example to see available Debian images, run something like `az vm image list --all --publisher Debian --offer debian-11 --all | less`.

Each VM is created in it's own resource group with it's own virtual network, separate from all other runners.

## Tweaking the provider

Garm supports sending opaque json encoded configs to the IaaS providers it hooks into. This allows the providers to implement some very provider specific functionality that doesn't necessarily translate well to other providers. Features that may exists on Azure, may not exist on AWS or OpenStack and vice versa.

To this end, this provider supports the following extra specs schema:

```json
{
	"$schema": "http://cloudbase.it/garm-provider-azure/schemas/extra_specs#",
	"type": "object",
	"description": "Schema defining supported extra specs for the Garm Azure Provider",
	"properties": {
		"spot": {
			"type": "boolean",
			"description": "Launch the VM as an Azure Spot VM. Defaults to false; eviction deletes the VM."
		},
		"region_selection": {
			"type": "string",
			"enum": ["fixed", "cheapest"],
			"description": "Where a Spot VM is created. fixed uses the configured location. cheapest picks the cheapest usable region."
		},
		"candidate_regions": {
			"type": "array",
			"items": {
				"type": "string"
			},
			"description": "The regions that cheapest region selection may pick. An empty list allows every region that has a Spot price."
		},
		"on_demand_fallback": {
			"type": "boolean",
			"description": "Create a regular VM in the home region when no region can take a Spot VM."
		},
		"allocate_public_ip": {
			"type": "boolean",
			"description": "Allocate a public IP to the VM."
		},
		"confidential": {
			"type": "boolean",
			"description": "The selected virtual machine size is confidential."
		},
		"trustedlaunch": {
			"type": "boolean",
			"description": "The selected virtual machine uses TrustedLaunch."
		},
		"use_ephemeral_storage": {
			"type": "boolean",
			"description": "Use ephemeral storage for the VM."
		},
		"use_accelerated_networking": {
			"type": "boolean",
			"description": "Use accelerated networking for the VM."
		},
		"open_inbound_ports": {
			"type": "object",
			"description": "A map of protocol to list of inbound ports to open.",
			"properties": {
				"Tcp": {
					"type": "array",
					"description": "List of ports to open.",
					"items": {
						"type": "integer",
						"minimum": 1,
						"maximum": 65535
					}
				},
				"Udp": {
					"type": "array",
					"description": "List of ports to open.",
					"items": {
						"type": "integer",
						"minimum": 1,
						"maximum": 65535
					}
				}
			}
		},
		"storage_account_type": {
			"type": "string",
			"description": "Azure storage account type. Default is Standard_LRS."
		},
		"virtual_network_cidr": {
			"type": "string",
			"description": "The CIDR for the virtual network."
		},
		"disk_size_gb": {
			"type": "integer",
			"description": "The size of the root disk in GB. Default is 127 GB."
		},
		"extra_tags": {
			"type": "object",
			"description": "Extra tags that will get added to all VMs spawned in a pool."
		},
		"ssh_public_keys": {
			"type": "array",
			"description": "SSH public keys to add to the admin user on Linux runners.",
			"items": {
				"type": "string"
			}
		},
		"vnet_subnet_id": {
			"type": "string",
			"description": "The ID of the subnet to use for the VM. Must be in the same region as the VM. This is required if disable_isolated_networks is set to true, otherwise it is ignored."
		},
		"disable_updates": {
			"type": "boolean",
			"description": "Disable automatic updates on the VM."
		},
		"enable_boot_debug": {
			"type": "boolean",
			"description": "Enable boot debug on the VM."
		},
		"extra_packages": {
			"type": "array",
			"description": "Extra packages to install on the VM.",
			"items": {
				"type": "string"
			}
		},
		"runner_install_template": {
			"type": "string",
			"description": "This option can be used to override the default runner install template. If used, the caller is responsible for the correctness of the template as well as the suitability of the template for the target OS. Use the extra_context extra spec if your template has variables in it that need to be expanded."
		},
		"extra_context": {
			"type": "object",
			"description": "Extra context that will be passed to the runner_install_template.",
			"additionalProperties": {
				"type": "string"
			}
		},
		"pre_install_scripts": {
			"type": "object",
			"description": "A map of pre-install scripts that will be run before the runner install script. These will run as root and can be used to prep a generic image before we attempt to install the runner. The key of the map is the name of the script as it will be written to disk. The value is a byte array with the contents of the script."
		},
		"disable_isolated_networks": {
			"type": "boolean",
			"description": "Disable network isolation for the VM."
		}
	},
	"additionalProperties": false
}
```

An example extra specs json would look like this:

```json
{
    "allocate_public_ip": true,
    "open_inbound_ports": {
        "Tcp": [22, 80]
    },
    "storage_account_type": "Standard_LRS",
    "disk_size_gb": 200,
    "extra_tags": {
        "my_custom_tag": "some value"
    },
    "ssh_public_keys": [
        "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAACAQC2oT7j/+elHY9U2ibgk2R...."
    ],
    "vnet_subnet_id": "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/myResourceGroup/providers/Microsoft.Network/virtualNetworks/myVnet/subnets/mySubnet",
    "disable_network_isolation": true
}
```

Set `spot` to `true` to request an Azure Spot VM. The provider sets the
eviction policy to `Delete` and `maxPrice` to `-1`, Azure's on-demand price
ceiling. This does not guarantee Spot capacity or a particular price; Azure
may evict the VM when capacity is unavailable. To let the provider choose the
region of each Spot VM by price, see
[Spot VMs in the cheapest region](#spot-vms-in-the-cheapest-region).

Set `allocate_public_ip` to `true` to give the VM a public IP address. The
provider creates a Standard SKU address with static allocation.

To set it on an existing pool, simply run:

```bash
garm-cli pool update --extra-specs='{"allocate_public_ip": true}' <POOL_ID>
```

You can also set a spec when creating a new pool, using the same flag.

Workers in that pool will be created taking into account the specs you set on the pool.

## Spot VMs in the cheapest region

By default, the provider creates every VM in `location`. With `region_selection = "cheapest"`, the provider creates each Spot VM in the region that has the lowest Spot price and can take the VM. Regular VMs stay in `location`. So do Spot VMs in a pool that sets `region_selection` to `fixed`.

A pool can use this mode only if its runners depend on nothing that exists in a single region:

- The provider rejects `cheapest` together with `vnet_subnet_id`, because a subnet exists in one region only.
- The pool image must exist in every region that the provider may select. A compute gallery image exists only in the regions that the gallery replicates it to.

### Settings

Set the defaults in the provider config file:

```toml
location = "westeurope"
region_selection = "cheapest"
state_dir = "/var/lib/garm-provider-azure"
candidate_regions = ["westeurope", "northeurope", "swedencentral"]
on_demand_fallback = false
home_region = "westeurope"
```

| Setting | Default | Meaning |
| --- | --- | --- |
| `region_selection` | `fixed` | `fixed` creates Spot VMs in `location`. `cheapest` selects a region for each Spot VM. |
| `state_dir` | none | A directory that the provider can write to. Required when `region_selection` is `cheapest`, in the config file or in a pool. See [Files in `state_dir`](#files-in-state_dir). |
| `candidate_regions` | empty | The regions that the provider may select. An empty list allows every region that has a Spot price for the VM size. |
| `on_demand_fallback` | `false` | If no region can take a Spot VM, create a regular VM in `home_region`. A regular VM costs more than a Spot VM. |
| `home_region` | the value of `location` | The region of the regular VM that `on_demand_fallback` creates. |
| `provisioning_wait_seconds` | `120` | The longest time that a create waits for Azure to report the provisioning result of a Spot VM in a selected region. |
| `price_cache_ttl_minutes` | `360` | The age at which the provider stops using a cached price ranking and reads the price API again. |
| `region_skip_minutes` | `30` | The time that a region stays out of the selection for a VM size after a create failed there. |

A pool can override `region_selection`, `candidate_regions`, and `on_demand_fallback` in its extra specs:

```json
{
    "spot": true,
    "allocate_public_ip": true,
    "region_selection": "cheapest",
    "candidate_regions": ["westeurope", "swedencentral"]
}
```

Region names are the names that the Azure APIs use, such as `westeurope`. The provider also accepts display names such as `West Europe`.

In addition to the permissions for creating VMs, the identity that the provider uses must be allowed to list the compute usage of a region and to list the compute resource SKUs of the subscription.

### How the provider selects a region

For each Spot VM, the provider does the following:

1. It reads the Spot prices of the VM size from the public [Azure Retail Prices API](https://learn.microsoft.com/en-us/rest/api/cost-management/retail-prices/azure-retail-prices) and ranks the regions, cheapest first. It ranks a Windows pool by Windows prices and a Linux pool by Linux prices.
2. It selects the first region in the ranking that passes all of these checks:
    - The region is in `candidate_regions`, or `candidate_regions` is empty.
    - The region has no skip marker for the VM size that is still valid.
    - The free Spot vCPU quota of the subscription in the region covers the vCPUs of the VM size. Azure reports this quota as `lowPriorityCores`.
    - The region offers the VM size, and the subscription is not restricted from the size in the whole region.
3. It creates the resource group, the network resources, and the VM in the selected region.
4. It waits for Azure to report the provisioning result, for at most `provisioning_wait_seconds`. The wait lets a capacity failure that Azure reports after it accepted the VM fail the create. If the wait ends without a result, the create succeeds. If the VM then never registers, GARM removes the runner after the `runner_bootstrap_timeout` of the pool.
5. If a step after the selection fails, the provider writes a skip marker for the region and VM size, deletes the resource group, and returns the error.

The provider does not try a second region in the same create. The resource group has the name of the runner and belongs to one region. GARM deletes the failed runner and creates a replacement under a new name, and the replacement selects the next region.

If no region passes the checks, the result depends on `on_demand_fallback`:

- With `on_demand_fallback = true`, the provider creates a regular VM in `home_region`.
- With `on_demand_fallback = false`, the provider writes a no-spot record and the create fails.

If the provider cannot judge the regions, the create fails with no fallback and no no-spot record. The provider cannot judge the regions in these cases:

- The price API fails and `state_dir` has no cached ranking for the VM size. If a cached ranking exists, the provider uses it, however old it is.
- The price API has no Spot price for the VM size.
- Every quota or VM size check returned an error, for example because the credentials are not valid. A single region whose check returns an error only counts as a region that did not pass.

### Files in `state_dir`

GARM starts one provider process for each operation and can run several creates at the same time. The provider keeps what it must remember between operations in JSON files in `state_dir`. Each file holds one fact, and each write replaces the whole file with a rename. A reader never sees a partial file.

The provider writes each file as one line of JSON. Times are in UTC. `<os>` is `linux` or `windows`.

| File | Written when | Read by the provider |
| --- | --- | --- |
| `prices-<vm_size>-<os>.json` | The provider reads a ranking from the price API. | Yes. A file older than `price_cache_ttl_minutes` is replaced. |
| `skip-<region>-<vm_size>.json` | A create fails in a selected region. | Yes. The region is skipped for the VM size until `expires_at`. |
| `no-spot-<os>.json` | No region passes the checks and `on_demand_fallback` is `false`. | No. The file is for other tools. |

A price ranking, here formatted for reading:

```json
{
    "vm_size": "Standard_D2ads_v5",
    "os": "linux",
    "fetched_at": "2026-11-24T12:00:00Z",
    "offers": [
        {"region": "westus2", "usd_per_hour": 0.01},
        {"region": "eastus", "usd_per_hour": 0.02}
    ]
}
```

A skip marker. `reason` is the error that the create returned:

```json
{
    "region": "westus2",
    "vm_size": "Standard_D2ads_v5",
    "expires_at": "2026-11-24T12:30:00Z",
    "reason": "failed to create VM: VM provisioning failed: ..."
}
```

A no-spot record. `rejected` lists every region that the provider checked, in price order:

```json
{
    "os": "linux",
    "vm_size": "Standard_D2ads_v5",
    "recorded_at": "2026-11-24T12:00:00Z",
    "recorded_at_unix": 1795521600,
    "rejected": [
        {"region": "westus2", "reason": "skipped until 2026-11-24T12:30:00Z"},
        {"region": "eastus", "reason": "no free Spot vCPU quota"}
    ]
}
```

The provider overwrites a no-spot record and never deletes it, so the file can describe a shortage that has ended. A tool that reads the record must compare `recorded_at_unix`, in seconds since the Unix epoch, with the current time:

```bash
recorded=$(jq -r '.recorded_at_unix' "$STATE_DIR/no-spot-linux.json" 2>/dev/null || echo 0)
if [ $(( $(date +%s) - recorded )) -lt 3600 ]; then
    echo "no Spot region was usable for Linux in the last hour"
fi
```

The provider ignores an expired skip marker and leaves the file in place. You can delete any file in `state_dir` at any time. If you delete a price file, the next create reads the price API. If you delete a skip marker, the region can be selected again.

### Tags

In this mode the provider adds these tags to the resource group and to the VM:

| Tag | Value |
| --- | --- |
| `garm-region` | The region of the VM. |
| `garm-placement` | `spot`, or `on-demand-fallback` for a regular VM that `on_demand_fallback` created. |
| `garm-spot-usd-per-hour` | The Spot price, in USD per hour, that ranked the region. Only on a `spot` placement. This is the list price at the time of the selection, not the amount that Azure bills. |
