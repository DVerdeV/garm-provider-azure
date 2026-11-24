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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// State is a directory of JSON files. GARM starts one provider process per
// operation and may run several at once, so every fact gets a file of its own
// and every write replaces the whole file with a rename. No process reads a
// file in order to write it back.
type State struct {
	dir string
}

func OpenState(dir string) (*State, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create state_dir: %w", err)
	}
	return &State{dir: dir}, nil
}

type priceFile struct {
	VMSize    string    `json:"vm_size"`
	OS        OS        `json:"os"`
	FetchedAt time.Time `json:"fetched_at"`
	Offers    Ranking   `json:"offers"`
}

type skipFile struct {
	Region    string    `json:"region"`
	VMSize    string    `json:"vm_size"`
	ExpiresAt time.Time `json:"expires_at"`
	Reason    string    `json:"reason"`
}

type noSpotFile struct {
	OS             OS          `json:"os"`
	VMSize         string      `json:"vm_size"`
	RecordedAt     time.Time   `json:"recorded_at"`
	RecordedAtUnix int64       `json:"recorded_at_unix"`
	Rejected       []Rejection `json:"rejected"`
}

func (s *State) SkipRegion(region, vmSize string, until time.Time, reason string) error {
	name, err := safeFileName("skip", region, vmSize)
	if err != nil {
		return err
	}
	return s.write(name, skipFile{
		Region:    region,
		VMSize:    vmSize,
		ExpiresAt: until.UTC().Truncate(time.Second),
		Reason:    reason,
	})
}

func (s *State) skippedUntil(region, vmSize string, now time.Time) (time.Time, bool) {
	name, err := safeFileName("skip", region, vmSize)
	if err != nil {
		return time.Time{}, false
	}
	var marker skipFile
	if !s.readIntact(name, &marker) || !now.Before(marker.ExpiresAt) {
		return time.Time{}, false
	}
	return marker.ExpiresAt, true
}

func (s *State) cachedRanking(vmSize string, osType OS) (Ranking, time.Time, bool) {
	name, err := safeFileName("prices", vmSize, string(osType))
	if err != nil {
		return nil, time.Time{}, false
	}
	var cached priceFile
	if !s.readIntact(name, &cached) {
		return nil, time.Time{}, false
	}
	return cached.Offers, cached.FetchedAt, true
}

func (s *State) storeRanking(vmSize string, osType OS, ranking Ranking, fetchedAt time.Time) error {
	name, err := safeFileName("prices", vmSize, string(osType))
	if err != nil {
		return err
	}
	return s.write(name, priceFile{
		VMSize:    vmSize,
		OS:        osType,
		FetchedAt: fetchedAt.UTC().Truncate(time.Second),
		Offers:    ranking,
	})
}

func (s *State) recordNoSpot(noSpot *NoSpotRegionError, at time.Time) error {
	name, err := safeFileName("no-spot", string(noSpot.OS))
	if err != nil {
		return err
	}
	at = at.UTC().Truncate(time.Second)
	return s.write(name, noSpotFile{
		OS:             noSpot.OS,
		VMSize:         noSpot.VMSize,
		RecordedAt:     at,
		RecordedAtUnix: at.Unix(),
		Rejected:       noSpot.Rejected,
	})
}

var safeFileNamePart = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func safeFileName(kind string, parts ...string) (string, error) {
	for _, part := range parts {
		if !safeFileNamePart.MatchString(part) {
			return "", fmt.Errorf("%q cannot be part of a state file name", part)
		}
	}
	return kind + "-" + strings.Join(parts, "-") + ".json", nil
}

func (s *State) readIntact(name string, into any) bool {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return false
	}
	return json.Unmarshal(data, into) == nil
}

func (s *State) write(name string, content any) error {
	data, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("failed to encode %s: %w", name, err)
	}
	tmp, err := os.CreateTemp(s.dir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to write %s: %w", name, err)
	}
	_, err = tmp.Write(append(data, '\n'))
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(s.dir, name))
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("failed to write %s: %w", name, err)
	}
	return nil
}
