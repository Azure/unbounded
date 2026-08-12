// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

func TestParseListRoundTrip(t *testing.T) {
	entries := []ListEntry{
		{Item: "3", Values: url.Values{"volume": {"pvc-abc"}}},
		{Item: "7", Values: url.Values{"volume": {"pvc-def"}, "zone": {"2"}}},
	}

	raw := FormatList(entries)
	assert.Equal(t, "3?volume=pvc-abc,7?volume=pvc-def&zone=2", raw)

	back, err := ParseList(raw)
	require.NoError(t, err)
	assert.Equal(t, entries, back)
}

func TestParseListRejectsMalformed(t *testing.T) {
	_, err := ParseList("3?volume=%zz")
	assert.Error(t, err)
}

func TestParseListEmpty(t *testing.T) {
	entries, err := ParseList("   ")
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestParseGeometryImmutableOnly(t *testing.T) {
	segments, err := ParseGeometry(64<<20, map[string]string{})
	require.NoError(t, err)
	require.Len(t, segments, 1)
	assert.Equal(t, racerconfig.Kind_IMMUTABLE_4M, segments[0].Kind)
	assert.Equal(t, uint64(16), segments[0].Pages)
}

func TestParseGeometryTwoSegments(t *testing.T) {
	segments, err := ParseGeometry(68<<20, map[string]string{
		AttrMutableBytes: "4Mi",
		AttrMutableKind:  "OCC",
	})
	require.NoError(t, err)
	require.Len(t, segments, 2)

	assert.Equal(t, racerconfig.Kind_OCC, segments[0].Kind)
	assert.Equal(t, uint64(1024), segments[0].Pages)
	assert.Equal(t, racerconfig.Kind_IMMUTABLE_4M, segments[1].Kind)
	assert.Equal(t, uint64(16), segments[1].Pages)
}

func TestParseGeometrySmallImmutable(t *testing.T) {
	segments, err := ParseGeometry(8<<10, map[string]string{
		AttrMutableBytes:      "4Ki",
		AttrImmutablePageSize: "4Ki",
	})
	require.NoError(t, err)
	require.Len(t, segments, 2)
	assert.Equal(t, racerconfig.Kind_LWW, segments[0].Kind)
	assert.Equal(t, racerconfig.Kind_IMMUTABLE, segments[1].Kind)
	assert.Equal(t, uint64(1), segments[1].Pages)
}

func TestParseGeometryRejectsUnalignedMutableBytes(t *testing.T) {
	// The head has to end on a tail-page boundary or the tail's pages would not
	// line up with the device's offsets. Rounding silently would hand the user a
	// volume that is not the size they asked for.
	_, err := ParseGeometry(64<<20, map[string]string{AttrMutableBytes: "1Ki"})
	assert.Error(t, err)
}

func TestParseGeometryRejectsMutableLargerThanCapacity(t *testing.T) {
	_, err := ParseGeometry(4<<20, map[string]string{AttrMutableBytes: "8Mi"})
	assert.Error(t, err)
}

func TestParseGeometryRejectsUnknownAttribute(t *testing.T) {
	_, err := ParseGeometry(64<<20, map[string]string{"nonsense": "1"})
	assert.Error(t, err)
}

func TestParseGeometryRejectsImmutableMutableKind(t *testing.T) {
	_, err := ParseGeometry(64<<20, map[string]string{
		AttrMutableBytes: "4Mi",
		AttrMutableKind:  "IMMUTABLE",
	})
	assert.Error(t, err)
}

func TestParseGeometryRejectsZeroCapacity(t *testing.T) {
	_, err := ParseGeometry(0, nil)
	assert.Error(t, err)
}

func TestBuildCatalogIsBalanced(t *testing.T) {
	members := Membership{}
	for i := uint32(1); i <= 12; i++ {
		members = append(members, Member{NodeID: i, Cohort: (i - 1) % Cohorts})
	}

	catalog, err := BuildCatalog(members.Normalized(), 12)
	require.NoError(t, err)
	require.Len(t, catalog, 12)

	counts := map[uint32]int{}

	for _, trio := range catalog {
		ids := []uint32{trio.GetCohort_0(), trio.GetCohort_1(), trio.GetCohort_2()}
		assert.Len(t, uniqueIDs(ids), 3, "a group must name three distinct nodes")

		for _, id := range ids {
			counts[id]++
		}
	}

	require.Len(t, counts, 12)

	for id, count := range counts {
		assert.Equal(t, 3, count, "node %d holds an unbalanced share", id)
	}
}

func TestBuildCatalogSpreadsGroups(t *testing.T) {
	// A rotation that did not offset the second and third cohorts would collapse
	// into only perCohort distinct replica sets, so losing one node would cost a
	// single peer everything rather than spreading the replay.
	members := Membership{}
	for i := uint32(1); i <= 9; i++ {
		members = append(members, Member{NodeID: i, Cohort: (i - 1) % Cohorts})
	}

	catalog, err := BuildCatalog(members.Normalized(), 9)
	require.NoError(t, err)

	distinct := map[[3]uint32]struct{}{}
	for _, trio := range catalog {
		distinct[[3]uint32{trio.GetCohort_0(), trio.GetCohort_1(), trio.GetCohort_2()}] = struct{}{}
	}

	assert.Greater(t, len(distinct), 3)
}

func TestBuildCatalogRejectsUnbalancedMembership(t *testing.T) {
	members := Membership{
		{NodeID: 1, Cohort: 0},
		{NodeID: 2, Cohort: 1},
	}

	_, err := BuildCatalog(members, 3)
	assert.Error(t, err)
}

func TestBuildCatalogRejectsIndivisibleSize(t *testing.T) {
	members := Membership{
		{NodeID: 1, Cohort: 0},
		{NodeID: 2, Cohort: 1},
		{NodeID: 3, Cohort: 2},
		{NodeID: 4, Cohort: 0},
		{NodeID: 5, Cohort: 1},
		{NodeID: 6, Cohort: 2},
	}

	_, err := BuildCatalog(members, 9)
	assert.Error(t, err)
}

func TestMembershipRoundTrip(t *testing.T) {
	members := Membership{
		{NodeID: 4, Cohort: 0},
		{NodeID: 1, Cohort: 1},
		{NodeID: 9, Cohort: 2},
	}

	back, err := ParseMembership(FormatMembership(members))
	require.NoError(t, err)
	assert.Equal(t, members.Normalized(), back)
}

func TestNextMembershipSwapsOneNode(t *testing.T) {
	current := Membership{{NodeID: 1, Cohort: 0}, {NodeID: 2, Cohort: 1}, {NodeID: 3, Cohort: 2}}
	desired := Membership{{NodeID: 1, Cohort: 0}, {NodeID: 5, Cohort: 1}, {NodeID: 3, Cohort: 2}}

	step, err := NextMembership(current, desired, 3)
	require.NoError(t, err)
	assert.False(t, step.Done)
	assert.Equal(t, desired.Normalized(), step.Next.Normalized())
}

func TestNextMembershipDone(t *testing.T) {
	current := Membership{{NodeID: 1, Cohort: 0}, {NodeID: 2, Cohort: 1}, {NodeID: 3, Cohort: 2}}

	step, err := NextMembership(current, current, 3)
	require.NoError(t, err)
	assert.True(t, step.Done)
}

func TestDesiredMembershipPicksLargestBalancedSubset(t *testing.T) {
	candidates := Membership{}
	for i := uint32(1); i <= 4; i++ {
		candidates = append(candidates, Member{NodeID: i, Cohort: 0})
	}

	for i := uint32(5); i <= 8; i++ {
		candidates = append(candidates, Member{NodeID: i, Cohort: 1})
	}

	for i := uint32(9); i <= 11; i++ {
		candidates = append(candidates, Member{NodeID: i, Cohort: 2})
	}

	desired := DesiredMembership(candidates, 12)

	// Only three per cohort can be used: a catalog needs the cohorts equal, and
	// the smallest cohort has three.
	perCohort, err := desired.PerCohort()
	require.NoError(t, err)
	assert.Equal(t, 3, perCohort)
	assert.Equal(t, 0, 12%perCohort)
}

func uniqueIDs(ids []uint32) []uint32 {
	return dedupeSorted(ids)
}
