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
)

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
// A volume is at most two extents: an optional mutable head in 4 KiB pages, then
// an immutable tail that runs to the end of the device. The tail's size is the
// remaining capacity, so the whole advertised block space is addressable and
// nothing is reserved.
//
// The tail's size has to be declared up front rather than grown, because an
// extent's page count is frozen for its life and the node sizes its store from
// it. That is why it is derived from capacity and not left open.
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

	segments := make([]SegmentSpec, 0, 2)

	if mutableBytes > 0 {
		segments = append(segments, SegmentSpec{Kind: mutableKind, Pages: mutableBytes / SmallPage})
	}

	if immutableBytes > 0 {
		segments = append(segments, SegmentSpec{Kind: tailKind, Pages: immutableBytes / KindPageBytes(tailKind)})
	}

	if len(segments) == 0 {
		return nil, fmt.Errorf("volume has no segments")
	}

	return segments, nil
}

// checkKnownAttributes refuses attributes this driver does not understand. A
// typo in a volume's geometry has to fail at admission rather than quietly
// provisioning something other than what was asked for, because the result is
// frozen for the volume's life.
func checkKnownAttributes(attributes map[string]string) error {
	known := map[string]bool{
		AttrMutableBytes:      true,
		AttrMutableKind:       true,
		AttrImmutablePageSize: true,
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
