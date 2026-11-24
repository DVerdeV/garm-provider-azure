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

package placement_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudbase/garm-provider-azure/internal/placement"
	"github.com/cloudbase/garm-provider-azure/internal/placement/placementtest"
)

const vmSize = "Standard_D2ads_v5"

func TestSpotRankingsAreSplitByOSAndSortedCheapestFirst(t *testing.T) {
	lowPriority := placementtest.LinuxSpot("westus2", 0.001)
	lowPriority.SKUName = "D2ads v5 Low Priority"
	api := placementtest.NewPriceAPI(t,
		placementtest.LinuxSpot("eastus", 0.02),
		placementtest.WindowsSpot("eastus", 0.05),
		placementtest.LinuxSpot("westus2", 0.01),
		lowPriority,
		placementtest.WindowsSpot("westus2", 0.09),
		placementtest.LinuxSpot("eastus", 0.015),
		placementtest.LinuxSpot("", 0.0001),
		placementtest.LinuxSpot("../../etc", 0.0001),
		placementtest.LinuxSpot("swedencentral", 0),
	)
	api.SetPageSize(2)

	rankings, err := placement.PriceSource{BaseURL: api.URL}.SpotRankings(context.Background(), vmSize)
	require.NoError(t, err)

	assert.Equal(t, placement.Ranking{
		{Region: "westus2", USDPerHour: 0.01},
		{Region: "eastus", USDPerHour: 0.015},
	}, rankings[placement.Linux])
	assert.Equal(t, placement.Ranking{
		{Region: "eastus", USDPerHour: 0.05},
		{Region: "westus2", USDPerHour: 0.09},
	}, rankings[placement.Windows])
	assert.Equal(t, []string{
		"serviceName eq 'Virtual Machines' and priceType eq 'Consumption' and armSkuName eq 'Standard_D2ads_v5' and contains(skuName,'Spot')",
	}, api.FirstPageFilters())
}

func TestSpotRankingsFailWhenThePriceAPIFails(t *testing.T) {
	api := placementtest.NewPriceAPI(t, placementtest.LinuxSpot("eastus", 0.02))
	api.SetStatus(http.StatusServiceUnavailable)

	_, err := placement.PriceSource{BaseURL: api.URL}.SpotRankings(context.Background(), vmSize)
	require.Error(t, err)
}

func TestSpotRankingsRejectAVMSizeThatWouldChangeTheFilter(t *testing.T) {
	api := placementtest.NewPriceAPI(t, placementtest.LinuxSpot("eastus", 0.02))

	_, err := placement.PriceSource{BaseURL: api.URL}.SpotRankings(context.Background(), "x' or serviceName ne '")
	require.Error(t, err)
	assert.Empty(t, api.FirstPageFilters())
}

func TestSpotRankingsDoNotFollowALinkToAnotherHost(t *testing.T) {
	var elsewhereCalled bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhereCalled = true
		_, _ = w.Write([]byte(`{"Items":[]}`))
	}))
	defer elsewhere.Close()
	// httptest servers share the host 127.0.0.1, so the other host needs another name.
	elsewhereURL := "http://localhost" + elsewhere.URL[len("http://127.0.0.1"):]
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Items":[],"NextPageLink":"` + elsewhereURL + `"}`))
	}))
	defer api.Close()

	_, err := placement.PriceSource{BaseURL: api.URL}.SpotRankings(context.Background(), vmSize)
	require.Error(t, err)
	assert.False(t, elsewhereCalled)
}
