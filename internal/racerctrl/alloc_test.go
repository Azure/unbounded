// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

func TestCursorsRoundTrip(t *testing.T) {
	cursors, err := ParseCursors(nil)
	require.NoError(t, err)
	assert.Equal(t, Cursors{
		NextUniverseID: 1,
		NextExtentID:   1,
		NextNodeID:     1,
		NextZoneID:     1,
		Zones:          map[string]uint32{},
		ZoneDefs:       map[uint32]ZoneDef{},
		ZoneBuckets:    map[uint32]map[string]uint32{},
	}, cursors)

	universe, err := cursors.AllocateUniverseID()
	require.NoError(t, err)
	assert.Equal(t, uint32(1), universe)

	node, err := cursors.AllocateNodeID()
	require.NoError(t, err)
	assert.Equal(t, uint32(1), node)

	zone, err := cursors.ZoneID("westus2-1")
	require.NoError(t, err)
	assert.Equal(t, uint32(1), zone)

	// A name that has been seen keeps its id forever; a new name gets the next.
	again, err := cursors.ZoneID("westus2-1")
	require.NoError(t, err)
	assert.Equal(t, uint32(1), again)

	other, err := cursors.ZoneID("westus2-2")
	require.NoError(t, err)
	assert.Equal(t, uint32(2), other)

	back, err := ParseCursors(cursors.Data())
	require.NoError(t, err)
	assert.Equal(t, cursors, back)
}

func TestZoneCursorsRoundTrip(t *testing.T) {
	cursors, err := ParseCursors(nil)
	require.NoError(t, err)

	zone, err := cursors.AllocateZoneID()
	require.NoError(t, err)
	assert.Equal(t, uint32(1), zone)

	cursors.DefineZone(zone, ZoneDef{Site: "edge-1", Fabric: "rail a"})

	// A zone's definition is written once. A second call is what a requeue
	// looks like and must not repoint the zone at a different site.
	cursors.DefineZone(zone, ZoneDef{Site: "somewhere else"})
	assert.Equal(t, ZoneDef{Site: "edge-1", Fabric: "rail a"}, cursors.ZoneDefs[zone])

	cursors.ZoneBuckets[zone] = map[string]uint32{"westus2-1": 0, "westus2 2": 2}
	cursors.ZoneTarget = 250

	data := cursors.Data()

	// The zone keys must not collide with the name interning prefix, which is
	// parsed over the whole ConfigMap.
	assert.NotContains(t, data, ZoneKeyPrefix+"target")

	back, err := ParseCursors(data)
	require.NoError(t, err)
	assert.Equal(t, cursors, back)
}

func TestCursorsRejectAnOutOfRangeBucket(t *testing.T) {
	_, err := ParseCursors(map[string]string{ZoneBucketsKeyPrefix + "1": "westus2-1=3"})
	assert.Error(t, err)
}

func TestCursorsRefuseExhaustedZoneSpace(t *testing.T) {
	cursors := Cursors{NextZoneID: uint32(MaxZones) + 1}

	_, err := cursors.AllocateZoneID()
	assert.Error(t, err)
}

func TestCursorsNeverHandOutZero(t *testing.T) {
	// Zero is reserved everywhere in the schema, so a cursor that has somehow
	// been written as zero has to skip it rather than emit an id racer refuses.
	cursors, err := ParseCursors(map[string]string{NextUniverseIDKey: "0", NextExtentIDKey: "0"})
	require.NoError(t, err)

	universe, err := cursors.AllocateUniverseID()
	require.NoError(t, err)
	assert.Equal(t, uint32(1), universe)

	extent, err := cursors.AllocateExtentID()
	require.NoError(t, err)
	assert.Equal(t, uint32(1), extent)
}

func TestCursorsRefuseExhaustedUniverseSpace(t *testing.T) {
	cursors := Cursors{NextUniverseID: MaxUniverse, NextExtentID: 1}

	_, err := cursors.AllocateUniverseID()
	assert.Error(t, err)
}

func TestAllocateAlignsEverySegment(t *testing.T) {
	cursors := Cursors{NextUniverseID: 1, NextExtentID: 1}

	specs := []SegmentSpec{
		{Kind: racerconfig.Kind_LWW, Pages: 1024},
		{Kind: racerconfig.Kind_IMMUTABLE_4M, Pages: 16},
	}

	composition, next, err := Allocate(specs, &cursors, 0)
	require.NoError(t, err)
	require.Len(t, composition, 2)

	for _, segment := range composition {
		assert.Zero(t, segment.BaseLBA%HugeBlocks, "every base_lba is 4 MiB aligned")
	}

	assert.Equal(t, uint32(1), composition[0].ExtentID)
	assert.Equal(t, uint32(2), composition[1].ExtentID)
	assert.Greater(t, next, composition[1].BaseLBA)
	assert.Equal(t, uint32(3), cursors.NextExtentID)
}

func TestAllocateLeavesCursorsAloneOnFailure(t *testing.T) {
	cursors := Cursors{NextUniverseID: 1, NextExtentID: 1}

	// A span that will not fit must not burn an extent id: the caller may retry
	// against another universe, and a burned id is gone forever.
	_, _, err := Allocate([]SegmentSpec{{Kind: racerconfig.Kind_IMMUTABLE_4M, Pages: 1 << 40}}, &cursors, 0)
	assert.Error(t, err)
	assert.Equal(t, uint32(1), cursors.NextExtentID)
}

func TestCompositionRoundTrip(t *testing.T) {
	composition := Composition{
		{ExtentID: 7, BaseLBA: 0, Pages: 1024, Kind: racerconfig.Kind_OCC},
		{ExtentID: 8, BaseLBA: 1024, Pages: 16, Kind: racerconfig.Kind_IMMUTABLE_4M},
	}

	back, err := ParseComposition(FormatComposition(composition))
	require.NoError(t, err)
	assert.Equal(t, composition, back)
}

func TestParseCompositionRejectsGaps(t *testing.T) {
	_, err := ParseComposition("0?extent=1&baseLba=0&pages=1&kind=LWW,2?extent=2&baseLba=1&pages=1&kind=LWW")
	assert.Error(t, err)
}

func TestMinorAllocationIsLowestFree(t *testing.T) {
	self := &NodeState{}

	fabric, changed, err := AssignFabricDeviceID(self, 5, MinorSpace{})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, uint32(1), fabric)

	device, changed, err := AssignDeviceID(self, "pv-a", MinorSpace{})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, uint32(2), device)

	// Idempotent: asking again returns the same minor and reports no change, so
	// a reload cannot move a volume's path out from under an open fd.
	again, changed, err := AssignDeviceID(self, "pv-a", MinorSpace{})
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, device, again)

	assert.True(t, ReleaseDeviceID(self, "pv-a"))

	reused, _, err := AssignDeviceID(self, "pv-b", MinorSpace{})
	require.NoError(t, err)
	assert.Equal(t, uint32(2), reused, "a released minor is free again")
}

func TestNodeStateAnnotationRoundTrip(t *testing.T) {
	state := NodeState{
		Name:       "node-1",
		ID:         4,
		Cohort:     1,
		Zone:       2,
		StoreBytes: 1 << 40,
		Devices:    []DeviceBinding{{DeviceID: 3, Volume: "pv-a"}},
		Fabric:     []FabricExport{{UniverseID: 9, DeviceID: 1, NQN: "nqn.2026-01.io.racer:u9"}},
		Health:     Health{Generation: 42, Replaying: 1},
		Live:       map[uint32]LiveExtent{7: {Pages: 12, Tombstones: 3}},
	}

	annotations := state.StatusAnnotations()
	annotations[NodeIDAnnotation] = "4"
	annotations[NodeCohortAnnotation] = "1"
	annotations[NodeZoneAnnotation] = "2"

	back, err := ParseNodeState("node-1", annotations)
	require.NoError(t, err)
	assert.Equal(t, state, back)
}

// The announcement is the one racer annotation a node writes before it has an
// identity, so it has to survive a round trip through a state with nothing else
// in it.
func TestParseNodeStateReadsTheAgentAnnouncement(t *testing.T) {
	state, err := ParseNodeState("node-1", map[string]string{NodeAgentAnnotation: NodeAgentRunning})
	require.NoError(t, err)
	assert.Equal(t, NodeAgentRunning, state.Agent)
	assert.Zero(t, state.ID)

	silent, err := ParseNodeState("node-1", nil)
	require.NoError(t, err)
	assert.Empty(t, silent.Agent)
}

// StatusAnnotations must not carry the announcement. It opens the identity gate
// that publishing sits behind, so a node that only wrote it there could never
// write it at all.
func TestStatusAnnotationsOmitsTheAgentAnnouncement(t *testing.T) {
	state := NodeState{Name: "node-1", Agent: NodeAgentRunning}
	assert.NotContains(t, state.StatusAnnotations(), NodeAgentAnnotation)
}

func TestParseNodeStateRejectsBadCohort(t *testing.T) {
	_, err := ParseNodeState("node-1", map[string]string{NodeCohortAnnotation: "3"})
	assert.Error(t, err)
}

func TestParseNodeStateTreatsMissingIdentityAsNotReady(t *testing.T) {
	state, err := ParseNodeState("node-1", nil)
	require.NoError(t, err)
	assert.Zero(t, state.ID)
	assert.Zero(t, state.Zone)
}

func TestParseDeviceBindingsRejectsDuplicateVolume(t *testing.T) {
	_, err := ParseDeviceBindings("1?volume=pv-a,2?volume=pv-a")
	assert.Error(t, err)
}

func TestPublishInstallsAtomicallyAndSkipsNoOps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)

	cfg := fixtureConfig(t, 1)

	changed, err := Publish(path, nil, cfg)
	require.NoError(t, err)
	assert.True(t, changed)

	installed, err := ReadConfig(path)
	require.NoError(t, err)
	assert.True(t, proto.Equal(cfg, installed))

	// No temporary file may survive a successful install.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, ConfigFileName, entries[0].Name())

	changed, err = Publish(path, installed, cfg)
	require.NoError(t, err)
	assert.False(t, changed, "an unchanged derivation must not provoke a reload")
}

func TestPublishRefusesInvalidCandidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigFileName)

	cfg := fixtureConfig(t, 1)
	cfg.Node.Id = 0

	_, err := Publish(path, nil, cfg)
	require.Error(t, err)

	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr), "a rejected candidate must never reach the directory")
}

func TestReadConfigTreatsMissingFileAsNoConfig(t *testing.T) {
	cfg, err := ReadConfig(filepath.Join(t.TempDir(), ConfigFileName))
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

// fixtureConfig builds a minimal config that passes validation: one node, one
// universe with a three-node catalog, two peers and one extent.
func fixtureConfig(t *testing.T, generation uint64) *racerconfig.NodeConfig {
	t.Helper()

	cohort := racerconfig.Cohort_COHORT_0

	return &racerconfig.NodeConfig{
		Generation: generation,
		Node: &racerconfig.Node{
			Id:     1,
			Zone:   1,
			Cohort: &cohort,
			Store:  &racerconfig.Store{SizeBytes: 1 << 30},
		},
		Universes: []*racerconfig.Universe{{
			Id:    1,
			Epoch: 1,
			Catalog: []*racerconfig.Trio{
				{Cohort_0: 1, Cohort_1: 2, Cohort_2: 3},
			},
			Peers: []*racerconfig.Peer{
				{Id: 2, Device: "/dev/nvme1n1"},
				{Id: 3, Device: "/dev/nvme2n1"},
			},
			Extents: []*racerconfig.Extent{
				{Id: 1, BaseLba: 0, Pages: 16, Kind: racerconfig.Kind_IMMUTABLE_4M, Zone: 1},
			},
			FabricDeviceId: 1,
		}},
		Devices: []*racerconfig.Device{
			{Id: 2, Extents: []uint32{1}},
		},
	}
}

// The cursor is read from a ConfigMap annotation and parsed as a full uint64, so
// a value near the top of the range is a thing an edit can produce. Aligning
// before bounding wrapped it to a low base that looked perfectly valid, and the
// extent placed there would overlap live data.
func TestAllocateLBARefusesACursorThatWouldWrap(t *testing.T) {
	for name, next := range map[string]uint64{
		"max uint64":        math.MaxUint64,
		"one below max":     math.MaxUint64 - 1,
		"just above maxlba": MaxLBA + 1,
	} {
		t.Run(name, func(t *testing.T) {
			base, advanced, err := AllocateLBA(next, 1)
			require.Error(t, err)
			assert.Zero(t, base, "a refused allocation must not hand back an address")
			assert.Zero(t, advanced)
		})
	}
}

// The same guard on the length: a block count that large cannot fit whatever the
// cursor is, and adding it first is what overflows.
func TestAllocateLBARefusesALengthThatWouldWrap(t *testing.T) {
	_, _, err := AllocateLBA(0, math.MaxUint64)
	require.Error(t, err)

	_, _, err = AllocateLBA(HugeBlocks, MaxLBA)
	require.Error(t, err)
}

// The boundary itself is allocatable: the last block of the universe is a block.
func TestAllocateLBAFillsTheAddressSpaceExactly(t *testing.T) {
	base, advanced, err := AllocateLBA(MaxLBA-HugeBlocks, HugeBlocks)
	require.NoError(t, err)
	assert.Equal(t, uint64(MaxLBA-HugeBlocks), base)
	assert.Equal(t, uint64(MaxLBA), advanced)

	_, _, err = AllocateLBA(MaxLBA-HugeBlocks, HugeBlocks+1)
	require.Error(t, err, "one block past the end is past the end")
}

// alignUp saturates rather than wrapping, because every caller reads the result
// as an address or a count and a wrap turns the largest value into a small one.
func TestAlignUpSaturates(t *testing.T) {
	assert.Equal(t, uint64(math.MaxUint64), alignUp(math.MaxUint64, HugeBlocks))
	assert.Equal(t, uint64(math.MaxUint64), alignUp(math.MaxUint64-1, 4))
	assert.Equal(t, uint64(HugeBlocks), alignUp(1, HugeBlocks))
	assert.Equal(t, uint64(HugeBlocks), alignUp(HugeBlocks, HugeBlocks))
	assert.Equal(t, uint64(0), alignUp(0, HugeBlocks))
	assert.Equal(t, uint64(7), alignUp(7, 0), "no alignment is not a division")
}

// A declared zone name is an administrator's string joined to a site name, so it
// routinely is not a legal ConfigMap key. Writing it raw produced a ConfigMap
// the API server refused, which lost every allocation written alongside it.
func TestZoneNamesSurviveAConfigMapRoundTrip(t *testing.T) {
	names := []string{
		"plain",
		"site-a|rack-3",
		"with spaces",
		"emoji-\u2601",
		"trailing/slash",
		".",
		"..",
	}

	cursors := Cursors{Zones: map[string]uint32{}}
	for i, name := range names {
		cursors.Zones[name] = uint32(i + 1)
	}

	data := cursors.Data()

	for key := range data {
		assert.True(t, isConfigMapKey(key), "key %q is not one a ConfigMap accepts", key)
	}

	parsed, err := ParseCursors(data)
	require.NoError(t, err)

	for i, name := range names {
		assert.Equal(t, uint32(i+1), parsed.Zones[name], "zone name %q did not survive", name)
	}
}

// A name that is already a legal key stays readable, because an operator reading
// the ConfigMap is the reason the mapping is recorded rather than hashed.
func TestKeySafeZoneNamesStayVerbatim(t *testing.T) {
	data := Cursors{Zones: map[string]uint32{"rack-3": 4}}.Data()

	_, ok := data[ZoneKeyPrefix+"rack-3"]
	assert.True(t, ok, "a legal name has no reason to be encoded")
}

// A name too long to record is refused when it is interned, not when the write
// fails, because by then the rest of the pass has already allocated ids.
func TestZoneIDRefusesANameTooLongForAKey(t *testing.T) {
	cursors := Cursors{NextZoneID: 1}

	_, err := cursors.ZoneID(strings.Repeat("a", 256))
	require.ErrorContains(t, err, "too long")
}

// isConfigMapKey is the API server's rule for a ConfigMap data key.
func isConfigMapKey(key string) bool {
	if key == "" || key == "." || key == ".." || len(key) > maxConfigMapKey {
		return false
	}

	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '.', r == '_':
		default:
			return false
		}
	}

	return true
}

func TestAppliedAnnotationRoundTrip(t *testing.T) {
	applied := Applied{
		Generation: 42,
		Epochs:     map[uint32]uint32{1: 7, 9: 3},
		Extents: map[uint32]AppliedExtent{
			5: {NextZone: 2},
			8: {TombstoneEpoch: 11},
		},
	}

	raw := FormatApplied(applied)

	parsed, err := ParseApplied(raw)
	require.NoError(t, err)
	require.Equal(t, applied, parsed)

	// Stable ordering, so an unchanged fact does not rewrite the annotation.
	require.Equal(t, raw, FormatApplied(parsed))
}

func TestAppliedIsEmptyBeforeAnythingIsInstalled(t *testing.T) {
	require.Empty(t, FormatApplied(Applied{}))

	parsed, err := ParseApplied("")
	require.NoError(t, err)
	require.Zero(t, parsed.Generation)
}

func TestAppliedRefusesGarbage(t *testing.T) {
	for _, raw := range []string{"nonsense", "u?epoch=1", "xzz?next=1", "generation?at=x"} {
		_, err := ParseApplied(raw)
		require.Error(t, err, "parsed %q", raw)
	}
}

// An extent an operation is waiting on has to be reported even when its count is
// zero. Otherwise a node that has never heard of it and a node that has finished
// with it look identical, and the destructive half of the sequence acts on the
// wrong one.
func TestLiveReportsAnExplicitZeroForExtentsInFlight(t *testing.T) {
	required := map[uint32]AppliedExtent{7: {TombstoneEpoch: 4}}

	raw := FormatLive(map[uint32]LiveExtent{}, required)

	parsed, err := ParseLive(raw)
	require.NoError(t, err)

	entry, ok := parsed[7]
	require.True(t, ok, "extent 7 was omitted from %q", raw)
	require.Zero(t, entry.Pages)
	require.Zero(t, entry.Tombstones)
}

// Everything else that is empty still goes unsaid: a node carries on the order
// of a thousand extents and the annotation has a 256 KiB ceiling.
func TestLiveOmitsEmptyExtentsNobodyIsWaitingOn(t *testing.T) {
	raw := FormatLive(map[uint32]LiveExtent{7: {}, 8: {Pages: 3}}, nil)

	parsed, err := ParseLive(raw)
	require.NoError(t, err)

	_, ok := parsed[7]
	require.False(t, ok, "an idle extent nobody asked about was published: %q", raw)
	require.Equal(t, uint64(3), parsed[8].Pages)
}

func TestLiveDoesNotDuplicateAnExtentItAlreadyReports(t *testing.T) {
	raw := FormatLive(
		map[uint32]LiveExtent{7: {Pages: 3}},
		map[uint32]AppliedExtent{7: {NextZone: 2}},
	)

	parsed, err := ParseLive(raw)
	require.NoError(t, err)
	require.Equal(t, uint64(3), parsed[7].Pages)
}

func TestMinorAllocationHonoursTheBase(t *testing.T) {
	// ublk minors are the kernel's, so instances sharing one take disjoint
	// windows rather than both starting at the bottom.
	self := &NodeState{}

	fabric, _, err := AssignFabricDeviceID(self, 5, MinorSpace{Base: 257})
	require.NoError(t, err)
	assert.Equal(t, uint32(257), fabric)

	device, _, err := AssignDeviceID(self, "pv-a", MinorSpace{Base: 257})
	require.NoError(t, err)
	assert.Equal(t, uint32(258), device)

	// The window is still MaxExports wide, so the per-node budget is unchanged.
	full := &NodeState{}
	for i := range MaxExports {
		_, _, err := AssignDeviceID(full, fmt.Sprintf("pv-%d", i), MinorSpace{Base: 257})
		require.NoError(t, err)
	}

	assert.Equal(t, uint32(257), full.Devices[0].DeviceID)
	assert.Equal(t, uint32(257+MaxExports-1), full.Devices[MaxExports-1].DeviceID)

	_, _, err = AssignDeviceID(full, "pv-one-too-many", MinorSpace{Base: 257})
	assert.Error(t, err)

	// A zero base is the bottom of the space, not minor zero.
	bottom := &NodeState{}
	id, _, err := AssignDeviceID(bottom, "pv-a", MinorSpace{})
	require.NoError(t, err)
	assert.Equal(t, uint32(MinDeviceID), id)
}

func TestMinorAllocationSkipsMinorsHeldElsewhere(t *testing.T) {
	// The bindings a node keeps describe only the minors it put there itself.
	// Everything else the kernel holds - another instance, an unrelated ublk
	// user, a device leaked by a crash - is invisible to them and fatal to
	// CMD_ADD_DEV, so the probe is what makes the allocation truthful.
	held := map[uint32]bool{1: true, 2: true, 4: true}
	space := MinorSpace{InUse: func(id uint32) bool { return held[id] }}

	self := &NodeState{}

	fabric, _, err := AssignFabricDeviceID(self, 5, space)
	require.NoError(t, err)
	assert.Equal(t, uint32(3), fabric)

	device, _, err := AssignDeviceID(self, "pv-a", space)
	require.NoError(t, err)
	assert.Equal(t, uint32(5), device)

	// A minor this node has already bound is not offered to the probe: it is
	// held by us, and answering "in use" for it must not push allocation past
	// the end of the window.
	probed := map[uint32]bool{}
	counting := MinorSpace{InUse: func(id uint32) bool {
		probed[id] = true

		return held[id]
	}}

	_, _, err = AssignDeviceID(self, "pv-b", counting)
	require.NoError(t, err)
	assert.False(t, probed[3], "the fabric minor this node owns was handed to the probe")
	assert.False(t, probed[5], "the device minor this node owns was handed to the probe")

	// A window whose every minor is spoken for is exhausted, not silently
	// wrapped onto somebody else's device.
	_, _, err = AssignDeviceID(&NodeState{}, "pv-c", MinorSpace{InUse: func(uint32) bool { return true }})
	assert.ErrorContains(t, err, "held elsewhere")
}
