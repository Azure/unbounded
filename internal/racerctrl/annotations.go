// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package racerctrl derives, validates and installs the RACER dataplane's node
// configuration.
//
// The dataplane in cmd/racer knows nothing but the file it is handed: no
// discovery, no membership protocol, no NVMe-oF, no Kubernetes. Everything it
// does is a function of that file, and the only feedback it offers is a
// Prometheus endpoint. This package is the half that decides what goes in the
// file.
//
// The split of responsibility is deliberate. Cluster-scoped state - universe
// ids, extent ids, address-space placement, catalog membership, epoch cursors -
// is agreed by exactly one writer, the unbounded-operator, and published on
// StorageClass and PersistentVolume annotations. Node-scoped state - a node's
// id, cohort, store footprint, exported devices and scraped health - lives on
// that node's own Node annotations and is written only by that node's agent.
// Derivation is then a pure function of those two, so every node reaches the
// same answer without talking to any other node.
package racerctrl

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// AnnotationDomain prefixes every annotation this package reads or writes.
const AnnotationDomain = "racer.unbounded-cloud.io/"

// DriverName is the CSI driver name. A StorageClass naming it as its
// provisioner is a racer universe, and a PersistentVolume naming it in
// spec.csi.driver is a racer volume; nothing else in the cluster is either.
const DriverName = "racer.unbounded-cloud.io"

// Node identity annotations. The operator is the single writer of these three:
// identity has to be allocated from one place or two nodes patching two
// different Node objects would both succeed with the same id. The node agent
// only ever reads them, on its own Node and on its peers'.
const (
	// NodeIDAnnotation is the node's RACER id, non-zero and unique in the
	// cluster. Assigned by the operator and frozen from then on: it names the
	// node in every universe's catalog and in every peer list.
	NodeIDAnnotation = AnnotationDomain + "node-id"

	// NodeCohortAnnotation is the node's cohort, "0", "1" or "2". Assigned by the
	// operator and frozen from then on, because a node's position in a trio is
	// normative.
	NodeCohortAnnotation = AnnotationDomain + "cohort"

	// NodeZoneAnnotation is the node's zone id, non-zero. A zone is a failure and
	// latency domain; catalogs are per zone. Assigned by the operator.
	NodeZoneAnnotation = AnnotationDomain + "zone"
)

// Node placement annotations. These are the only racer annotations a user or a
// higher-level controller writes. All three are optional, all three are inputs
// to placement rather than results of it, and all three are read only while a
// node is being placed: once the operator has stamped a zone and a cohort,
// changing one of these does not move the node. Drift is reported, never acted
// on, because moving a placed node means rewriting a catalog.
const (
	// NodeFabricIDAnnotation names the node's RDMA fabric. Two nodes carrying the
	// same value are assumed to be able to reach each other over RDMA; two nodes
	// carrying different values, or none at all, are assumed to need TCP.
	//
	// Placement treats it as a strong preference rather than a partition: a
	// quorum group inside one fabric is worth a lot, but a zone that could not
	// be formed without crossing one is better than no zone.
	NodeFabricIDAnnotation = AnnotationDomain + "fabric-id"

	// NodeRDMAAddrAnnotation is the address peers dial to reach this node's NVMe
	// target over RDMA, as `host` or `host:port`. It is supplied rather than
	// discovered because the RDMA-capable interface is rarely the one the
	// kubelet reports as the node address, and guessing wrong means a fabric
	// that silently falls back to TCP.
	//
	// Empty means this node does not serve RDMA, whatever fabric it names.
	NodeRDMAAddrAnnotation = AnnotationDomain + "rdma-addr"

	// NodeZoneNameAnnotation pins the node to a named zone, overriding automatic
	// placement. It takes precedence over everything: an operator who has
	// reasoned about a blast radius gets the blast radius they asked for.
	//
	// The name is interned to a numeric zone id exactly as the topology label
	// used to be, so two nodes naming the same zone land in the same catalog.
	NodeZoneNameAnnotation = AnnotationDomain + "zone-name"
)

// Node status annotations. The node agent is the single writer of these on its
// own Node; the operator and the node's peers only read them.
const (
	// NodeStoreBytesAnnotation is the store size the node has formatted for. It
	// only ever grows, and it is cold: a change takes effect when racer next
	// starts.
	NodeStoreBytesAnnotation = AnnotationDomain + "store-bytes"

	// NodeDevicesAnnotation lists the volumes this node exports, as
	// `<deviceID>?volume=<pvName>`. The device id is the ublk minor, so the path
	// is /dev/ublkb<deviceID>.
	NodeDevicesAnnotation = AnnotationDomain + "devices"

	// NodeFabricAnnotation lists this node's published fabric namespaces, as
	// `<universeID>?device=<deviceID>&nqn=<subsystemNQN>`. Peers read it to find
	// what to attach.
	NodeFabricAnnotation = AnnotationDomain + "fabric"

	// NodeHealthAnnotation carries what the agent scraped from racer, as a query
	// string. It is the operator's only view of the dataplane and gates every
	// sequenced operation.
	NodeHealthAnnotation = AnnotationDomain + "health"

	// NodeLiveAnnotation carries per-extent liveness, as
	// `<extentID>?pages=<n>&tombstones=<n>`. Tombstone collection and extent
	// migration are judged from it.
	NodeLiveAnnotation = AnnotationDomain + "live"

	// NodeAppliedAnnotation says which configuration the agent last installed
	// and what a sequencer needs to know about it, as
	// `generation?...` followed by `u<universeID>?epoch=<n>` and
	// `x<extentID>?next=<zone>&tombstone=<epoch>` entries.
	//
	// It is what turns the health annotation's generation into a statement
	// about content. Racer publishes the generation in force but nothing about
	// what is in it, so on its own a generation cannot tell the operator that a
	// node is acting on the catalog, migration or tombstone epoch it is waiting
	// for. This says generation G carried these facts; the health annotation
	// says G or later is running; together they are proof rather than
	// inference.
	NodeAppliedAnnotation = AnnotationDomain + "applied"
)

// StorageClass annotations. A StorageClass is a universe. Only the operator
// writes these.
const (
	// UniverseIDAnnotation is the universe's id: globally unique, never reused,
	// non-zero, below 2^26. Stamped once.
	UniverseIDAnnotation = AnnotationDomain + "universe-id"

	// CatalogSizeAnnotation is the catalog length, pinned when the universe is
	// first published and fixed for the life of every zone in it. The slot a
	// request hashes to selects its group as `slot % len(catalog)`, so changing
	// this would move every slot at once and the dataplane refuses it.
	CatalogSizeAnnotation = AnnotationDomain + "catalog-size"

	// EpochAnnotation is the universe's epoch, bumped whenever its topology
	// changes. Monotonic.
	EpochAnnotation = AnnotationDomain + "epoch"

	// NextLBAAnnotation is the universe's address-space bump cursor. It never
	// goes backwards and space is never reclaimed, which is what makes an extent
	// id and its placement safe to freeze for life.
	NextLBAAnnotation = AnnotationDomain + "next-lba"

	// GatewayCountAnnotation is how many of each zone's members other zones may
	// route through. Optional; DefaultGatewayCount when unset or unparseable as
	// a positive number. Capped at MaxGateways.
	//
	// It is the overlap knob: a wider list spreads cross-zone traffic over more
	// of a zone's edge and survives more gateway failures, at the cost of one
	// NVMe-oF controller per gateway per foreign zone on every node.
	GatewayCountAnnotation = AnnotationDomain + "gateway-count"

	// GatewaysAnnotationPrefix is followed by a zone id. The value is that zone's
	// gateway node ids, comma separated. A universe publishes each foreign zone's
	// gateways so cross-zone reads have somewhere to go.
	//
	// Gateways stay on the StorageClass while membership does not: a gateway list
	// is capped at MaxGateways entries per zone, so all 64 zones together are a
	// few kilobytes, whereas a single zone's membership is not.
	GatewaysAnnotationPrefix = AnnotationDomain + "gateways-zone-"
)

// PersistentVolume annotations. The geometry of a volume lives in
// spec.csi.volumeAttributes and is frozen by admission; these carry what the
// operator allocates and what an operator or administrator may still steer.
const (
	// CompositionAnnotation is the allocation result, stamped exactly once and
	// frozen: `<index>?extent=<id>&baseLba=<n>&pages=<n>&kind=<KIND>` in device
	// order. Nothing may rewrite it, because an extent's placement and size are
	// frozen for its life and a rewrite would silently remap live data.
	CompositionAnnotation = AnnotationDomain + "composition"

	// VolumeZoneAnnotation is the volume's home zone. It only ever moves to the
	// value NextZoneAnnotation previously held.
	VolumeZoneAnnotation = AnnotationDomain + "zone"

	// NextZoneAnnotation starts a migration when set to a zone the universe
	// knows and that differs from the home zone. The operator declares the
	// migration complete by moving zone to it and clearing this.
	NextZoneAnnotation = AnnotationDomain + "next-zone"

	// WarmZonesAnnotation lists zones to warm, comma separated. Immutable kinds
	// only, at most 16, excluding the home and next zones.
	WarmZonesAnnotation = AnnotationDomain + "warm-zones"

	// CacheAdmitAnnotation is the read-cache admission class, 0 to 15.
	CacheAdmitAnnotation = AnnotationDomain + "cache-admit"

	// TombstoneEpochAnnotation is the per-volume tombstone cursor. Advancing it
	// is destructive and never reversible, so the operator only does so once
	// every catalog node reports the extent holds no live pages.
	TombstoneEpochAnnotation = AnnotationDomain + "tombstone-epoch"

	// PhaseAnnotation tracks where a volume is in its lifecycle, so a restarted
	// operator resumes a destructive sequence rather than restarting it.
	PhaseAnnotation = AnnotationDomain + "phase"
)

// UniverseFinalizer holds a StorageClass until all of its volumes and membership
// state have been retired.
const UniverseFinalizer = "racer.unbounded-cloud.io/universe"

// VolumeFinalizer holds a PersistentVolume until its extents have been retired.
// Deleting a volume is not instant: the extents must first stop holding live
// pages, then the tombstone cursor advances, then the tombstones drain. Dropping
// the extents before that loses data that is still being read.
const VolumeFinalizer = "racer.unbounded-cloud.io/extents"

// Volume phases, in order.
const (
	// PhaseActive is a volume that may be exported and written.
	PhaseActive = "Active"

	// PhaseDraining is a deleted volume whose extents are no longer exported and
	// are being watched for live pages to reach zero.
	PhaseDraining = "Draining"

	// PhaseCollecting is a volume whose tombstone cursor has advanced and whose
	// tombstones are draining.
	PhaseCollecting = "Collecting"
)

// ListEntry is one item of a comma-separated annotation list, in the
// `item?key=value&key=value` form this repository already uses for node
// inventory annotations.
type ListEntry struct {
	Item   string
	Values url.Values
}

// ParseList splits an annotation list into its entries. An empty or blank value
// is an empty list rather than an error, so an absent annotation and an
// annotation set to "" mean the same thing.
func ParseList(raw string) ([]ListEntry, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	parts := strings.Split(raw, ",")
	entries := make([]ListEntry, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		item, query, hasQuery := strings.Cut(part, "?")

		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("empty item in annotation list %q", raw)
		}

		values := url.Values{}

		if hasQuery {
			parsed, err := url.ParseQuery(query)
			if err != nil {
				return nil, fmt.Errorf("parse query for %q: %w", item, err)
			}

			for key := range parsed {
				if strings.TrimSpace(key) == "" {
					return nil, fmt.Errorf("empty query key for %q", item)
				}
			}

			values = parsed
		}

		entries = append(entries, ListEntry{Item: item, Values: values})
	}

	return entries, nil
}

// FormatList renders entries back to the wire form. Query keys are sorted, so
// the same logical content always produces the same string and an unchanged
// annotation never provokes a write.
func FormatList(entries []ListEntry) string {
	parts := make([]string, 0, len(entries))

	for _, entry := range entries {
		if len(entry.Values) == 0 {
			parts = append(parts, entry.Item)

			continue
		}

		keys := make([]string, 0, len(entry.Values))
		for key := range entry.Values {
			keys = append(keys, key)
		}

		sort.Strings(keys)

		pairs := make([]string, 0, len(keys))

		for _, key := range keys {
			for _, value := range entry.Values[key] {
				pairs = append(pairs, url.QueryEscape(key)+"="+url.QueryEscape(value))
			}
		}

		parts = append(parts, entry.Item+"?"+strings.Join(pairs, "&"))
	}

	return strings.Join(parts, ",")
}

// FormatValues renders a bare query string with sorted keys, for annotations
// that carry one record rather than a list.
func FormatValues(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))

	for _, key := range keys {
		for _, value := range values[key] {
			pairs = append(pairs, url.QueryEscape(key)+"="+url.QueryEscape(value))
		}
	}

	return strings.Join(pairs, "&")
}

// ParseUint32 reads a decimal uint32 annotation value.
func ParseUint32(raw string) (uint32, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse %q as uint32: %w", raw, err)
	}

	return uint32(value), nil
}

// ParseUint64 reads a decimal uint64 annotation value.
func ParseUint64(raw string) (uint64, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %q as uint64: %w", raw, err)
	}

	return value, nil
}

// ParseUint32List reads a comma-separated list of decimal uint32 values.
func ParseUint32List(raw string) ([]uint32, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	parts := strings.Split(raw, ",")
	out := make([]uint32, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		value, err := ParseUint32(part)
		if err != nil {
			return nil, err
		}

		out = append(out, value)
	}

	return out, nil
}

// FormatUint32List renders a list of ids in the order given.
func FormatUint32List(values []uint32) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.FormatUint(uint64(value), 10))
	}

	return strings.Join(parts, ",")
}

// uint32Value reads a required uint32 out of a list entry's query.
func uint32Value(values url.Values, key string) (uint32, error) {
	raw := values.Get(key)
	if raw == "" {
		return 0, fmt.Errorf("missing %q", key)
	}

	return ParseUint32(raw)
}

// uint64Value reads a required uint64 out of a list entry's query.
func uint64Value(values url.Values, key string) (uint64, error) {
	raw := values.Get(key)
	if raw == "" {
		return 0, fmt.Errorf("missing %q", key)
	}

	return ParseUint64(raw)
}

// optionalUint64Value reads a uint64 that defaults to zero when absent.
func optionalUint64Value(values url.Values, key string) (uint64, error) {
	raw := values.Get(key)
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}

	return ParseUint64(raw)
}

// optionalUint32Value reads a uint32 that defaults to zero when absent.
func optionalUint32Value(values url.Values, key string) (uint32, error) {
	raw := values.Get(key)
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}

	return ParseUint32(raw)
}
