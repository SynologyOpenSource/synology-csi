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
	"testing"

	"github.com/SynologyOpenSource/synology-csi/pkg/utils"
)

// Share-backed protocols serve files and cannot expose a raw block device;
// LUN-backed ones can. volumeMode: Block is rejected based on this split.
func TestIsShareProtocol(t *testing.T) {
	tests := []struct {
		protocol string
		want     bool
	}{
		{utils.ProtocolNfs, true},
		{utils.ProtocolSmb, true},
		{utils.ProtocolIscsi, false},
		{utils.ProtocolNvme, false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.protocol, func(t *testing.T) {
			if got := isShareProtocol(tt.protocol); got != tt.want {
				t.Errorf("isShareProtocol(%q) = %v, want %v", tt.protocol, got, tt.want)
			}
		})
	}
}
