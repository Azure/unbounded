// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

// Volume attribute keys. These live in a PersistentVolume's
// spec.csi.volumeAttributes, which is where a statically provisioned volume
// carries its driver parameters, and they are the volume's geometry. Geometry is
// frozen at creation: an extent's base_lba, page count and kind may never change
// for the life of the extent, so there is nothing here an update could mean.
const (
	// AttrMutableBytes sizes the mutable head, in Kubernetes quantity notation.
	// Defaults to zero, which gives a single-extent immutable volume.
	AttrMutableBytes = "mutableBytes"

	// AttrMutableKind selects the mutable head's kind: "LWW" for last-writer-wins
	// or "OCC" for optimistic concurrency. Defaults to LWW.
	AttrMutableKind = "mutableKind"

	// AttrImmutablePageSize selects the immutable tail's page size: "4Ki" or
	// "4Mi". Defaults to 4Mi, which is the cheaper of the two per byte stored.
	AttrImmutablePageSize = "immutablePageSize"

	// AttrImmutableExtentBytes cuts the immutable tail into equal extents of
	// this size instead of one extent spanning the whole tail. It must divide
	// the tail exactly. Absent means one extent.
	//
	// The extent is the unit of reclamation: tombstone_epoch is per extent and
	// advancing it destroys everything in that extent, so a volume that wants
	// to reclaim space in pieces has to be built out of pieces. Nothing else
	// about the volume changes: the extents are concatenated in order and the
	// guest still sees one flat device.
	AttrImmutableExtentBytes = "immutableExtentBytes"
)

// MaxVolumeExtents caps the extents one volume may be cut into. Every extent is
// declared capacity that each node in the zone reserves on its store at format
// time, so a typo in immutableExtentBytes is a typo that fallocates a petabyte.
// The ceiling is well under MaxExtents, which counts every extent a node is told
// about across every volume.
const MaxVolumeExtents = 256

// SegmentSpec is one extent's shape, before it has been allocated an id or a
// place in the address space.
type SegmentSpec struct {
	// Kind is the extent kind, which also fixes the page size.
	Kind racerconfig.Kind

	// Pages is the extent's length in pages of that kind's size.
	Pages uint64
}

// PageBytes is the size of one page of this segment's kind.
func (s SegmentSpec) PageBytes() uint64 {
	return KindPageBytes(s.Kind)
}

// Bytes is the segment's size as the exported device sees it.
func (s SegmentSpec) Bytes() uint64 {
	return s.Pages * s.PageBytes()
}

// Blocks is the segment's length in 4 KiB universe blocks, which is what
// base_lba counts in.
func (s SegmentSpec) Blocks() uint64 {
	return s.Pages * (s.PageBytes() / SmallPage)
}

// KindPageBytes is the page size a kind is stored in. IMMUTABLE_4M is the only
// kind stored in 4 MiB pages; every other kind is stored in 4 KiB pages.
func KindPageBytes(kind racerconfig.Kind) uint64 {
	if kind == racerconfig.Kind_IMMUTABLE_4M {
		return HugePage
	}

	return SmallPage
}

// KindIsHuge reports whether a kind is stored in 4 MiB pages.
func KindIsHuge(kind racerconfig.Kind) bool {
	return kind == racerconfig.Kind_IMMUTABLE_4M
}

// KindIsImmutable reports whether a kind is written once. Only immutable kinds
// may be warmed into other zones, because warming assumes the copy cannot go
// stale.
func KindIsImmutable(kind racerconfig.Kind) bool {
	return kind == racerconfig.Kind_IMMUTABLE || kind == racerconfig.Kind_IMMUTABLE_4M
}

// ParseKind reads a kind name as it appears in an annotation or attribute.
func ParseKind(raw string) (racerconfig.Kind, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "LWW":
		return racerconfig.Kind_LWW, nil
	case "OCC":
		return racerconfig.Kind_OCC, nil
	case "IMMUTABLE":
		return racerconfig.Kind_IMMUTABLE, nil
	case "IMMUTABLE_4M", "IMMUTABLE4M":
		return racerconfig.Kind_IMMUTABLE_4M, nil
	default:
		return 0, fmt.Errorf("unknown extent kind %q, want LWW, OCC, IMMUTABLE or IMMUTABLE_4M", raw)
	}
}

// ParseGeometry turns a volume's capacity and CSI attributes into the ordered
// segments of its device.
//
// A volume is an optional mutable head in 4 KiB pages, then an immutable tail
// that runs to the end of the device. The tail is one extent by default, or a
// run of equal extents when immutableExtentBytes says so. Either way the tail
// covers the remaining capacity exactly, so the whole advertised block space is
// addressable and nothing is reserved.
//
// The tail's size has to be declared up front rather than grown, because an
// extent's page count is frozen for its life, a device's extent list is frozen
// once the device exists, and the node sizes its store from both. That is why it
// is derived from capacity and not left open.
func ParseGeometry(capacityBytes uint64, attributes map[string]string) ([]SegmentSpec, error) {
	if err := checkKnownAttributes(attributes); err != nil {
		return nil, err
	}

	if capacityBytes == 0 {
		return nil, fmt.Errorf("capacity must be greater than zero")
	}

	mutableBytes, err := parseQuantityAttribute(attributes, AttrMutableBytes)
	if err != nil {
		return nil, err
	}

	extentBytes, err := parseQuantityAttribute(attributes, AttrImmutableExtentBytes)
	if err != nil {
		return nil, err
	}

	mutableKind := racerconfig.Kind_LWW

	if raw := strings.TrimSpace(attributes[AttrMutableKind]); raw != "" {
		mutableKind, err = ParseKind(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", AttrMutableKind, err)
		}

		if KindIsImmutable(mutableKind) {
			return nil, fmt.Errorf("%s must be LWW or OCC, not %s", AttrMutableKind, mutableKind)
		}
	}

	var tailKind racerconfig.Kind

	switch raw := strings.TrimSpace(attributes[AttrImmutablePageSize]); raw {
	case "", "4Mi":
		tailKind = racerconfig.Kind_IMMUTABLE_4M
	case "4Ki":
		tailKind = racerconfig.Kind_IMMUTABLE
	default:
		return nil, fmt.Errorf("%s must be 4Ki or 4Mi, got %q", AttrImmutablePageSize, raw)
	}

	if mutableBytes > capacityBytes {
		return nil, fmt.Errorf("%s (%d) exceeds capacity (%d)", AttrMutableBytes, mutableBytes, capacityBytes)
	}

	immutableBytes := capacityBytes - mutableBytes

	// The mutable head sits at device offset zero and the tail begins where it
	// ends, so the head's size is also the tail's alignment. A 4 MiB tail whose
	// pages straddle a 4 KiB boundary could never be written whole, and
	// IMMUTABLE_4M cannot read-modify-write its way out of that, so reject the
	// layout rather than silently rounding a size the author asked for.
	tailAlignment := KindPageBytes(tailKind)
	if mutableBytes%tailAlignment != 0 {
		return nil, fmt.Errorf(
			"%s (%d) must be a multiple of %d so the immutable tail starts on a page boundary",
			AttrMutableBytes, mutableBytes, tailAlignment,
		)
	}

	if immutableBytes%tailAlignment != 0 {
		return nil, fmt.Errorf(
			"immutable tail (%d bytes, capacity minus %s) must be a multiple of %d",
			immutableBytes, AttrMutableBytes, tailAlignment,
		)
	}

	tail, err := cutTail(immutableBytes, extentBytes, tailKind)
	if err != nil {
		return nil, err
	}

	segments := make([]SegmentSpec, 0, 1+len(tail))

	if mutableBytes > 0 {
		segments = append(segments, SegmentSpec{Kind: mutableKind, Pages: mutableBytes / SmallPage})
	}

	segments = append(segments, tail...)

	if len(segments) == 0 {
		return nil, fmt.Errorf("volume has no segments")
	}

	return segments, nil
}

// cutTail divides the immutable tail into extents. Zero extentBytes means one
// extent covering the whole tail.
func cutTail(immutableBytes, extentBytes uint64, kind racerconfig.Kind) ([]SegmentSpec, error) {
	if immutableBytes == 0 {
		if extentBytes != 0 {
			return nil, fmt.Errorf("%s is set but the volume has no immutable tail", AttrImmutableExtentBytes)
		}

		return nil, nil
	}

	if extentBytes == 0 {
		extentBytes = immutableBytes
	}

	pageBytes := KindPageBytes(kind)
	if extentBytes%pageBytes != 0 {
		return nil, fmt.Errorf(
			"%s (%d) must be a multiple of the %d byte page size",
			AttrImmutableExtentBytes, extentBytes, pageBytes,
		)
	}

	// An uneven division would leave a short extent at the end, which the
	// snapshotter's reclaim arithmetic and the operator's capacity arithmetic
	// would both have to special-case. Refusing costs the author one edit.
	if immutableBytes%extentBytes != 0 {
		return nil, fmt.Errorf(
			"immutable tail (%d bytes) is not a whole number of %s (%d)",
			immutableBytes, AttrImmutableExtentBytes, extentBytes,
		)
	}

	count := immutableBytes / extentBytes
	if count > MaxVolumeExtents {
		return nil, fmt.Errorf(
			"immutable tail cuts into %d extents, more than the %d allowed",
			count, MaxVolumeExtents,
		)
	}

	tail := make([]SegmentSpec, 0, count)
	for range count {
		tail = append(tail, SegmentSpec{Kind: kind, Pages: extentBytes / pageBytes})
	}

	return tail, nil
}

// checkKnownAttributes refuses attributes this driver does not understand. A
// typo in a volume's geometry has to fail at admission rather than quietly
// provisioning something other than what was asked for, because the result is
// frozen for the volume's life.
func checkKnownAttributes(attributes map[string]string) error {
	known := map[string]bool{
		AttrMutableBytes:         true,
		AttrMutableKind:          true,
		AttrImmutablePageSize:    true,
		AttrImmutableExtentBytes: true,
	}

	unknown := make([]string, 0)

	for key := range attributes {
		// Kubernetes injects its own attributes on some paths; ours are the only
		// ones we police, and everything else is left alone.
		if strings.Contains(key, "/") || strings.HasPrefix(key, "csi.storage.k8s.io") {
			continue
		}

		if !known[key] {
			unknown = append(unknown, key)
		}
	}

	if len(unknown) > 0 {
		sort.Strings(unknown)

		return fmt.Errorf("unknown volume attributes: %s", strings.Join(unknown, ", "))
	}

	return nil
}

func parseQuantityAttribute(attributes map[string]string, key string) (uint64, error) {
	raw := strings.TrimSpace(attributes[key])
	if raw == "" {
		return 0, nil
	}

	quantity, err := resource.ParseQuantity(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: parse %q: %w", key, raw, err)
	}

	value, ok := quantity.AsInt64()
	if !ok || value < 0 {
		return 0, fmt.Errorf("%s: %q is not a non-negative byte count", key, raw)
	}

	if uint64(value)%SmallPage != 0 {
		return 0, fmt.Errorf("%s: %q must be a multiple of %d", key, raw, SmallPage)
	}

	return uint64(value), nil
}
