/*
Copyright 2026 Synology Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

// TestParseMinVolumeSize verifies that the minVolumeSize StorageClass parameter
// is parsed correctly. Supported formats: plain bytes, Ki/Mi/Gi suffixes, and
// K/M/G (powers of 1000) for user convenience.
func TestParseMinVolumeSize(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		{"empty returns default 1GiB", "", 1 * utils.UNIT_GB, false},
		{"plain bytes", "1073741824", 1 * utils.UNIT_GB, false},
		{"500Mi", "500Mi", 500 * utils.UNIT_MB, false},
		{"1Gi", "1Gi", 1 * utils.UNIT_GB, false},
		{"2Gi", "2Gi", 2 * utils.UNIT_GB, false},
		{"100Mi", "100Mi", 100 * utils.UNIT_MB, false},
		{"1024Ki", "1024Ki", 1024 * 1024, false},
		{"1Ti", "1Ti", 1024 * utils.UNIT_GB, false},
		{"invalid suffix", "500Xi", 0, true},
		{"negative not allowed", "-1Gi", 0, true},
		{"zero not allowed", "0", 0, true},
		{"non-numeric", "abc", 0, true},
		{"fractional not supported", "1.5Gi", 0, true},
		{"overflow Ti rejected", "9999999999999999Ti", 0, true},
		{"overflow Gi rejected", "999999999999999999Gi", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMinVolumeSize(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseMinVolumeSize(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseMinVolumeSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// TestGetSizeByCapacityRange_WithMinVolumeSize verifies that the minVolumeSize
// parameter overrides the default 1 GiB floor.
func TestGetSizeByCapacityRange_WithMinVolumeSize(t *testing.T) {
	tests := []struct {
		name          string
		capRange      *csi.CapacityRange
		minVolumeSize int64
		want          int64
		wantErr       bool
	}{
		{
			name:          "nil capRange uses minVolumeSize",
			capRange:      nil,
			minVolumeSize: 500 * utils.UNIT_MB,
			want:          500 * utils.UNIT_MB,
			wantErr:       false,
		},
		{
			name: "requiredBytes above minVolumeSize is accepted",
			capRange: &csi.CapacityRange{
				RequiredBytes: 1 * utils.UNIT_GB,
			},
			minVolumeSize: 500 * utils.UNIT_MB,
			want:          1 * utils.UNIT_GB,
			wantErr:       false,
		},
		{
			name: "requiredBytes below minVolumeSize is clamped to minVolumeSize",
			capRange: &csi.CapacityRange{
				RequiredBytes: 100 * utils.UNIT_MB,
			},
			minVolumeSize: 500 * utils.UNIT_MB,
			want:          500 * utils.UNIT_MB,
			wantErr:       false,
		},
		{
			name: "requiredBytes equals minVolumeSize",
			capRange: &csi.CapacityRange{
				RequiredBytes: 500 * utils.UNIT_MB,
			},
			minVolumeSize: 500 * utils.UNIT_MB,
			want:          500 * utils.UNIT_MB,
			wantErr:       false,
		},
		{
			name: "limitBytes below requiredBytes is rejected",
			capRange: &csi.CapacityRange{
				RequiredBytes: 2 * utils.UNIT_GB,
				LimitBytes:    1 * utils.UNIT_GB,
			},
			minVolumeSize: 500 * utils.UNIT_MB,
			want:          0,
			wantErr:       true,
		},
		{
			name:          "floor 0 means no floor (expansion path) — nil capRange returns 0",
			capRange:      nil,
			minVolumeSize: 0,
			want:          0,
			wantErr:       false,
		},
		{
			name: "floor 0 — small requiredBytes passes through unchanged (no clamp)",
			capRange: &csi.CapacityRange{
				RequiredBytes: 500 * utils.UNIT_MB,
			},
			minVolumeSize: 0,
			want:          500 * utils.UNIT_MB,
			wantErr:       false,
		},
		{
			name: "floor 0 — expand 800Mi on SC floor 500Mi is not clamped to 1GiB",
			capRange: &csi.CapacityRange{
				RequiredBytes: 800 * utils.UNIT_MB,
			},
			minVolumeSize: 0,
			want:          800 * utils.UNIT_MB,
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getSizeByCapacityRange(tt.capRange, tt.minVolumeSize)
			if (err != nil) != tt.wantErr {
				t.Errorf("getSizeByCapacityRange() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("getSizeByCapacityRange() = %d, want %d", got, tt.want)
			}
		})
	}
}
