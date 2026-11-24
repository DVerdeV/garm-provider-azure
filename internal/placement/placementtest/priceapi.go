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

package placementtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
)

type Price struct {
	ArmRegionName string  `json:"armRegionName"`
	ProductName   string  `json:"productName"`
	SKUName       string  `json:"skuName"`
	RetailPrice   float64 `json:"retailPrice"`
}

func LinuxSpot(region string, usdPerHour float64) Price {
	return Price{ArmRegionName: region, ProductName: "Virtual Machines Dadsv5 Series", SKUName: "D2ads v5 Spot", RetailPrice: usdPerHour}
}

func WindowsSpot(region string, usdPerHour float64) Price {
	return Price{ArmRegionName: region, ProductName: "Virtual Machines Dadsv5 Series Windows", SKUName: "D2ads v5 Spot", RetailPrice: usdPerHour}
}

type PriceAPI struct {
	URL string

	mu       sync.Mutex
	prices   []Price
	pageSize int
	status   int
	filters  []string
}

func NewPriceAPI(t testing.TB, prices ...Price) *PriceAPI {
	t.Helper()
	api := &PriceAPI{prices: prices, pageSize: 100, status: http.StatusOK}
	server := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(server.Close)
	api.URL = server.URL + "/api/retail/prices"
	return api
}

func (a *PriceAPI) SetPrices(prices ...Price) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prices = prices
}

func (a *PriceAPI) SetPageSize(rows int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pageSize = rows
}

func (a *PriceAPI) SetStatus(status int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status = status
}

func (a *PriceAPI) FirstPageFilters() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.filters...)
}

func (a *PriceAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	query := r.URL.Query()
	skip, _ := strconv.Atoi(query.Get("$skip"))
	if skip == 0 {
		a.filters = append(a.filters, query.Get("$filter"))
	}
	if a.status != http.StatusOK {
		http.Error(w, "unavailable", a.status)
		return
	}

	end := min(skip+a.pageSize, len(a.prices))
	page := struct {
		Items        []Price `json:"Items"`
		NextPageLink string  `json:"NextPageLink"`
	}{Items: a.prices[min(skip, end):end]}
	if end < len(a.prices) {
		page.NextPageLink = a.URL + "?$filter=" + url.QueryEscape(query.Get("$filter")) + "&$skip=" + strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}
