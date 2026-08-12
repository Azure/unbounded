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

	// Gateways is each zone's gateway node ids, keyed by zone id. A zone with no
	// entry falls back to its membership.
	Gateways map[uint32][]uint32

	// Volumes are the universe's volumes.
	Volumes []VolumeState
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
// either because its zone's catalog names it, or because it exports one of the
// universe's volumes and so has to route for it.
func (d *Derivation) joins(state *UniverseState, volumes map[string]struct{}) (bool, error) {
	if state.ID == 0 {
		return false, fmt.Errorf("storage class %q has no universe id yet", state.Class)
	}

	if state.Members[d.Self.Zone].Contains(d.Self.ID) {
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

	extents, err := d.deriveExtents(state)
	if err != nil {
		return nil, err
	}

	return &racerconfig.Universe{
		Id:             state.ID,
		Epoch:          state.Epoch,
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
func (d *Derivation) deriveExtents(state *UniverseState) ([]*racerconfig.Extent, error) {
	exported := d.Self.volumesByName()
	extents := make([]*racerconfig.Extent, 0)

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
				WarmZones:      warmZonesFor(volume),
			})
		}
	}

	// racer resolves an address by binary search, so the list has to be sorted by
	// base_lba, not by volume or by id.
	sort.Slice(extents, func(i, j int) bool { return extents[i].GetBaseLba() < extents[j].GetBaseLba() })

	return extents, nil
}

// warmZonesFor drops warm zones from kinds that cannot carry them. Warming
// copies pages to a zone that does not own them, which is only sound when the
// pages cannot change under the copy.
func warmZonesFor(volume *VolumeState) []uint32 {
	if len(volume.WarmZones) == 0 {
		return nil
	}

	warm := make([]uint32, 0, len(volume.WarmZones))

	for _, zone := range dedupeSorted(volume.WarmZones) {
		if zone == 0 || zone == volume.Zone || zone == volume.NextZone {
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
