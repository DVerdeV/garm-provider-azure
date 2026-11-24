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
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudbase/garm-provider-azure/internal/placement"
	"github.com/cloudbase/garm-provider-azure/internal/placement/placementtest"
)

var start = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

type fixture struct {
	api      *placementtest.PriceAPI
	stateDir string
	state    *placement.State
	now      time.Time
	selector placement.Selector
}

func newFixture(t *testing.T, prices ...placementtest.Price) *fixture {
	t.Helper()
	f := &fixture{
		api:      placementtest.NewPriceAPI(t, prices...),
		stateDir: t.TempDir(),
		now:      start,
	}
	state, err := placement.OpenState(f.stateDir)
	require.NoError(t, err)
	f.state = state
	f.selector = placement.Selector{
		Prices:   placement.PriceSource{BaseURL: f.api.URL},
		State:    state,
		PriceTTL: 6 * time.Hour,
		Now:      func() time.Time { return f.now },
	}
	return f
}

func (f *fixture) stateFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.stateDir)
	require.NoError(t, err)
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func linuxRequest() placement.Request {
	return placement.Request{VMSize: vmSize, OS: placement.Linux, HomeRegion: "westeurope"}
}

func everyRegionUsable(context.Context, string) (string, error) {
	return "", nil
}

func rejecting(reasons map[string]string) placement.RegionCheck {
	return func(_ context.Context, region string) (string, error) {
		return reasons[region], nil
	}
}

func TestSelectPicksTheCheapestUsableRegion(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("eastus", 0.02),
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("swedencentral", 0.03),
	)

	got, err := f.selector.Select(context.Background(), linuxRequest(), everyRegionUsable)
	require.NoError(t, err)
	assert.Equal(t, placement.Spot{Region: "westus2", USDPerHour: 0.01}, got)
}

func TestSelectUsesThePricesOfTheRequestedOS(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("eastus", 0.01),
		placementtest.LinuxSpot("westus2", 0.02),
		placementtest.WindowsSpot("eastus", 0.09),
		placementtest.WindowsSpot("westus2", 0.05),
	)
	request := linuxRequest()
	request.OS = placement.Windows

	got, err := f.selector.Select(context.Background(), request, everyRegionUsable)
	require.NoError(t, err)
	assert.Equal(t, placement.Spot{Region: "westus2", USDPerHour: 0.05}, got)
}

func TestSelectSkipsARegionThatTheCheckRejects(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("eastus", 0.02),
	)

	got, err := f.selector.Select(context.Background(), linuxRequest(), rejecting(map[string]string{
		"westus2": "0 of 2 Spot vCPUs free",
	}))
	require.NoError(t, err)
	assert.Equal(t, placement.Spot{Region: "eastus", USDPerHour: 0.02}, got)
}

func TestSelectOnlyPicksCandidateRegions(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("eastus", 0.02),
		placementtest.LinuxSpot("swedencentral", 0.03),
	)
	request := linuxRequest()
	request.CandidateRegions = []string{"swedencentral", "eastus"}

	got, err := f.selector.Select(context.Background(), request, everyRegionUsable)
	require.NoError(t, err)
	assert.Equal(t, placement.Spot{Region: "eastus", USDPerHour: 0.02}, got)
}

func TestSelectHonoursASkipMarkerUntilItExpires(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("eastus", 0.02),
	)
	require.NoError(t, f.state.SkipRegion("westus2", vmSize, start.Add(30*time.Minute), "AllocationFailed"))

	f.now = start.Add(29 * time.Minute)
	got, err := f.selector.Select(context.Background(), linuxRequest(), everyRegionUsable)
	require.NoError(t, err)
	assert.Equal(t, placement.Spot{Region: "eastus", USDPerHour: 0.02}, got)

	f.now = start.Add(30 * time.Minute)
	got, err = f.selector.Select(context.Background(), linuxRequest(), everyRegionUsable)
	require.NoError(t, err)
	assert.Equal(t, placement.Spot{Region: "westus2", USDPerHour: 0.01}, got)
}

func TestASkipMarkerAppliesToOneVMSizeOnly(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("eastus", 0.02),
	)
	require.NoError(t, f.state.SkipRegion("westus2", "Standard_D4ads_v5", start.Add(30*time.Minute), "AllocationFailed"))

	got, err := f.selector.Select(context.Background(), linuxRequest(), everyRegionUsable)
	require.NoError(t, err)
	assert.Equal(t, placement.Spot{Region: "westus2", USDPerHour: 0.01}, got)
}

func TestSelectRecordsThatNoRegionIsUsable(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("eastus", 0.02),
	)
	require.NoError(t, f.state.SkipRegion("westus2", vmSize, start.Add(30*time.Minute), "AllocationFailed"))

	_, err := f.selector.Select(context.Background(), linuxRequest(), rejecting(map[string]string{
		"eastus": "0 of 2 Spot vCPUs free",
	}))

	var noSpot *placement.NoSpotRegionError
	require.ErrorAs(t, err, &noSpot)
	assert.Equal(t, vmSize, noSpot.VMSize)
	assert.Equal(t, placement.Linux, noSpot.OS)
	assert.Equal(t, []placement.Rejection{
		{Region: "westus2", Reason: "skipped until 2026-11-24T12:30:00Z"},
		{Region: "eastus", Reason: "0 of 2 Spot vCPUs free"},
	}, noSpot.Rejected)

	raw, err := os.ReadFile(filepath.Join(f.stateDir, "no-spot-linux.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"os": "linux",
		"vm_size": "Standard_D2ads_v5",
		"recorded_at": "2026-11-24T12:00:00Z",
		"recorded_at_unix": 1795521600,
		"rejected": [
			{"region": "westus2", "reason": "skipped until 2026-11-24T12:30:00Z"},
			{"region": "eastus", "reason": "0 of 2 Spot vCPUs free"}
		]
	}`, string(raw))
}

func TestSelectRecordsAnEmptyListWhenNoCandidateRegionHasASpotPrice(t *testing.T) {
	f := newFixture(t, placementtest.LinuxSpot("westus2", 0.01))
	request := linuxRequest()
	request.CandidateRegions = []string{"swedencentral"}

	_, err := f.selector.Select(context.Background(), request, everyRegionUsable)

	var noSpot *placement.NoSpotRegionError
	require.ErrorAs(t, err, &noSpot)
	raw, err := os.ReadFile(filepath.Join(f.stateDir, "no-spot-linux.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"os": "linux",
		"vm_size": "Standard_D2ads_v5",
		"recorded_at": "2026-11-24T12:00:00Z",
		"recorded_at_unix": 1795521600,
		"rejected": []
	}`, string(raw))
}

func TestSelectKeepsOneNoSpotRecordPerOS(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("eastus", 0.02),
		placementtest.WindowsSpot("eastus", 0.05),
	)
	nothingUsable := rejecting(map[string]string{"eastus": "0 of 2 Spot vCPUs free"})
	windows := linuxRequest()
	windows.OS = placement.Windows

	_, err := f.selector.Select(context.Background(), linuxRequest(), nothingUsable)
	require.Error(t, err)
	f.now = start.Add(time.Hour)
	_, err = f.selector.Select(context.Background(), windows, nothingUsable)
	require.Error(t, err)

	recordedAt := func(name string) string {
		raw, err := os.ReadFile(filepath.Join(f.stateDir, name))
		require.NoError(t, err)
		var record struct {
			RecordedAt string `json:"recorded_at"`
		}
		require.NoError(t, json.Unmarshal(raw, &record))
		return record.RecordedAt
	}
	assert.Equal(t, "2026-11-24T12:00:00Z", recordedAt("no-spot-linux.json"))
	assert.Equal(t, "2026-11-24T13:00:00Z", recordedAt("no-spot-windows.json"))
}

func TestSelectFallsBackToOnDemandInTheHomeRegion(t *testing.T) {
	f := newFixture(t, placementtest.LinuxSpot("eastus", 0.02))
	request := linuxRequest()
	request.OnDemandFallback = true

	got, err := f.selector.Select(context.Background(), request, rejecting(map[string]string{
		"eastus": "the size is restricted for this subscription",
	}))
	require.NoError(t, err)
	assert.Equal(t, placement.OnDemand{Region: "westeurope"}, got)
	assert.NotContains(t, f.stateFiles(t), "no-spot-linux.json")
}

func TestSelectRecordsNoSpotWhenEveryCheckFails(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("eastus", 0.02),
	)
	request := linuxRequest()
	request.OnDemandFallback = true
	unauthorized := errors.New("AuthorizationFailed")

	got, err := f.selector.Select(context.Background(), request, func(context.Context, string) (string, error) {
		return "", unauthorized
	})

	require.ErrorIs(t, err, unauthorized)
	assert.Nil(t, got)
	var noSpot *placement.NoSpotRegionError
	assert.False(t, errors.As(err, &noSpot))
	raw, err := os.ReadFile(filepath.Join(f.stateDir, "no-spot-linux.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"os": "linux",
		"vm_size": "Standard_D2ads_v5",
		"recorded_at": "2026-11-24T12:00:00Z",
		"recorded_at_unix": 1795521600,
		"rejected": [
			{"region": "", "reason": "failed to check any region: westus2: AuthorizationFailed\neastus: AuthorizationFailed"}
		]
	}`, string(raw))
}

func TestSelectJudgesTheRegionsWhenOnlySomeChecksFail(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("eastus", 0.02),
	)

	_, err := f.selector.Select(context.Background(), linuxRequest(), func(_ context.Context, region string) (string, error) {
		if region == "westus2" {
			return "", errors.New("AuthorizationFailed")
		}
		return "0 of 2 Spot vCPUs free", nil
	})

	var noSpot *placement.NoSpotRegionError
	require.ErrorAs(t, err, &noSpot)
	raw, err := os.ReadFile(filepath.Join(f.stateDir, "no-spot-linux.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"os": "linux",
		"vm_size": "Standard_D2ads_v5",
		"recorded_at": "2026-11-24T12:00:00Z",
		"recorded_at_unix": 1795521600,
		"rejected": [
			{"region": "westus2", "reason": "check failed: AuthorizationFailed"},
			{"region": "eastus", "reason": "0 of 2 Spot vCPUs free"}
		]
	}`, string(raw))
}

func TestSelectTreatsAFailedCheckAsOneUnusableRegion(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("chinanorth3", 0.005),
		placementtest.LinuxSpot("westus2", 0.01),
	)

	got, err := f.selector.Select(context.Background(), linuxRequest(), func(_ context.Context, region string) (string, error) {
		if region == "chinanorth3" {
			return "", errors.New("NoRegisteredProviderFound")
		}
		return "", nil
	})
	require.NoError(t, err)
	assert.Equal(t, placement.Spot{Region: "westus2", USDPerHour: 0.01}, got)
}

func TestSelectRecordsNoSpotWhenThePriceAPIFailsAndNothingIsCached(t *testing.T) {
	f := newFixture(t, placementtest.LinuxSpot("eastus", 0.02))
	f.api.SetStatus(http.StatusServiceUnavailable)
	request := linuxRequest()
	request.OnDemandFallback = true

	got, err := f.selector.Select(context.Background(), request, everyRegionUsable)

	require.ErrorContains(t, err, "503 Service Unavailable")
	assert.Nil(t, got)
	var noSpot *placement.NoSpotRegionError
	assert.False(t, errors.As(err, &noSpot))
	assert.Equal(t, []string{"no-spot-linux.json"}, f.stateFiles(t))
	raw, err := os.ReadFile(filepath.Join(f.stateDir, "no-spot-linux.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"os": "linux",
		"vm_size": "Standard_D2ads_v5",
		"recorded_at": "2026-11-24T12:00:00Z",
		"recorded_at_unix": 1795521600,
		"rejected": [
			{"region": "", "reason": "the price API failed: the price API answered 503 Service Unavailable"}
		]
	}`, string(raw))
}

func TestSelectRecordsNoSpotWhenThePriceAPIHasNoSpotPriceForTheOS(t *testing.T) {
	f := newFixture(t, placementtest.WindowsSpot("eastus", 0.05))
	request := linuxRequest()
	request.OnDemandFallback = true

	got, err := f.selector.Select(context.Background(), request, everyRegionUsable)

	require.ErrorContains(t, err, "no Spot price")
	assert.Nil(t, got)
	assert.NotContains(t, f.stateFiles(t), "no-spot-windows.json")
	raw, err := os.ReadFile(filepath.Join(f.stateDir, "no-spot-linux.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"os": "linux",
		"vm_size": "Standard_D2ads_v5",
		"recorded_at": "2026-11-24T12:00:00Z",
		"recorded_at_unix": 1795521600,
		"rejected": [
			{"region": "", "reason": "the price API has no Spot price for Standard_D2ads_v5 (linux)"}
		]
	}`, string(raw))
}

func TestSelectRecordsNothingWhenTheCreateIsCancelledDuringThePriceFetch(t *testing.T) {
	f := newFixture(t, placementtest.LinuxSpot("eastus", 0.02))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.selector.Select(ctx, linuxRequest(), everyRegionUsable)

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, f.stateFiles(t))
}

func TestSelectReusesCachedPricesUntilTheTTLPasses(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("eastus", 0.02),
	)

	_, err := f.selector.Select(context.Background(), linuxRequest(), everyRegionUsable)
	require.NoError(t, err)
	f.api.SetPrices(
		placementtest.LinuxSpot("westus2", 0.04),
		placementtest.LinuxSpot("eastus", 0.02),
	)

	f.now = start.Add(6*time.Hour - time.Second)
	got, err := f.selector.Select(context.Background(), linuxRequest(), everyRegionUsable)
	require.NoError(t, err)
	assert.Equal(t, placement.Spot{Region: "westus2", USDPerHour: 0.01}, got)
	assert.Len(t, f.api.FirstPageFilters(), 1)

	f.now = start.Add(6 * time.Hour)
	got, err = f.selector.Select(context.Background(), linuxRequest(), everyRegionUsable)
	require.NoError(t, err)
	assert.Equal(t, placement.Spot{Region: "eastus", USDPerHour: 0.02}, got)
	assert.Len(t, f.api.FirstPageFilters(), 2)
}

func TestSelectUsesStalePricesWhenThePriceAPIFails(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("eastus", 0.02),
	)
	_, err := f.selector.Select(context.Background(), linuxRequest(), everyRegionUsable)
	require.NoError(t, err)

	f.api.SetStatus(http.StatusServiceUnavailable)
	f.now = start.Add(48 * time.Hour)
	got, err := f.selector.Select(context.Background(), linuxRequest(), everyRegionUsable)

	require.NoError(t, err)
	assert.Equal(t, placement.Spot{Region: "westus2", USDPerHour: 0.01}, got)
	assert.Len(t, f.api.FirstPageFilters(), 2)
}

func TestThePriceCacheIsOneFilePerVMSizeAndOS(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.WindowsSpot("westus2", 0.05),
	)

	_, err := f.selector.Select(context.Background(), linuxRequest(), everyRegionUsable)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{
		"prices-Standard_D2ads_v5-linux.json",
		"prices-Standard_D2ads_v5-windows.json",
	}, f.stateFiles(t))
	raw, err := os.ReadFile(filepath.Join(f.stateDir, "prices-Standard_D2ads_v5-linux.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"vm_size": "Standard_D2ads_v5",
		"os": "linux",
		"fetched_at": "2026-11-24T12:00:00Z",
		"offers": [{"region": "westus2", "usd_per_hour": 0.01}]
	}`, string(raw))
}

func TestSkipRegionWritesOneMarkerPerRegionAndVMSize(t *testing.T) {
	f := newFixture(t)

	require.NoError(t, f.state.SkipRegion("westus2", vmSize, start.Add(30*time.Minute), "AllocationFailed"))

	raw, err := os.ReadFile(filepath.Join(f.stateDir, "skip-westus2-Standard_D2ads_v5.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"region": "westus2",
		"vm_size": "Standard_D2ads_v5",
		"expires_at": "2026-11-24T12:30:00Z",
		"reason": "AllocationFailed"
	}`, string(raw))
}

func TestSkipRegionRefusesANameThatLeavesTheStateDirectory(t *testing.T) {
	f := newFixture(t)

	require.Error(t, f.state.SkipRegion("../outside", vmSize, start.Add(time.Minute), "AllocationFailed"))
	assert.Empty(t, f.stateFiles(t))
}

func TestConcurrentCreatesNeverSeeAHalfWrittenSkipMarker(t *testing.T) {
	f := newFixture(t,
		placementtest.LinuxSpot("westus2", 0.01),
		placementtest.LinuxSpot("eastus", 0.02),
	)
	require.NoError(t, f.state.SkipRegion("westus2", vmSize, start.Add(30*time.Minute), "AllocationFailed"))
	_, err := f.selector.Select(context.Background(), linuxRequest(), everyRegionUsable)
	require.NoError(t, err)

	var writers sync.WaitGroup
	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		writers.Wait()
	})
	for range 4 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					assert.NoError(t, f.state.SkipRegion("westus2", vmSize, start.Add(30*time.Minute), "AllocationFailed"))
				}
			}
		}()
	}

	for range 300 {
		got, err := f.selector.Select(context.Background(), linuxRequest(), everyRegionUsable)
		require.NoError(t, err)
		require.Equal(t, placement.Spot{Region: "eastus", USDPerHour: 0.02}, got)
	}
}
