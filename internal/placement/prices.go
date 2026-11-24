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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

type OS string

const (
	Linux   OS = "linux"
	Windows OS = "windows"
)

type Offer struct {
	Region     string  `json:"region"`
	USDPerHour float64 `json:"usd_per_hour"`
}

type Ranking []Offer

const DefaultPriceURL = "https://prices.azure.com/api/retail/prices"

const (
	maxPricePages    = 50
	maxPricePageSize = 16 << 20
)

var (
	vmSizePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	regionPattern = regexp.MustCompile(`^[a-z0-9]+$`)
)

type PriceSource struct {
	BaseURL    string
	HTTPClient *http.Client
}

type pricePage struct {
	Items []struct {
		ArmRegionName string  `json:"armRegionName"`
		ProductName   string  `json:"productName"`
		SKUName       string  `json:"skuName"`
		RetailPrice   float64 `json:"retailPrice"`
	} `json:"Items"`
	NextPageLink string `json:"NextPageLink"`
}

func (p PriceSource) SpotRankings(ctx context.Context, vmSize string) (map[OS]Ranking, error) {
	if !vmSizePattern.MatchString(vmSize) {
		return nil, fmt.Errorf("invalid VM size %q", vmSize)
	}

	baseURL := p.BaseURL
	if baseURL == "" {
		baseURL = DefaultPriceURL
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid price API URL: %w", err)
	}
	filter := fmt.Sprintf("serviceName eq 'Virtual Machines' and priceType eq 'Consumption' and armSkuName eq '%s' and contains(skuName,'Spot')", vmSize)
	base.RawQuery = url.Values{"$filter": {filter}}.Encode()

	lowest := map[OS]map[string]float64{Linux: {}, Windows: {}}
	next := base.String()
	for pages := 0; next != ""; pages++ {
		if pages == maxPricePages {
			return nil, fmt.Errorf("the price API returned more than %d pages for %s", maxPricePages, vmSize)
		}
		page, err := p.fetch(ctx, next)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if strings.Contains(item.SKUName, "Low Priority") || !regionPattern.MatchString(item.ArmRegionName) || item.RetailPrice <= 0 {
				continue
			}
			osType := Linux
			if strings.Contains(item.ProductName, "Windows") {
				osType = Windows
			}
			if price, seen := lowest[osType][item.ArmRegionName]; !seen || item.RetailPrice < price {
				lowest[osType][item.ArmRegionName] = item.RetailPrice
			}
		}
		if next, err = nextPageOnSameHost(base, page.NextPageLink); err != nil {
			return nil, err
		}
	}

	rankings := map[OS]Ranking{}
	for osType, prices := range lowest {
		ranking := make(Ranking, 0, len(prices))
		for region, price := range prices {
			ranking = append(ranking, Offer{Region: region, USDPerHour: price})
		}
		sort.Slice(ranking, func(i, j int) bool {
			if ranking[i].USDPerHour != ranking[j].USDPerHour {
				return ranking[i].USDPerHour < ranking[j].USDPerHour
			}
			return ranking[i].Region < ranking[j].Region
		})
		rankings[osType] = ranking
	}
	return rankings, nil
}

func (p PriceSource) fetch(ctx context.Context, pageURL string) (pricePage, error) {
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return pricePage{}, fmt.Errorf("failed to build price request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return pricePage{}, fmt.Errorf("failed to fetch prices: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return pricePage{}, fmt.Errorf("the price API answered %s", resp.Status)
	}

	var page pricePage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPricePageSize)).Decode(&page); err != nil {
		return pricePage{}, fmt.Errorf("failed to decode prices: %w", err)
	}
	return page, nil
}

func nextPageOnSameHost(base *url.URL, link string) (string, error) {
	if link == "" {
		return "", nil
	}
	next, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("invalid next page link from the price API: %w", err)
	}
	if next.Scheme != base.Scheme || !strings.EqualFold(next.Hostname(), base.Hostname()) {
		return "", fmt.Errorf("the price API linked to another host: %s", next.Host)
	}
	return link, nil
}
