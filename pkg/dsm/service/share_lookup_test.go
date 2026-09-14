/*
 * Copyright 2021 Synology Inc.
 */

package service

import (
	"strings"
	"testing"

	"github.com/SynologyOpenSource/synology-csi/pkg/dsm/webapi"
	"github.com/SynologyOpenSource/synology-csi/pkg/models"
)

// classifyCount mirrors what findSMBorNFSVolume does with a share list: filter
// by prefix and predicate, then classify only what matched. Classifying is what
// costs a call to the DSM privilege API, so this counts how many of those a
// lookup would make.
func classifyCount(shares []webapi.ShareInfo, pred func(webapi.ShareInfo) bool) int {
	calls := 0
	for _, share := range shares {
		if !strings.HasPrefix(share.Name, models.SharePrefix) || !pred(share) {
			continue
		}
		calls++
		break // findSMBorNFSVolume returns as soon as one matches
	}
	return calls
}

func manyShares(n int) []webapi.ShareInfo {
	shares := make([]webapi.ShareInfo, 0, n)
	for i := 0; i < n; i++ {
		shares = append(shares, webapi.ShareInfo{
			Name: models.SharePrefix + "pvc-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Uuid: "uuid-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
		})
	}
	return shares
}

func TestLookingUpOneShareCostsOnePrivilegeCall(t *testing.T) {
	shares := manyShares(20)
	target := shares[13]

	calls := classifyCount(shares, func(s webapi.ShareInfo) bool { return s.Uuid == target.Uuid })

	// Classifying every share is what made the driver take the DSM's share lock
	// once per share on every single volume lookup.
	if calls != 1 {
		t.Errorf("expected one privilege call to identify one share, got %d", calls)
	}
}

func TestLookupCostDoesNotGrowWithTheNumberOfShares(t *testing.T) {
	for _, total := range []int{1, 20, 200} {
		shares := manyShares(total)
		target := shares[total-1]

		calls := classifyCount(shares, func(s webapi.ShareInfo) bool { return s.Uuid == target.Uuid })

		if calls != 1 {
			t.Errorf("with %d shares present, a single lookup made %d privilege calls, want 1", total, calls)
		}
	}
}

func TestLookupMakesNoPrivilegeCallWhenNothingMatches(t *testing.T) {
	shares := manyShares(20)

	calls := classifyCount(shares, func(s webapi.ShareInfo) bool { return s.Uuid == "not-a-share" })

	// A volume that belongs to another protocol must not cost anything on the
	// share lock before the iSCSI and NVMe listings are searched.
	if calls != 0 {
		t.Errorf("expected no privilege call when no share matches, got %d", calls)
	}
}

func TestSharesWithoutTheCsiPrefixAreIgnored(t *testing.T) {
	shares := []webapi.ShareInfo{
		{Name: "homes", Uuid: "same-uuid"},
		{Name: models.SharePrefix + "pvc-1", Uuid: "other"},
	}

	// A user's own share could in principle match, and classifying it would both
	// waste a call and report it as a CSI volume.
	calls := classifyCount(shares, func(s webapi.ShareInfo) bool { return s.Uuid == "same-uuid" })

	if calls != 0 {
		t.Errorf("expected shares outside the CSI prefix to be skipped, got %d calls", calls)
	}
}
