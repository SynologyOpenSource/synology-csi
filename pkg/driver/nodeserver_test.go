/*
Copyright 2021 Synology Inc.

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
	"reflect"
	"testing"
)

func TestWithXfsMountOptions(t *testing.T) {
	tests := []struct {
		name    string
		fsType  string
		options []string
		want    []string
	}{
		{
			name:    "xfs gets nouuid so clones can be mounted next to their source",
			fsType:  "xfs",
			options: []string{"rw"},
			want:    []string{"rw", "nouuid"},
		},
		{
			name:    "nouuid is not repeated when the user already asked for it",
			fsType:  "xfs",
			options: []string{"rw", "nouuid"},
			want:    []string{"rw", "nouuid"},
		},
		{
			name:    "ext4 is left untouched",
			fsType:  "ext4",
			options: []string{"rw"},
			want:    []string{"rw"},
		},
		{
			name:    "an unset fsType is left untouched",
			fsType:  "",
			options: []string{"rw"},
			want:    []string{"rw"},
		},
		{
			name:    "other user options are preserved",
			fsType:  "xfs",
			options: []string{"rw", "noatime"},
			want:    []string{"rw", "noatime", "nouuid"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withXfsMountOptions(tt.fsType, tt.options)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("withXfsMountOptions(%q, %v) = %v, want %v",
					tt.fsType, tt.options, got, tt.want)
			}
		})
	}
}
