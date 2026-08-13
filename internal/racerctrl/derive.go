// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import (
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

// Derivation.
//
// This is the whole of R3 in one function: cluster state in, one node's
// NodeConfig out, no I/O and no clock. Every node runs the same code over the
// same annotations, so the configs they produce agree without any of them
// talking to each other. That is what "hold derived state consistent across
// nodes" means in practice - not a protocol, but a pure function of shared
// inputs.
//
// Determinism is load-bearing rather than tidy. Anything that varied per node -
// map iteration order, a timestamp, a random tiebreak - would show up as two
// nodes disagreeing about a catalog, which is indistinguishable from a split
// brain. So every list here is sorted before it is emitted.

// NodeState is one node's published identity and status, read from its Node
// annotations. The operator writes ID, Cohort and Zone; the node itself writes
// the rest.
type NodeState struct {
	// Name is the Kubernetes Node name, used only for diagnostics and for
	// picking this node out of the cluster state.
	Name string

	ID     uint32
	Cohort uint32
	Zone   uint32

	// FabricID is the RDMA fabric the node declared, and RDMAAddr the address
	// peers reach it on over that fabric. Both are user-supplied and both are
	// optional. They are inputs to placement and to transport selection, never
	// results of either: the operator reads FabricID once when it places the
	// node, and every node reads its peers' pair to decide whether a link can
	// be RDMA or has to be TCP.
	FabricID string
	RDMAAddr string

	// StoreBytes is the store size the node has already formatted for.
	StoreBytes uint64

	// Devices are the volumes this node exports, keyed by the ublk minor it
	// exports them on.
	Devices []DeviceBinding

	// Fabric are the per-universe fabric namespaces this node publishes.
	Fabric []FabricExport

	// Health is what the agent last scraped from racer.
	Health Health

	// Live is per-extent liveness, keyed by extent id.
	Live map[uint32]LiveExtent

	// Applied is what the agent last wrote to this node's config file, and the
	// facts that configuration carried.
	Applied Applied
}

// Applied describes the configuration the agent last installed on a node.
//
// It exists because Health.Generation, on its own, says only that racer has
// some configuration in force; it does not say what is in it. Every sequenced
// operation waits on a fact the control plane put in a configuration - a
// catalog at an epoch, an extent pointed at a new zone, a tombstone epoch
// advanced - and the only way to know a node is acting on that fact is to know
// which generation carried it. The agent knows, because it wrote it.
//
// The comparison is therefore: the agent says generation G carried these facts,
// racer says generation G or later is in force, so racer is acting on them.
// This always describes the latest configuration written, never a history, so a
// fact that has since been withdrawn is simply not here.
type Applied struct {
	// Generation is the generation of the configuration this describes. Zero
	// means the agent has not published anything, which every gate reads as no
	// report rather than as agreement.
	Generation uint64

	// Epochs is the topology epoch each universe was published at, keyed by
	// universe id.
	Epochs map[uint32]uint32

	// Extents carries only the extents in the middle of a sequenced operation:
	// those with a migration destination or a tombstone epoch. Every other
	// extent is at rest and nothing waits on it, so carrying them all would
	// spend the node's annotation budget to say nothing.
	Extents map[uint32]AppliedExtent
}

// AppliedExtent is the part of an extent's configuration a sequencer waits on.
type AppliedExtent struct {
	NextZone       uint32
	TombstoneEpoch uint32
}

// DeviceBinding names a volume this node exports and the ublk minor it exports
// it on. The minor is the device id in the config, so the path is
// /dev/ublkb<DeviceID>.
type DeviceBinding struct {
	DeviceID uint32
	Volume   string
}

// FabricExport names the ublk minor a node publishes a universe's fabric
// namespace from, and how peers reach it.
//
// This is the one annotation whose consumer is always a different machine from
// its writer: every other node in the universe reads it to learn what to
// attach. NQN and Addr are empty until the node's fabric manager has actually
// created the target, so a peer that finds them empty simply has nothing to
// attach yet.
type FabricExport struct {
	// UniverseID is the universe this namespace carries.
	UniverseID uint32

	// DeviceID is the local ublk minor the namespace is backed by, so the
	// backing path is /dev/ublkb<DeviceID>.
	DeviceID uint32

	// NQN is the subsystem NQN peers attach.
	NQN string

	// Addr is the transport address, host:port, peers connect to over TCP.
	Addr string

	// RDMAAddr is the address, host:port, peers on the same fabric connect to
	// over RDMA. Empty when this node publishes no RDMA port, which is what a
	// peer reads as "dial me over TCP": it is advertised only once the port is
	// actually listening, so a node never invites traffic it cannot serve.
	RDMAAddr string
}

// Health is the subset of racer's metrics the control plane acts on.
type Health struct {
	Generation      uint64
	RejectedTotal   uint64
	Replaying       uint64
	Shedding        uint64
	UnbackedPages   uint64
	GroupsUnavail   uint64
	CoresPressured  uint64
	GatewayFallback uint64
}

// LiveExtent is one extent's liveness on one node.
type LiveExtent struct {
	Pages      uint64
	Tombstones uint64
}

// VolumeState is one PersistentVolume's racer state.
type VolumeState struct {
	// Name is the PersistentVolume name.
	Name string

	// Composition is the volume's allocated extents, in device order. Stamped
	// once and frozen.
	Composition Composition

	Zone           uint32
	NextZone       uint32
	WarmZones      []uint32
	CacheAdmit     uint32
	TombstoneEpoch uint32

	// Phase is the volume's lifecycle phase. The dataplane never sees it: it
	// exists so the control plane can tell a volume that is being served from
	// one whose extents are being collected, without inferring that from the
	// tombstone epoch.
	Phase string
}

// UniverseState is one StorageClass's racer state.
type UniverseState struct {
	// Class is the StorageClass name.
	Class string

	ID    uint32
	Epoch uint32

	// CatalogSize is len(catalog), pinned when the universe's first zone is
	// published and fixed for the life of that zone.
	CatalogSize int

	// GatewayCount is how many gateways each zone of this universe publishes,
	// which is how much a zone's edge overlaps with its neighbours. Zero means
	// DefaultGatewayCount.
	GatewayCount uint32

	// Members is each zone's catalog membership, keyed by zone id. It is filled
	// from one ConfigMap per zone rather than from the class's annotations: a
	// thousand-node zone is fourteen kilobytes and sixty-four of them do not fit
	// in one object.
	Members map[uint32]Membership

	// MemberEpochs is the topology epoch each zone's membership was published
	// at, keyed by zone id, taken from the same ConfigMap as the membership
	// itself. A zone with no entry has no published membership, or one from
	// before the epoch travelled with it; either way Epoch stands in.
	MemberEpochs map[uint32]uint32

	// Draining is each zone's departing nodes, keyed by zone id: nodes the
	// catalog no longer names but which have not yet handed over what they
	// held. They keep deriving the universe, so racer keeps shedding.
	Draining map[uint32]Membership

	// Gateways is each zone's gateway node ids, keyed by zone id. A zone with no
	// entry falls back to its membership.
	Gateways map[uint32][]uint32

	// Volumes are the universe's volumes.
	Volumes []VolumeState
}

// EpochFor is the topology epoch a node in the given zone runs this universe
// at: the epoch published alongside that zone's catalog, or the class's epoch
// when the zone has no membership of its own to date it.
//
// The epoch has to come from wherever the catalog came from. Taking it from the
// class instead would let a node pair a catalog it has just read with an epoch
// that was bumped for a different zone's change, and then read the next catalog
// under that same epoch: two topologies, one term, which is exactly what the
// epoch exists to prevent.
func (s *UniverseState) EpochFor(zone uint32) uint32 {
	if epoch := s.MemberEpochs[zone]; epoch != 0 {
		return epoch
	}

	return s.Epoch
}

// ClusterState is everything the operator has published, as any node sees it.
type ClusterState struct {
	Nodes     []NodeState
	Universes []UniverseState
}

// Attachment identifies one fabric link: a peer's namespace inside one universe.
type Attachment struct {
	Universe uint32
	Peer     uint32
}

// Derivation is one call's input: the shared cluster state, which node we are,
// and the fabric attachments only this node knows about.
type Derivation struct {
	Cluster ClusterState

	// Self is this node's own state.
	Self NodeState

	// Attachments maps a fabric link to the local device path the fabric manager
	// attached it at. R7 requires these paths to be stable: an unchanged path
	// keeps its open fd across a reload.
	Attachments map[Attachment]string

	// Established names the universes this node already serves in full, so a
	// universe that has been published with peers is never demoted back to its
	// bootstrap shape. Callers build it from the config they last published;
	// see EstablishedUniverses.
	Established map[uint32]bool

	// Generation is the generation to stamp. R1 requires it to strictly increase
	// per node, so callers pass the previous generation plus one.
	Generation uint64
}

// Derive builds this node's NodeConfig. The result is not validated; callers run
// Validate and ValidateTransition before publishing it.
func Derive(d Derivation) (*racerconfig.NodeConfig, error) {
	if d.Self.ID == 0 {
		return nil, fmt.Errorf("node %q has no racer id yet", d.Self.Name)
	}

	if d.Self.Zone == 0 {
		return nil, fmt.Errorf("node %q has no zone yet", d.Self.Name)
	}

	if d.Self.Cohort >= Cohorts {
		return nil, fmt.Errorf("node %q has cohort %d, which is out of range", d.Self.Name, d.Self.Cohort)
	}

	volumes := d.Self.volumesByName()
	fabricIDs := d.Self.fabricDeviceIDs()

	full := make([]*racerconfig.Universe, 0, len(d.Cluster.Universes))

	for i := range d.Cluster.Universes {
		state := &d.Cluster.Universes[i]

		joined, err := d.joins(state, volumes)
		if err != nil {
			return nil, err
		}

		if !joined {
			continue
		}

		universe, err := d.deriveUniverse(state, fabricIDs)
		if err != nil {
			return nil, fmt.Errorf("universe %q: %w", state.Class, err)
		}

		full = append(full, universe)
	}

	sort.Slice(full, func(i, j int) bool { return full[i].GetId() < full[j].GetId() })

	universes, bootstrapping := d.publishable(full)

	devices, err := d.deriveDevices(universes, bootstrapping)
	if err != nil {
		return nil, err
	}

	cohort := racerconfig.Cohort(d.Self.Cohort)

	// Sizing is computed from the full shape even when a universe is published
	// in its bootstrap shape. The store is cold - racer formats it at startup
	// and only a restart picks up a larger one - so sizing it to the inert
	// config would mean every bootstrap ended in a restart to grow it back.
	pages := CountStorePages(d.Self.Zone, full)

	return &racerconfig.NodeConfig{
		Generation: d.Generation,
		Node: &racerconfig.Node{
			Id:     d.Self.ID,
			Zone:   d.Self.Zone,
			Cohort: &cohort,
			Store: &racerconfig.Store{
				SizeBytes: GrowStore(d.Self.StoreBytes, StoreSizeBytes(pages)),
			},
		},
		Universes: universes,
		Devices:   devices,
		Policy:    derivePolicy(full),
	}, nil
}

// EstablishedUniverses names the universes a config already serves in full.
// Callers pass it back through Derivation.Established so that a universe which
// has once been published with peers is never demoted to its bootstrap shape.
func EstablishedUniverses(cfg *racerconfig.NodeConfig) map[uint32]bool {
	universes := cfg.GetUniverses()

	established := make(map[uint32]bool, len(universes))

	for _, universe := range universes {
		if len(universe.GetPeers()) > 0 || len(universe.GetZones()) > 0 || len(universe.GetExtents()) > 0 {
			established[universe.GetId()] = true
		}
	}

	return established
}

// AppliedFrom reads back the facts a sequencer waits on out of the
// configuration that carries them.
//
// The agent calls this on the file it has just installed, so that what it
// publishes about itself describes a configuration that exists rather than one
// it intends. Only the extents in the middle of an operation are carried: an
// extent with a migration destination or a tombstone epoch. Everything else is
// at rest, and a per-extent entry for every extent on a node would spend the
// whole annotation budget restating that.
func AppliedFrom(cfg *racerconfig.NodeConfig) Applied {
	universes := cfg.GetUniverses()

	applied := Applied{
		Generation: cfg.GetGeneration(),
		Epochs:     make(map[uint32]uint32, len(universes)),
		Extents:    map[uint32]AppliedExtent{},
	}

	for _, universe := range universes {
		applied.Epochs[universe.GetId()] = universe.GetEpoch()

		for _, extent := range universe.GetExtents() {
			if extent.GetNextZone() == 0 && extent.GetTombstoneEpoch() == 0 {
				continue
			}

			applied.Extents[extent.GetId()] = AppliedExtent{
				NextZone:       extent.GetNextZone(),
				TombstoneEpoch: extent.GetTombstoneEpoch(),
			}
		}
	}

	return applied
}

// publishable replaces a universe this node cannot serve yet with the inert
// shape that lets racer create its fabric device, and reports which universes
// were replaced.
//
// This is the bootstrap half of R7, and it exists because the fabric is
// circular on a cold cluster. A peer link is an NVMe-oF namespace backed by
// /dev/ublkb<fabric_device_id>, and racer creates that device only once it has
// accepted a config naming the universe. A first config that insisted on peers
// could therefore never be accepted: nothing would have created the devices its
// peers are attached from, and every node would sit waiting for the others. So
// the first config a node publishes for a universe names the catalog and the
// fabric device id and nothing else - no zones, no peers, no extents - which
// carries no data and answers no reads. It exists only to bring the fabric
// devices up. Once the attachments land, the next derivation publishes the
// universe in full.
//
// Only a universe this node has never served in full is demoted. An established
// one keeps its full shape and renders degraded when a peer drops, which is
// what racer expects of a live group; quietly dropping its extents instead
// would take data away from a node that is serving it.
func (d *Derivation) publishable(universes []*racerconfig.Universe) ([]*racerconfig.Universe, map[uint32]bool) {
	out := make([]*racerconfig.Universe, 0, len(universes))
	bootstrapping := make(map[uint32]bool)

	for _, universe := range universes {
		if d.Established[universe.GetId()] || fabricSatisfied(universe) {
			out = append(out, universe)

			continue
		}

		bootstrapping[universe.GetId()] = true

		out = append(out, &racerconfig.Universe{
			Id:             universe.GetId(),
			Epoch:          universe.GetEpoch(),
			Catalog:        universe.GetCatalog(),
			FabricDeviceId: universe.GetFabricDeviceId(),
		})
	}

	return out, bootstrapping
}

// fabricSatisfied reports whether every fabric link a universe's full shape
// depends on has been attached. It mirrors the two rules validation applies to
// a universe that carries data: a replicated catalog needs peers, and every
// remote zone needs a gateway this node holds a link to.
func fabricSatisfied(universe *racerconfig.Universe) bool {
	peers := make(map[uint32]struct{}, len(universe.GetPeers()))
	for _, peer := range universe.GetPeers() {
		peers[peer.GetId()] = struct{}{}
	}

	if len(peers) == 0 && catalogNodeCount(universe.GetCatalog()) > 1 {
		return false
	}

	for _, zone := range universe.GetZones() {
		reachable := false

		for _, gateway := range zone.GetGateways() {
			if _, ok := peers[gateway]; ok {
				reachable = true

				break
			}
		}

		if !reachable {
			return false
		}
	}

	return true
}

// joins reports whether this node takes part in a universe at all. A node joins
// because its zone's catalog names it, because the catalog has just stopped
// naming it and it is still draining, or because it exports one of the
// universe's volumes and so has to route for it.
//
// The draining case is the one that is not obvious. A node dropped from a
// catalog has to keep running that universe, with itself absent from the
// catalog, because that is the configuration that tells racer the groups it
// holds are no longer its own. Stop deriving the universe and the node keeps
// the configuration that still names it, sheds nothing, and holds its registers
// until the process ends.
func (d *Derivation) joins(state *UniverseState, volumes map[string]struct{}) (bool, error) {
	if state.ID == 0 {
		return false, fmt.Errorf("storage class %q has no universe id yet", state.Class)
	}

	if state.Members[d.Self.Zone].Contains(d.Self.ID) {
		return true, nil
	}

	if state.Draining[d.Self.Zone].Contains(d.Self.ID) {
		return true, nil
	}

	for _, volume := range state.Volumes {
		if _, ok := volumes[volume.Name]; ok {
			return true, nil
		}
	}

	return false, nil
}

func (d *Derivation) deriveUniverse(state *UniverseState, fabricIDs map[uint32]uint32) (*racerconfig.Universe, error) {
	fabricID, ok := fabricIDs[state.ID]
	if !ok {
		return nil, fmt.Errorf("no local fabric device id assigned")
	}

	catalog, err := BuildCatalog(state.Members[d.Self.Zone], state.CatalogSize)
	if err != nil {
		return nil, err
	}

	zones := d.deriveZones(state)

	extents, err := d.deriveExtents(state, zones)
	if err != nil {
		return nil, err
	}

	return &racerconfig.Universe{
		Id:             state.ID,
		Epoch:          state.EpochFor(d.Self.Zone),
		Catalog:        catalog,
		Zones:          zones,
		Peers:          d.derivePeers(state, catalog, zones),
		Extents:        extents,
		FabricDeviceId: fabricID,
	}, nil
}

// deriveZones lists the universe's other zones with their gateways. Our own zone
// is deliberately absent: racer reaches it through the catalog, not through a
// gateway.
//
// The zones are enumerated from the published gateway lists rather than from
// the memberships, because a node only ever holds its own zone's membership: a
// zone's members are its whole catalog and shipping every zone's to every node
// is what the per-zone ConfigMaps exist to avoid. A zone with no gateways is
// unreachable, so a zone that is missing here is a zone racer could not have
// routed to anyway.
func (d *Derivation) deriveZones(state *UniverseState) []*racerconfig.Zone {
	ids := make([]uint32, 0, len(state.Gateways))

	for zone, gateways := range state.Gateways {
		if zone == d.Self.Zone || len(gateways) == 0 {
			continue
		}

		ids = append(ids, zone)
	}

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	zones := make([]*racerconfig.Zone, 0, len(ids))

	for _, id := range ids {
		gateways := dedupeSorted(state.Gateways[id])
		if len(gateways) > MaxGateways {
			gateways = gateways[:MaxGateways]
		}

		zones = append(zones, &racerconfig.Zone{Id: id, Gateways: gateways})
	}

	return zones
}

// derivePeers lists the fabric links this node holds inside a universe: the
// other members of our own zone's catalog, plus every other zone's gateways. A
// link with no attachment yet is left out rather than named with an empty path;
// the fabric manager will attach it and the next derivation will pick it up.
func (d *Derivation) derivePeers(
	state *UniverseState,
	catalog []*racerconfig.Trio,
	zones []*racerconfig.Zone,
) []*racerconfig.Peer {
	wanted := make(map[uint32]struct{})

	for id := range catalogMembers(&racerconfig.Universe{Catalog: catalog}) {
		if id != d.Self.ID {
			wanted[id] = struct{}{}
		}
	}

	for _, zone := range zones {
		for _, gateway := range zone.GetGateways() {
			if gateway != d.Self.ID {
				wanted[gateway] = struct{}{}
			}
		}
	}

	ids := make([]uint32, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}

	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	peers := make([]*racerconfig.Peer, 0, len(ids))

	for _, id := range ids {
		device, ok := d.Attachments[Attachment{Universe: state.ID, Peer: id}]
		if !ok || device == "" {
			continue
		}

		peers = append(peers, &racerconfig.Peer{Id: id, Device: device})
	}

	return peers
}

// deriveExtents ships this node only the extents it has business knowing about:
// the ones homed in our zone, which we store, and the ones behind a device we
// export, which we route for. An extent we neither store nor route for would
// only cost index space.
func (d *Derivation) deriveExtents(
	state *UniverseState,
	zones []*racerconfig.Zone,
) ([]*racerconfig.Extent, error) {
	exported := d.Self.volumesByName()
	extents := make([]*racerconfig.Extent, 0)

	// A warm zone racer does not know is a rejected config, and a rejected
	// config leaves the node running the previous generation forever, so the
	// reachable set is computed once and every extent is filtered through it.
	reachable := map[uint32]struct{}{d.Self.Zone: {}}
	for _, zone := range zones {
		reachable[zone.GetId()] = struct{}{}
	}

	for i := range state.Volumes {
		volume := &state.Volumes[i]

		_, local := exported[volume.Name]
		homed := volume.Zone == d.Self.Zone || volume.NextZone == d.Self.Zone

		if !local && !homed {
			continue
		}

		if volume.Zone == 0 {
			return nil, fmt.Errorf("volume %q has no home zone", volume.Name)
		}

		for _, segment := range volume.Composition {
			extents = append(extents, &racerconfig.Extent{
				Id:             segment.ExtentID,
				BaseLba:        segment.BaseLBA,
				Pages:          segment.Pages,
				Kind:           segment.Kind,
				Zone:           volume.Zone,
				NextZone:       volume.NextZone,
				TombstoneEpoch: volume.TombstoneEpoch,
				CacheAdmit:     volume.CacheAdmit,
				WarmZones:      warmZonesFor(volume, segment.Kind, reachable),
			})
		}
	}

	// racer resolves an address by binary search, so the list has to be sorted by
	// base_lba, not by volume or by id.
	sort.Slice(extents, func(i, j int) bool { return extents[i].GetBaseLba() < extents[j].GetBaseLba() })

	return extents, nil
}

// warmZonesFor picks the warm zones one segment may carry.
//
// Warming copies pages to a zone that does not own them, which is only sound
// when the pages cannot change under the copy: an immutable page's version is a
// function of its extent's tombstone epoch, so a remote reader can believe a
// copy on sight. racer refuses a whole config that names warm zones on any
// other kind, so a volume with a mutable head and an immutable tail - which is
// the shape every volume with a mutable head has - would be unpublishable if
// the volume's warm list were applied to both. The head simply gets none.
//
// The zone also has to be one the universe knows, for the same reason: racer
// refuses the config otherwise, and a rejected config is not a warning, it is a
// node stuck at its previous generation with no way to say so.
func warmZonesFor(volume *VolumeState, kind racerconfig.Kind, reachable map[uint32]struct{}) []uint32 {
	if len(volume.WarmZones) == 0 || !KindIsImmutable(kind) {
		return nil
	}

	warm := make([]uint32, 0, len(volume.WarmZones))

	for _, zone := range dedupeSorted(volume.WarmZones) {
		if zone == 0 || zone == volume.Zone || zone == volume.NextZone {
			continue
		}

		if _, ok := reachable[zone]; !ok {
			continue
		}

		warm = append(warm, zone)
	}

	if len(warm) > MaxWarmZones {
		warm = warm[:MaxWarmZones]
	}

	return warm
}

func (d *Derivation) deriveDevices(
	universes []*racerconfig.Universe,
	bootstrapping map[uint32]bool,
) ([]*racerconfig.Device, error) {
	if len(d.Self.Devices) == 0 {
		return nil, nil
	}

	compositions := make(map[string]Composition)
	universeOf := make(map[string]uint32)

	for i := range d.Cluster.Universes {
		state := &d.Cluster.Universes[i]
		for j := range state.Volumes {
			compositions[state.Volumes[j].Name] = state.Volumes[j].Composition
			universeOf[state.Volumes[j].Name] = state.ID
		}
	}

	carried := make(map[uint32]struct{})

	for _, universe := range universes {
		for _, extent := range universe.GetExtents() {
			carried[extent.GetId()] = struct{}{}
		}
	}

	bindings := append([]DeviceBinding(nil), d.Self.Devices...)
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].DeviceID < bindings[j].DeviceID })

	devices := make([]*racerconfig.Device, 0, len(bindings))

	for _, binding := range bindings {
		// The volume's universe is still bringing its fabric device up, so this
		// config carries none of its extents yet. Leave the device unexported
		// rather than failing the render: failing would take the bootstrap
		// config down with it, and the bootstrap config is what creates the
		// fabric device this volume is waiting on. The derivation that
		// publishes the universe in full picks the binding up.
		if bootstrapping[universeOf[binding.Volume]] {
			continue
		}

		composition, ok := compositions[binding.Volume]
		if !ok {
			return nil, fmt.Errorf(
				"device %d exports volume %q, which no storage class carries",
				binding.DeviceID, binding.Volume,
			)
		}

		if len(composition) == 0 {
			return nil, fmt.Errorf("device %d exports volume %q, which has no composition", binding.DeviceID, binding.Volume)
		}

		for _, segment := range composition {
			if _, ok := carried[segment.ExtentID]; !ok {
				return nil, fmt.Errorf(
					"device %d exports volume %q, whose extent %d this config does not carry",
					binding.DeviceID, binding.Volume, segment.ExtentID,
				)
			}
		}

		devices = append(devices, &racerconfig.Device{
			Id:      binding.DeviceID,
			Extents: composition.ExtentIDs(),
		})
	}

	return devices, nil
}

// derivePolicy sizes the page index to the pages this node actually carries.
// Leaving it at the schema default would be a silent cap: racer refuses a config
// whose index cannot hold the pages it names, so the control plane has to size
// it rather than hope.
func derivePolicy(universes []*racerconfig.Universe) *racerconfig.Policy {
	const defaultMaxIndexBytes uint64 = 8 << 30

	var pages uint64

	for _, universe := range universes {
		for _, extent := range universe.GetExtents() {
			if !KindIsHuge(extent.GetKind()) {
				pages += extent.GetPages()
			}
		}
	}

	needed := pages * IndexEntryBytes
	if needed < defaultMaxIndexBytes {
		needed = defaultMaxIndexBytes
	}

	return &racerconfig.Policy{MaxIndexBytes: proto.Uint64(alignUp(needed, 1<<20))}
}

func (n *NodeState) volumesByName() map[string]struct{} {
	volumes := make(map[string]struct{}, len(n.Devices))
	for _, binding := range n.Devices {
		volumes[binding.Volume] = struct{}{}
	}

	return volumes
}

func (n *NodeState) fabricDeviceIDs() map[uint32]uint32 {
	ids := make(map[uint32]uint32, len(n.Fabric))
	for _, export := range n.Fabric {
		ids[export.UniverseID] = export.DeviceID
	}

	return ids
}

func dedupeSorted(ids []uint32) []uint32 {
	if len(ids) == 0 {
		return nil
	}

	sorted := append([]uint32(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	out := sorted[:1]

	for _, id := range sorted[1:] {
		if id != out[len(out)-1] {
			out = append(out, id)
		}
	}

	return out
}
