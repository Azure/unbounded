// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Annotation codecs.
//
// Every piece of shared state travels as an annotation, and every annotation is
// read by something other than its writer. That makes the encoding a wire format
// even though it never leaves etcd, so it is worth being strict about: a value
// that fails to parse is dropped with an error rather than defaulted, because a
// defaulted node id or a defaulted cohort is a node placed in the wrong replica
// slot.
//
// The form is the one this repo already uses for node inventory,
// `item?k=v&k2=v2` joined by commas. It survives kubectl, it is greppable, and
// url.ParseQuery does the escaping.

// Health annotation keys.
const (
	healthGenerationKey      = "generation"
	healthRejectedKey        = "rejected"
	healthReplayingKey       = "replaying"
	healthSheddingKey        = "shedding"
	healthUnbackedKey        = "unbacked"
	healthUnavailableKey     = "unavailable"
	healthPressuredKey       = "pressured"
	healthGatewayFallbackKey = "gatewayFallback"
)

// Live annotation keys.
const (
	livePagesKey      = "pages"
	liveTombstonesKey = "tombstones"
)

// Device and fabric annotation keys.
const (
	deviceVolumeKey = "volume"
	fabricDeviceKey = "device"
	fabricNQNKey    = "nqn"
	fabricAddrKey   = "addr"
	fabricRDMAKey   = "rdma"
)

// ParseNodeState reads one node's published state out of its annotations. A node
// that has not been given an identity yet parses to a zero ID, which callers
// treat as "not ready" rather than as an error: the operator has simply not got
// to it.
func ParseNodeState(name string, annotations map[string]string) (NodeState, error) {
	state := NodeState{Name: name}

	var err error

	if state.ID, err = optionalUint32Annotation(annotations, NodeIDAnnotation); err != nil {
		return NodeState{}, err
	}

	if state.Zone, err = optionalUint32Annotation(annotations, NodeZoneAnnotation); err != nil {
		return NodeState{}, err
	}

	if state.Cohort, err = optionalUint32Annotation(annotations, NodeCohortAnnotation); err != nil {
		return NodeState{}, err
	}

	if state.Cohort >= Cohorts && annotations[NodeCohortAnnotation] != "" {
		return NodeState{}, fmt.Errorf("node %q: cohort %d is out of range", name, state.Cohort)
	}

	// Placement inputs. Both are free-form strings a user wrote, so neither can
	// fail to parse; an unset one is the empty string, which is a distinct and
	// meaningful value ("no fabric", "no RDMA address") rather than an error.
	state.FabricID = annotations[NodeFabricIDAnnotation]
	state.RDMAAddr = annotations[NodeRDMAAddrAnnotation]

	// Presence is the whole meaning, so any non-empty value counts and none of
	// them can fail to parse.
	state.Agent = annotations[NodeAgentAnnotation]

	if raw := annotations[NodeStoreBytesAnnotation]; raw != "" {
		if state.StoreBytes, err = ParseUint64(raw); err != nil {
			return NodeState{}, fmt.Errorf("node %q: %s: %w", name, NodeStoreBytesAnnotation, err)
		}
	}

	if state.Devices, err = ParseDeviceBindings(annotations[NodeDevicesAnnotation]); err != nil {
		return NodeState{}, fmt.Errorf("node %q: %s: %w", name, NodeDevicesAnnotation, err)
	}

	if state.Fabric, err = ParseFabricExports(annotations[NodeFabricAnnotation]); err != nil {
		return NodeState{}, fmt.Errorf("node %q: %s: %w", name, NodeFabricAnnotation, err)
	}

	if state.Health, err = ParseHealth(annotations[NodeHealthAnnotation]); err != nil {
		return NodeState{}, fmt.Errorf("node %q: %s: %w", name, NodeHealthAnnotation, err)
	}

	if state.Live, err = ParseLive(annotations[NodeLiveAnnotation]); err != nil {
		return NodeState{}, fmt.Errorf("node %q: %s: %w", name, NodeLiveAnnotation, err)
	}

	if state.Applied, err = ParseApplied(annotations[NodeAppliedAnnotation]); err != nil {
		return NodeState{}, fmt.Errorf("node %q: %s: %w", name, NodeAppliedAnnotation, err)
	}

	return state, nil
}

// StatusAnnotations renders the annotations a node writes about itself. The
// identity annotations are deliberately absent: the operator owns those, and a
// node that wrote its own would race with every other node doing the same.
//
// NodeAgentAnnotation is absent too, for the opposite reason. It is written
// once, before the agent has an identity and so before it has any status to
// publish; folding it in here would put it behind the identity gate it is meant
// to open.
func (n *NodeState) StatusAnnotations() map[string]string {
	return map[string]string{
		NodeStoreBytesAnnotation: strconv.FormatUint(n.StoreBytes, 10),
		NodeDevicesAnnotation:    FormatDeviceBindings(n.Devices),
		NodeFabricAnnotation:     FormatFabricExports(n.Fabric),
		NodeHealthAnnotation:     FormatHealth(n.Health),
		NodeLiveAnnotation:       FormatLive(n.Live, n.Applied.Extents),
		NodeAppliedAnnotation:    FormatApplied(n.Applied),
	}
}

// FormatDeviceBindings renders a node's exported volumes.
func FormatDeviceBindings(bindings []DeviceBinding) string {
	sorted := append([]DeviceBinding(nil), bindings...)
	sortDevices(sorted)

	entries := make([]ListEntry, 0, len(sorted))

	for _, binding := range sorted {
		values := url.Values{}
		values.Set(deviceVolumeKey, binding.Volume)

		entries = append(entries, ListEntry{
			Item:   strconv.FormatUint(uint64(binding.DeviceID), 10),
			Values: values,
		})
	}

	return FormatList(entries)
}

// ParseDeviceBindings reads a node's exported volumes back.
func ParseDeviceBindings(raw string) ([]DeviceBinding, error) {
	entries, err := ParseList(raw)
	if err != nil {
		return nil, err
	}

	bindings := make([]DeviceBinding, 0, len(entries))
	seenID := make(map[uint32]struct{}, len(entries))
	seenVolume := make(map[string]struct{}, len(entries))

	for _, entry := range entries {
		id, err := ParseUint32(entry.Item)
		if err != nil {
			return nil, err
		}

		if id == 0 {
			return nil, fmt.Errorf("device id must not be zero")
		}

		volume := entry.Values.Get(deviceVolumeKey)
		if volume == "" {
			return nil, fmt.Errorf("device %d names no volume", id)
		}

		if _, dup := seenID[id]; dup {
			return nil, fmt.Errorf("device %d appears twice", id)
		}

		if _, dup := seenVolume[volume]; dup {
			return nil, fmt.Errorf("volume %q is bound to two devices", volume)
		}

		seenID[id] = struct{}{}
		seenVolume[volume] = struct{}{}

		bindings = append(bindings, DeviceBinding{DeviceID: id, Volume: volume})
	}

	sortDevices(bindings)

	return bindings, nil
}

// FormatFabricExports renders a node's published fabric namespaces.
func FormatFabricExports(exports []FabricExport) string {
	sorted := append([]FabricExport(nil), exports...)
	sortFabric(sorted)

	entries := make([]ListEntry, 0, len(sorted))

	for _, export := range sorted {
		values := url.Values{}
		values.Set(fabricDeviceKey, strconv.FormatUint(uint64(export.DeviceID), 10))

		if export.NQN != "" {
			values.Set(fabricNQNKey, export.NQN)
		}

		if export.Addr != "" {
			values.Set(fabricAddrKey, export.Addr)
		}

		if export.RDMAAddr != "" {
			values.Set(fabricRDMAKey, export.RDMAAddr)
		}

		entries = append(entries, ListEntry{
			Item:   strconv.FormatUint(uint64(export.UniverseID), 10),
			Values: values,
		})
	}

	return FormatList(entries)
}

// ParseFabricExports reads a node's published fabric namespaces back. Peers use
// this to learn what to attach, so it is the one annotation whose consumer is
// always a different machine from its writer.
func ParseFabricExports(raw string) ([]FabricExport, error) {
	entries, err := ParseList(raw)
	if err != nil {
		return nil, err
	}

	exports := make([]FabricExport, 0, len(entries))
	seen := make(map[uint32]struct{}, len(entries))

	for _, entry := range entries {
		universe, err := ParseUint32(entry.Item)
		if err != nil {
			return nil, err
		}

		if universe == 0 {
			return nil, fmt.Errorf("universe id must not be zero")
		}

		if _, dup := seen[universe]; dup {
			return nil, fmt.Errorf("universe %d appears twice", universe)
		}

		seen[universe] = struct{}{}

		device, err := uint32Value(entry.Values, fabricDeviceKey)
		if err != nil {
			return nil, fmt.Errorf("universe %d: %w", universe, err)
		}

		if device == 0 {
			return nil, fmt.Errorf("universe %d fabric device id must not be zero", universe)
		}

		exports = append(exports, FabricExport{
			UniverseID: universe,
			DeviceID:   device,
			NQN:        entry.Values.Get(fabricNQNKey),
			Addr:       entry.Values.Get(fabricAddrKey),
			RDMAAddr:   entry.Values.Get(fabricRDMAKey),
		})
	}

	sortFabric(exports)

	return exports, nil
}

// FormatHealth renders a node's scraped metrics.
func FormatHealth(health Health) string {
	values := url.Values{}
	values.Set(healthGenerationKey, strconv.FormatUint(health.Generation, 10))
	values.Set(healthRejectedKey, strconv.FormatUint(health.RejectedTotal, 10))
	values.Set(healthReplayingKey, strconv.FormatUint(health.Replaying, 10))
	values.Set(healthSheddingKey, strconv.FormatUint(health.Shedding, 10))
	values.Set(healthUnbackedKey, strconv.FormatUint(health.UnbackedPages, 10))
	values.Set(healthUnavailableKey, strconv.FormatUint(health.GroupsUnavail, 10))
	values.Set(healthPressuredKey, strconv.FormatUint(health.CoresPressured, 10))
	values.Set(healthGatewayFallbackKey, strconv.FormatUint(health.GatewayFallback, 10))

	return FormatValues(values)
}

// ParseHealth reads a node's scraped metrics back. An empty annotation is a node
// whose agent has not scraped yet, not an error - but it is also not evidence of
// health, so the sequencers treat a zero Generation as "no report".
func ParseHealth(raw string) (Health, error) {
	if strings.TrimSpace(raw) == "" {
		return Health{}, nil
	}

	values, err := url.ParseQuery(raw)
	if err != nil {
		return Health{}, fmt.Errorf("malformed health: %w", err)
	}

	var health Health

	fields := []struct {
		key   string
		field *uint64
	}{
		{healthGenerationKey, &health.Generation},
		{healthRejectedKey, &health.RejectedTotal},
		{healthReplayingKey, &health.Replaying},
		{healthSheddingKey, &health.Shedding},
		{healthUnbackedKey, &health.UnbackedPages},
		{healthUnavailableKey, &health.GroupsUnavail},
		{healthPressuredKey, &health.CoresPressured},
		{healthGatewayFallbackKey, &health.GatewayFallback},
	}

	for _, field := range fields {
		value, err := optionalUint64Value(values, field.key)
		if err != nil {
			return Health{}, err
		}

		*field.field = value
	}

	return health, nil
}

// FormatLive renders per-extent liveness. Extents with nothing to report are
// omitted, since the annotation has a 256 KiB ceiling and a node can carry a
// thousand extents.
//
// Required names the extents that must be reported anyway, even at zero: the
// ones in the middle of a sequenced operation. Omission is otherwise
// indistinguishable from a count of zero, and every gate that reads this is
// waiting for a zero, so the compression that keeps the annotation small would
// otherwise let an extent nobody has said anything about pass for an extent
// that has drained.
func FormatLive(live map[uint32]LiveExtent, required map[uint32]AppliedExtent) string {
	ids := make([]uint32, 0, len(live)+len(required))
	seen := make(map[uint32]struct{}, len(live)+len(required))

	for id, extent := range live {
		if extent.Pages == 0 && extent.Tombstones == 0 {
			if _, ok := required[id]; !ok {
				continue
			}
		}

		ids = append(ids, id)
		seen[id] = struct{}{}
	}

	for id := range required {
		if _, ok := seen[id]; ok {
			continue
		}

		ids = append(ids, id)
	}

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	entries := make([]ListEntry, 0, len(ids))

	for _, id := range ids {
		extent := live[id]

		values := url.Values{}
		values.Set(livePagesKey, strconv.FormatUint(extent.Pages, 10))
		values.Set(liveTombstonesKey, strconv.FormatUint(extent.Tombstones, 10))

		entries = append(entries, ListEntry{
			Item:   strconv.FormatUint(uint64(id), 10),
			Values: values,
		})
	}

	return FormatList(entries)
}

// ParseLive reads per-extent liveness back. An extent that is absent is absent
// from the map, which a gate reads as no report rather than as zero.
func ParseLive(raw string) (map[uint32]LiveExtent, error) {
	entries, err := ParseList(raw)
	if err != nil {
		return nil, err
	}

	live := make(map[uint32]LiveExtent, len(entries))

	for _, entry := range entries {
		id, err := ParseUint32(entry.Item)
		if err != nil {
			return nil, err
		}

		if id == 0 {
			return nil, fmt.Errorf("extent id must not be zero")
		}

		if _, dup := live[id]; dup {
			return nil, fmt.Errorf("extent %d appears twice", id)
		}

		pages, err := optionalUint64Value(entry.Values, livePagesKey)
		if err != nil {
			return nil, fmt.Errorf("extent %d: %w", id, err)
		}

		tombstones, err := optionalUint64Value(entry.Values, liveTombstonesKey)
		if err != nil {
			return nil, fmt.Errorf("extent %d: %w", id, err)
		}

		live[id] = LiveExtent{Pages: pages, Tombstones: tombstones}
	}

	return live, nil
}

// Applied annotation keys and item prefixes.
//
// Universes and extents share one list, distinguished by a prefix on the item,
// because they are one fact about one configuration and splitting them over two
// annotations would let a reader pair the universes of one generation with the
// extents of another.
const (
	appliedGenerationItem = "generation"
	appliedUniversePrefix = "u"
	appliedExtentPrefix   = "x"

	appliedAtKey        = "at"
	appliedEpochKey     = "epoch"
	appliedNextZoneKey  = "next"
	appliedTombstoneKey = "tombstone"
)

// FormatApplied renders what the agent last installed. An Applied with no
// generation renders empty, which is how a node that has published nothing says
// so.
func FormatApplied(applied Applied) string {
	if applied.Generation == 0 {
		return ""
	}

	generation := url.Values{}
	generation.Set(appliedAtKey, strconv.FormatUint(applied.Generation, 10))

	entries := []ListEntry{{Item: appliedGenerationItem, Values: generation}}

	universes := make([]uint32, 0, len(applied.Epochs))
	for id := range applied.Epochs {
		universes = append(universes, id)
	}

	sort.Slice(universes, func(i, j int) bool { return universes[i] < universes[j] })

	for _, id := range universes {
		values := url.Values{}
		values.Set(appliedEpochKey, strconv.FormatUint(uint64(applied.Epochs[id]), 10))

		entries = append(entries, ListEntry{
			Item:   appliedUniversePrefix + strconv.FormatUint(uint64(id), 10),
			Values: values,
		})
	}

	extents := make([]uint32, 0, len(applied.Extents))
	for id := range applied.Extents {
		extents = append(extents, id)
	}

	sort.Slice(extents, func(i, j int) bool { return extents[i] < extents[j] })

	for _, id := range extents {
		extent := applied.Extents[id]

		values := url.Values{}
		values.Set(appliedNextZoneKey, strconv.FormatUint(uint64(extent.NextZone), 10))
		values.Set(appliedTombstoneKey, strconv.FormatUint(uint64(extent.TombstoneEpoch), 10))

		entries = append(entries, ListEntry{
			Item:   appliedExtentPrefix + strconv.FormatUint(uint64(id), 10),
			Values: values,
		})
	}

	return FormatList(entries)
}

// ParseApplied reads it back. An empty annotation is a zero Applied and not an
// error: the node has simply not published a configuration yet.
func ParseApplied(raw string) (Applied, error) {
	if raw == "" {
		// Nothing installed, or an agent too old to say. Either way the maps
		// stay nil: reading one is the same, and a gate that wants proof is
		// looking at the generation.
		return Applied{}, nil
	}

	applied := Applied{
		Epochs:  map[uint32]uint32{},
		Extents: map[uint32]AppliedExtent{},
	}

	entries, err := ParseList(raw)
	if err != nil {
		return Applied{}, err
	}

	for _, entry := range entries {
		switch {
		case entry.Item == appliedGenerationItem:
			if applied.Generation, err = optionalUint64Value(entry.Values, appliedAtKey); err != nil {
				return Applied{}, err
			}

		case strings.HasPrefix(entry.Item, appliedUniversePrefix):
			id, err := ParseUint32(strings.TrimPrefix(entry.Item, appliedUniversePrefix))
			if err != nil {
				return Applied{}, err
			}

			epoch, err := optionalUint32Value(entry.Values, appliedEpochKey)
			if err != nil {
				return Applied{}, fmt.Errorf("universe %d: %w", id, err)
			}

			applied.Epochs[id] = epoch

		case strings.HasPrefix(entry.Item, appliedExtentPrefix):
			id, err := ParseUint32(strings.TrimPrefix(entry.Item, appliedExtentPrefix))
			if err != nil {
				return Applied{}, err
			}

			next, err := optionalUint32Value(entry.Values, appliedNextZoneKey)
			if err != nil {
				return Applied{}, fmt.Errorf("extent %d: %w", id, err)
			}

			tombstone, err := optionalUint32Value(entry.Values, appliedTombstoneKey)
			if err != nil {
				return Applied{}, fmt.Errorf("extent %d: %w", id, err)
			}

			applied.Extents[id] = AppliedExtent{NextZone: next, TombstoneEpoch: tombstone}

		default:
			return Applied{}, fmt.Errorf("unknown entry %q", entry.Item)
		}
	}

	return applied, nil
}

// ParseUniverseState reads a StorageClass's racer state out of its annotations.
// The universe id is zero on a class the operator has not yet admitted.
func ParseUniverseState(class string, annotations map[string]string) (UniverseState, error) {
	state := UniverseState{
		Class:        class,
		Members:      map[uint32]Membership{},
		MemberEpochs: map[uint32]uint32{},
		Draining:     map[uint32]Membership{},
		Catalogs:     map[uint32]Catalog{},
		Gateways:     map[uint32][]uint32{},
	}

	var err error

	if state.ID, err = optionalUint32Annotation(annotations, UniverseIDAnnotation); err != nil {
		return UniverseState{}, fmt.Errorf("storage class %q: %w", class, err)
	}

	if state.Epoch, err = optionalUint32Annotation(annotations, EpochAnnotation); err != nil {
		return UniverseState{}, fmt.Errorf("storage class %q: %w", class, err)
	}

	size, err := optionalUint32Annotation(annotations, CatalogSizeAnnotation)
	if err != nil {
		return UniverseState{}, fmt.Errorf("storage class %q: %w", class, err)
	}

	state.CatalogSize = int(size)

	if state.GatewayCount, err = optionalUint32Annotation(annotations, GatewayCountAnnotation); err != nil {
		return UniverseState{}, fmt.Errorf("storage class %q: %w", class, err)
	}

	// Membership is not here. It lives in one ConfigMap per zone, because a
	// thousand-node zone is fourteen kilobytes and sixty-four of them do not fit
	// in one object's annotations. The caller fills Members in from those maps.
	for key, value := range annotations {
		if !strings.HasPrefix(key, GatewaysAnnotationPrefix) {
			continue
		}

		zone, err := ParseUint32(strings.TrimPrefix(key, GatewaysAnnotationPrefix))
		if err != nil {
			return UniverseState{}, fmt.Errorf("storage class %q: %s: %w", class, key, err)
		}

		gateways, err := ParseUint32List(value)
		if err != nil {
			return UniverseState{}, fmt.Errorf("storage class %q: %s: %w", class, key, err)
		}

		state.Gateways[zone] = gateways
	}

	return state, nil
}

// NextLBA reads a universe's address bump cursor.
func NextLBA(annotations map[string]string) (uint64, error) {
	raw := annotations[NextLBAAnnotation]
	if raw == "" {
		return 0, nil
	}

	return ParseUint64(raw)
}

// GatewaysAnnotation is the annotation key holding a zone's gateway node ids.
func GatewaysAnnotation(zone uint32) string {
	return GatewaysAnnotationPrefix + strconv.FormatUint(uint64(zone), 10)
}

// ParseVolumeState reads a PersistentVolume's racer state out of its
// annotations. A volume with no composition has not been placed yet.
func ParseVolumeState(name string, annotations map[string]string) (VolumeState, error) {
	state := VolumeState{Name: name}

	var err error

	if raw := annotations[CompositionAnnotation]; raw != "" {
		if state.Composition, err = ParseComposition(raw); err != nil {
			return VolumeState{}, fmt.Errorf("volume %q: %w", name, err)
		}
	}

	if state.Zone, err = optionalUint32Annotation(annotations, VolumeZoneAnnotation); err != nil {
		return VolumeState{}, fmt.Errorf("volume %q: %w", name, err)
	}

	if state.NextZone, err = optionalUint32Annotation(annotations, NextZoneAnnotation); err != nil {
		return VolumeState{}, fmt.Errorf("volume %q: %w", name, err)
	}

	if state.CacheAdmit, err = optionalUint32Annotation(annotations, CacheAdmitAnnotation); err != nil {
		return VolumeState{}, fmt.Errorf("volume %q: %w", name, err)
	}

	if state.CacheAdmit > MaxCacheAdmit {
		return VolumeState{}, fmt.Errorf("volume %q: cache admit %d is above %d", name, state.CacheAdmit, MaxCacheAdmit)
	}

	state.Phase = annotations[PhaseAnnotation]

	if state.TombstoneEpochs, err = ParseTombstoneEpochs(annotations[TombstoneEpochAnnotation]); err != nil {
		return VolumeState{}, fmt.Errorf("volume %q: %s: %w", name, TombstoneEpochAnnotation, err)
	}

	if raw := annotations[WarmZonesAnnotation]; raw != "" {
		if state.WarmZones, err = ParseUint32List(raw); err != nil {
			return VolumeState{}, fmt.Errorf("volume %q: %s: %w", name, WarmZonesAnnotation, err)
		}
	}

	return state, nil
}

func optionalUint32Annotation(annotations map[string]string, key string) (uint32, error) {
	raw := annotations[key]
	if raw == "" {
		return 0, nil
	}

	value, err := ParseUint32(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}

	return value, nil
}
