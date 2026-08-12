// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

// Identifier allocation.
//
// R2 gives every id space the same three properties: non-zero, unique in its
// scope, never reused. Reuse is the dangerous one. An extent id that comes back
// after the extent it named was collected would let a stale page register
// resolve against live data, so the cursors here only ever move forward and are
// never compacted. The spaces are large enough that this costs nothing: the
// extent space is 2^32 and a cluster that created one extent a second would take
// 136 years to exhaust it.
//
// Cursors live in a single ConfigMap that the operator writes under an
// optimistic lock. That is what makes allocation safe without a lease: a lost
// race is a conflict, and a conflict is a retry, not a duplicate id.

// Cursor keys in the racer-allocations ConfigMap.
const (
	// NextUniverseIDKey holds the next universe id to hand out.
	NextUniverseIDKey = "next-universe-id"

	// NextExtentIDKey holds the next extent id to hand out.
	NextExtentIDKey = "next-extent-id"

	// NextNodeIDKey holds the next node id to hand out.
	NextNodeIDKey = "next-node-id"

	// NextZoneIDKey holds the next zone id to hand out.
	NextZoneIDKey = "next-zone-id"

	// ZoneKeyPrefix prefixes one key per known failure domain, mapping the
	// Kubernetes zone name to the numeric zone id the schema uses. The mapping
	// is recorded rather than derived because a zone id appears in every node's
	// config and in every extent's home, so a hash change or a renamed label
	// would silently repartition the cluster.
	ZoneKeyPrefix = "zone-"
)

// AllocationsConfigMapName is the ConfigMap holding the cluster-wide cursors.
const AllocationsConfigMapName = "racer-allocations"

// Cursors is the parsed content of the allocations ConfigMap. The zero value is
// the state of a cluster that has never allocated anything; every cursor is
// normalized to 1 on read, because zero is reserved.
type Cursors struct {
	NextUniverseID uint32
	NextExtentID   uint32
	NextNodeID     uint32
	NextZoneID     uint32

	// Zones maps a Kubernetes failure-domain name to its numeric zone id.
	Zones map[string]uint32
}

// ParseCursors reads the cursors out of a ConfigMap's data. Missing keys start
// at one rather than zero, since zero is not a legal id anywhere in the schema.
func ParseCursors(data map[string]string) (Cursors, error) {
	cursors := Cursors{
		NextUniverseID: 1,
		NextExtentID:   1,
		NextNodeID:     1,
		NextZoneID:     1,
		Zones:          map[string]uint32{},
	}

	scalars := []struct {
		key    string
		target *uint32
	}{
		{key: NextUniverseIDKey, target: &cursors.NextUniverseID},
		{key: NextExtentIDKey, target: &cursors.NextExtentID},
		{key: NextNodeIDKey, target: &cursors.NextNodeID},
		{key: NextZoneIDKey, target: &cursors.NextZoneID},
	}

	for _, scalar := range scalars {
		raw, ok := data[scalar.key]
		if !ok {
			continue
		}

		value, err := ParseUint32(raw)
		if err != nil {
			return Cursors{}, fmt.Errorf("%s: %w", scalar.key, err)
		}

		if value != 0 {
			*scalar.target = value
		}
	}

	for key, raw := range data {
		name, ok := strings.CutPrefix(key, ZoneKeyPrefix)
		if !ok || name == "" {
			continue
		}

		value, err := ParseUint32(raw)
		if err != nil {
			return Cursors{}, fmt.Errorf("%s: %w", key, err)
		}

		if value == 0 {
			return Cursors{}, fmt.Errorf("%s: zone id must not be zero", key)
		}

		cursors.Zones[name] = value
	}

	return cursors, nil
}

// Data renders the cursors back into ConfigMap data.
func (c Cursors) Data() map[string]string {
	data := map[string]string{
		NextUniverseIDKey: strconv.FormatUint(uint64(c.NextUniverseID), 10),
		NextExtentIDKey:   strconv.FormatUint(uint64(c.NextExtentID), 10),
		NextNodeIDKey:     strconv.FormatUint(uint64(c.NextNodeID), 10),
		NextZoneIDKey:     strconv.FormatUint(uint64(c.NextZoneID), 10),
	}

	for name, id := range c.Zones {
		data[ZoneKeyPrefix+name] = strconv.FormatUint(uint64(id), 10)
	}

	return data
}

// ZoneID returns the numeric zone id for a failure-domain name, allocating one
// the first time the name is seen. A name is never given a second id and an id
// is never given to a second name, so a zone that empties out and later refills
// comes back as itself.
func (c *Cursors) ZoneID(name string) (uint32, error) {
	if name == "" {
		return 0, fmt.Errorf("zone name must not be empty")
	}

	if c.Zones == nil {
		c.Zones = map[string]uint32{}
	}

	if id, ok := c.Zones[name]; ok {
		return id, nil
	}

	if c.NextZoneID == 0 {
		c.NextZoneID = 1
	}

	if int(c.NextZoneID) > MaxZones {
		return 0, fmt.Errorf("zone id space exhausted at %d; a universe may name at most %d zones", c.NextZoneID, MaxZones)
	}

	id := c.NextZoneID
	c.NextZoneID = id + 1
	c.Zones[name] = id

	return id, nil
}

// AllocateNodeID takes the next node id and advances the cursor. Node ids are
// never reused: a returned id would let a replaced machine inherit the page
// registers of the one it replaced.
func (c *Cursors) AllocateNodeID() (uint32, error) {
	if c.NextNodeID == 0 {
		c.NextNodeID = 1
	}

	if c.NextNodeID == ^uint32(0) {
		return 0, fmt.Errorf("node id space exhausted at %d", c.NextNodeID)
	}

	id := c.NextNodeID
	c.NextNodeID = id + 1

	return id, nil
}

// AllocateUniverseID takes the next universe id and advances the cursor. The
// address a page register carries is universe:26 | lba:38, so a universe id has
// to stay below 2^26; running out is fatal rather than wrapping, because wrapping
// would alias two universes onto one address space.
func (c *Cursors) AllocateUniverseID() (uint32, error) {
	if c.NextUniverseID == 0 {
		c.NextUniverseID = 1
	}

	if uint64(c.NextUniverseID) >= MaxUniverse {
		return 0, fmt.Errorf("universe id space exhausted at %d", c.NextUniverseID)
	}

	id := c.NextUniverseID
	c.NextUniverseID = id + 1

	return id, nil
}

// AllocateExtentID takes the next extent id and advances the cursor.
func (c *Cursors) AllocateExtentID() (uint32, error) {
	if c.NextExtentID == 0 {
		c.NextExtentID = 1
	}

	if c.NextExtentID == ^uint32(0) {
		return 0, fmt.Errorf("extent id space exhausted at %d", c.NextExtentID)
	}

	id := c.NextExtentID
	c.NextExtentID = id + 1

	return id, nil
}

// AllocateLBA carves blocks off a universe's bump cursor and returns the base.
//
// Every allocation is aligned to HugeBlocks whatever the extent's page size. The
// schema only requires it of IMMUTABLE_4M, but applying it everywhere means a
// volume's two segments can differ in page size without the tail's alignment
// depending on the head's length, and the waste is at most 4 MiB per extent out
// of a universe's 1 PiB.
//
// The cursor never goes backwards and space is never reclaimed. That is what
// makes an extent id safe to retire: nothing will ever be placed where it was.
func AllocateLBA(next, blocks uint64) (base, advanced uint64, err error) {
	if blocks == 0 {
		return 0, 0, fmt.Errorf("cannot allocate zero blocks")
	}

	base = alignUp(next, HugeBlocks)

	end := base + blocks
	if end > MaxLBA {
		return 0, 0, fmt.Errorf(
			"universe address space exhausted: %d blocks at base %d exceeds %d",
			blocks, base, uint64(MaxLBA),
		)
	}

	return base, end, nil
}

// Segment is one allocated extent of a volume: a SegmentSpec that has been given
// an id and a place in a universe's address space.
type Segment struct {
	ExtentID uint32
	BaseLBA  uint64
	Pages    uint64
	Kind     racerconfig.Kind
}

// Blocks is the segment's span in 4 KiB blocks.
func (s Segment) Blocks() uint64 {
	if KindIsHuge(s.Kind) {
		return s.Pages * HugeBlocks
	}

	return s.Pages
}

// Bytes is the segment's addressable size.
func (s Segment) Bytes() uint64 {
	return s.Pages * KindPageBytes(s.Kind)
}

// Composition is a volume's allocated layout: the ordered segments that make up
// its exported device. It is stamped on the PersistentVolume once, when the
// volume is first placed, and frozen from then on. The order is the device's
// order: segment N's pages begin where N-1's ended.
type Composition []Segment

// Bytes is the composition's total addressable size.
func (c Composition) Bytes() uint64 {
	var total uint64

	for _, seg := range c {
		total += seg.Bytes()
	}

	return total
}

// ExtentIDs returns the composition's extent ids in device order.
func (c Composition) ExtentIDs() []uint32 {
	ids := make([]uint32, 0, len(c))
	for _, seg := range c {
		ids = append(ids, seg.ExtentID)
	}

	return ids
}

// Allocate places a volume's segments in a universe, advancing the id and
// address cursors. It returns the composition and the new next-LBA. Nothing is
// mutated unless the whole allocation succeeds, so a caller that hits the end of
// the address space partway through can retry against a different universe
// without having burned ids.
func Allocate(specs []SegmentSpec, cursors *Cursors, nextLBA uint64) (Composition, uint64, error) {
	if len(specs) == 0 {
		return nil, 0, fmt.Errorf("volume has no segments")
	}

	local := *cursors
	cursor := nextLBA
	composition := make(Composition, 0, len(specs))

	for i, spec := range specs {
		id, err := local.AllocateExtentID()
		if err != nil {
			return nil, 0, fmt.Errorf("segment %d: %w", i, err)
		}

		base, advanced, err := AllocateLBA(cursor, spec.Blocks())
		if err != nil {
			return nil, 0, fmt.Errorf("segment %d: %w", i, err)
		}

		composition = append(composition, Segment{
			ExtentID: id,
			BaseLBA:  base,
			Pages:    spec.Pages,
			Kind:     spec.Kind,
		})

		cursor = advanced
	}

	*cursors = local

	return composition, cursor, nil
}

// Composition annotation encoding. Keys in the query string.
const (
	compositionExtentKey  = "extent"
	compositionBaseLBAKey = "baseLba"
	compositionPagesKey   = "pages"
	compositionKindKey    = "kind"
)

// FormatComposition renders a composition for the PV annotation. The item is the
// segment's index in the device, so the encoding survives a sort and reading it
// back never depends on entry order.
func FormatComposition(composition Composition) string {
	entries := make([]ListEntry, 0, len(composition))

	for i, seg := range composition {
		values := url.Values{}
		values.Set(compositionExtentKey, strconv.FormatUint(uint64(seg.ExtentID), 10))
		values.Set(compositionBaseLBAKey, strconv.FormatUint(seg.BaseLBA, 10))
		values.Set(compositionPagesKey, strconv.FormatUint(seg.Pages, 10))
		values.Set(compositionKindKey, seg.Kind.String())

		entries = append(entries, ListEntry{
			Item:   strconv.Itoa(i),
			Values: values,
		})
	}

	return FormatList(entries)
}

// ParseComposition reads a composition back out of a PV annotation.
func ParseComposition(raw string) (Composition, error) {
	entries, err := ParseList(raw)
	if err != nil {
		return nil, err
	}

	type indexed struct {
		index   int
		segment Segment
	}

	parsed := make([]indexed, 0, len(entries))
	seen := make(map[int]struct{}, len(entries))

	for _, entry := range entries {
		index, err := strconv.Atoi(entry.Item)
		if err != nil || index < 0 {
			return nil, fmt.Errorf("composition: %q is not a segment index", entry.Item)
		}

		if _, dup := seen[index]; dup {
			return nil, fmt.Errorf("composition: segment %d listed twice", index)
		}

		seen[index] = struct{}{}

		segment, err := parseSegment(entry.Values)
		if err != nil {
			return nil, fmt.Errorf("composition: segment %d: %w", index, err)
		}

		parsed = append(parsed, indexed{index: index, segment: segment})
	}

	sort.Slice(parsed, func(i, j int) bool { return parsed[i].index < parsed[j].index })

	composition := make(Composition, 0, len(parsed))

	for position, item := range parsed {
		if item.index != position {
			return nil, fmt.Errorf("composition: segment indices are not contiguous from zero")
		}

		composition = append(composition, item.segment)
	}

	return composition, nil
}

func parseSegment(values url.Values) (Segment, error) {
	extentID, err := uint32Value(values, compositionExtentKey)
	if err != nil {
		return Segment{}, err
	}

	if extentID == 0 {
		return Segment{}, fmt.Errorf("extent id must not be zero")
	}

	baseLBA, err := uint64Value(values, compositionBaseLBAKey)
	if err != nil {
		return Segment{}, err
	}

	pages, err := uint64Value(values, compositionPagesKey)
	if err != nil {
		return Segment{}, err
	}

	if pages == 0 {
		return Segment{}, fmt.Errorf("pages must not be zero")
	}

	kind, err := ParseKind(values.Get(compositionKindKey))
	if err != nil {
		return Segment{}, err
	}

	return Segment{
		ExtentID: extentID,
		BaseLBA:  baseLBA,
		Pages:    pages,
		Kind:     kind,
	}, nil
}
