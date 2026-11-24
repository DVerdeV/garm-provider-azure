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

package placement

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Placement interface {
	placement()
}

type Spot struct {
	Region     string
	USDPerHour float64
}

type OnDemand struct {
	Region string
}

func (Spot) placement()     {}
func (OnDemand) placement() {}

type Rejection struct {
	Region string `json:"region"`
	Reason string `json:"reason"`
}

type NoSpotRegionError struct {
	VMSize   string
	OS       OS
	Rejected []Rejection
}

func (e *NoSpotRegionError) Error() string {
	if len(e.Rejected) == 0 {
		return fmt.Sprintf("no usable Spot region for %s (%s): no candidate region has a Spot price", e.VMSize, e.OS)
	}
	reasons := make([]string, len(e.Rejected))
	for i, rejection := range e.Rejected {
		reasons[i] = rejection.Region + ": " + rejection.Reason
	}
	return fmt.Sprintf("no usable Spot region for %s (%s): %s", e.VMSize, e.OS, strings.Join(reasons, "; "))
}

type RegionCheck func(ctx context.Context, region string) (unusableBecause string, err error)

type Request struct {
	VMSize           string
	OS               OS
	CandidateRegions []string
	OnDemandFallback bool
	HomeRegion       string
}

type Selector struct {
	Prices   PriceSource
	State    *State
	PriceTTL time.Duration
	Now      func() time.Time
}

func (s Selector) Select(ctx context.Context, req Request, check RegionCheck) (Placement, error) {
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}

	ranking, err := s.ranking(ctx, req.VMSize, req.OS, now)
	if err != nil {
		return nil, fmt.Errorf("failed to rank regions by Spot price: %w", err)
	}

	rejected := []Rejection{}
	var checkErrs []error
	for _, offer := range ranking.within(req.CandidateRegions) {
		if until, skipped := s.State.skippedUntil(offer.Region, req.VMSize, now); skipped {
			rejected = append(rejected, Rejection{Region: offer.Region, Reason: "skipped until " + until.Format(time.RFC3339)})
			continue
		}
		unusableBecause, err := check(ctx, offer.Region)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			checkErrs = append(checkErrs, fmt.Errorf("%s: %w", offer.Region, err))
			unusableBecause = "check failed: " + err.Error()
		}
		if unusableBecause != "" {
			rejected = append(rejected, Rejection{Region: offer.Region, Reason: unusableBecause})
			continue
		}
		return Spot(offer), nil
	}

	noRegionCouldBeJudged := len(checkErrs) > 0 && len(checkErrs) == len(rejected)
	if noRegionCouldBeJudged {
		return nil, fmt.Errorf("failed to check any region: %w", errors.Join(checkErrs...))
	}
	if req.OnDemandFallback {
		return OnDemand{Region: req.HomeRegion}, nil
	}

	noSpot := &NoSpotRegionError{VMSize: req.VMSize, OS: req.OS, Rejected: rejected}
	if err := s.State.recordNoSpot(noSpot.OS, noSpot.VMSize, noSpot.Rejected, now); err != nil {
		return nil, errors.Join(noSpot, err)
	}
	return nil, noSpot
}

func (s Selector) ranking(ctx context.Context, vmSize string, osType OS, now time.Time) (Ranking, error) {
	cached, fetchedAt, haveCache := s.State.cachedRanking(vmSize, osType)
	if haveCache && now.Sub(fetchedAt) < s.PriceTTL {
		return cached, nil
	}

	fresh, err := s.Prices.SpotRankings(ctx, vmSize)
	if err == nil && len(fresh[osType]) == 0 {
		err = fmt.Errorf("the price API has no Spot price for %s (%s)", vmSize, osType)
	}
	if err != nil {
		if haveCache {
			return cached, nil
		}
		return nil, err
	}

	for rankedOS, ranking := range fresh {
		if len(ranking) == 0 {
			continue
		}
		if err := s.State.storeRanking(vmSize, rankedOS, ranking, now); err != nil {
			return nil, err
		}
	}
	return fresh[osType], nil
}

func (r Ranking) within(candidates []string) Ranking {
	if len(candidates) == 0 {
		return r
	}
	allowed := make(map[string]bool, len(candidates))
	for _, region := range candidates {
		allowed[region] = true
	}
	var kept Ranking
	for _, offer := range r {
		if allowed[offer.Region] {
			kept = append(kept, offer)
		}
	}
	return kept
}
