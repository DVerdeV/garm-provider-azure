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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm/policy"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v5"
	armnetworkfake "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v5/fake"
	"github.com/stretchr/testify/require"
)

func TestCreatePublicIPRequestsStandardStaticAddress(t *testing.T) {
	var sent armnetwork.PublicIPAddress
	server := armnetworkfake.PublicIPAddressesServer{
		BeginCreateOrUpdate: func(_ context.Context, _ string, _ string, parameters armnetwork.PublicIPAddress, _ *armnetwork.PublicIPAddressesClientBeginCreateOrUpdateOptions) (resp azfake.PollerResponder[armnetwork.PublicIPAddressesClientCreateOrUpdateResponse], errResp azfake.ErrorResponder) {
			sent = parameters
			resp.SetTerminalResponse(200, armnetwork.PublicIPAddressesClientCreateOrUpdateResponse{}, nil)
			return resp, errResp
		},
	}
	pubIPCli, err := armnetwork.NewPublicIPAddressesClient("fake-id", &azfake.TokenCredential{}, &policy.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Transport: armnetworkfake.NewPublicIPAddressesServerTransport(&server),
		},
	})
	require.NoError(t, err)
	a := AzureCli{pubIPCli: pubIPCli, location: "eastus"}

	_, err = a.CreatePublicIP(context.Background(), "test")
	require.NoError(t, err)

	require.NotNil(t, sent.SKU)
	require.NotNil(t, sent.SKU.Name)
	require.Equal(t, armnetwork.PublicIPAddressSKUNameStandard, *sent.SKU.Name)
	require.NotNil(t, sent.Properties)
	require.NotNil(t, sent.Properties.PublicIPAllocationMethod)
	require.Equal(t, armnetwork.IPAllocationMethodStatic, *sent.Properties.PublicIPAllocationMethod)
}
