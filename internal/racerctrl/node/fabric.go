// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"crypto/sha1" //nolint:gosec // used to derive a stable name-based UUID, not for security
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/Azure/unbounded/internal/racerctrl"
)

// Fabric manages the NVMe-oF plumbing racer refuses to know about.
//
// R7 makes two demands, and the second is the one that matters. Each node
// publishes one namespace per universe it joins, backed by the local fabric
// ublk device racer exports for that universe, and attaches the corresponding
// namespace from every peer in that universe, recording the resulting local
// device path so it can be rendered into Peer.device.
//
// The demand that matters: attachment is the entire security boundary. The
// fabric provides no authentication, no authorization, no encryption, no peer
// identity and no replay protection. A node that can attach a universe's
// namespace can read and write every page in it, so attaching the wrong one is
// not a misconfiguration, it is privilege escalation. Every subsystem published
// here therefore sets attr_allow_any_host to 0 and carries an explicit
// allowed_hosts list holding exactly the host NQNs of that universe's members.
// Removing a node from a universe removes its host NQN here in the same pass.
type Fabric struct {
	// nvmetRoot is the nvmet configfs root, normally
	// /sys/kernel/config/nvmet. Injectable so the target half can be tested
	// against a temporary directory: every operation on it is an ordinary
	// mkdir, write or symlink.
	nvmetRoot string

	// sysClassNvme is where attached controllers appear, normally
	// /sys/class/nvme.
	sysClassNvme string

	// connector performs the initiator-side connect and disconnect. It is an
	// interface because writing to /dev/nvme-fabrics has no filesystem
	// equivalent that can be faked.
	connector Connector

	nodeID    uint32
	nqnPrefix string
	addr      string
	port      int

	// rdmaAddr is the address peers on this node's fabric reach it on over
	// RDMA, empty when the node declares no fabric. It comes from a node
	// annotation rather than the environment because the RDMA NIC is not the
	// interface the kubelet reports, and because it changes without the pod
	// restarting.
	rdmaAddr string
	rdmaPort int

	// rdmaLive records whether the RDMA port came up in the last pass. It
	// gates what is advertised to peers, so a node only invites RDMA traffic
	// it can actually serve.
	rdmaLive bool

	// tcpPortID and rdmaPortID are the configfs port directory ids resolved
	// for this node's two listeners. They are cached because resolution is a
	// search of the whole ports tree, and because everything downstream of
	// ensurePorts (linking, pruning) has to name the same directory the
	// address was written into.
	tcpPortID  int
	rdmaPortID int

	// devicePresent says whether the ublk block device backing an export is
	// there. It is a field so tests can drive the target half without any ublk
	// device, and it exists at all because an export outliving its device is
	// not a harmless stale entry: see ensureNamespace.
	devicePresent func(path string) bool
}

// sysClassNvmeSubsystem is where the multipath head of an attached namespace
// appears. It is a package variable rather than a field so tests can point it
// at a fixture; nothing else varies it.
var sysClassNvmeSubsystem = "/sys/class/nvme-subsystem"

// Connector abstracts the initiator side of NVMe-oF.
type Connector interface {
	// Connect establishes a controller for the given subsystem. It must be
	// idempotent: connecting an already-connected subsystem is a no-op.
	Connect(req ConnectRequest) error

	// Disconnect tears down the named controller, for example "nvme3".
	Disconnect(controller string) error
}

// ConnectRequest is one initiator connection.
type ConnectRequest struct {
	NQN     string
	HostNQN string

	// HostID is the NVMe host identifier presented alongside HostNQN.
	//
	// It is not optional. The kernel keeps one table of hosts keyed by id and
	// refuses a connect that presents an id it already knows under a different
	// NQN, and an omitted id defaults to the machine-wide one, so a connect
	// that carries a custom host NQN and no id is rejected outright with
	// EINVAL as soon as anything else on the box has connected.
	HostID string

	Traddr   string
	Trsvcid  string
	Trtype   string
	Adrfam   string
	HostAddr string
}

// FabricExportRequest asks for one published namespace.
type FabricExportRequest struct {
	// UniverseID names the universe whose fabric device backs the namespace.
	UniverseID uint32

	// DeviceID is the local ublk minor racer exports the universe's fabric
	// device on, so the backing path is /dev/ublkb<DeviceID>.
	DeviceID uint32

	// AllowedNodes are the node ids admitted to this universe. Anything not
	// listed is refused by the target, which is the whole security boundary.
	AllowedNodes []uint32
}

// FabricImportRequest asks for one attached remote namespace.
type FabricImportRequest struct {
	UniverseID uint32
	PeerNodeID uint32
	NQN        string
	Addr       string

	// Trtype is the transport to dial the peer over, "tcp" or "rdma". The
	// planner picks it from whether the two nodes declare the same fabric,
	// and the address above is the one that transport listens on, so the two
	// always travel together.
	Trtype string
}

// FabricPlan is the complete desired fabric state for this node. Like the
// config file itself it is whole-state: anything published or attached that the
// plan does not name is torn down.
type FabricPlan struct {
	Exports []FabricExportRequest
	Imports []FabricImportRequest
}

// FabricState is what the fabric actually looks like after a reconcile.
type FabricState struct {
	// Exports is what was published, with the NQN and address peers need in
	// order to attach. This is what the node writes into its fabric
	// annotation.
	Exports []racerctrl.FabricExport

	// Attachments maps a (universe, peer) link to the local device path the
	// remote namespace showed up as. This is what becomes Peer.device.
	Attachments map[racerctrl.Attachment]string
}

// Transports racer's fabric traffic may take.
//
// TCP is the floor: it is available on every node the project targets and needs
// nothing of the network but reachability. RDMA is offered alongside it rather
// than instead of it, because reachability over RDMA is a property of the
// physical fabric a node is cabled into and not of the cluster. A node that
// declares a fabric publishes both, and a peer on the same fabric dials the
// RDMA one; everyone else dials TCP and is none the wiser.
const (
	fabricTrtypeTCP  = "tcp"
	fabricTrtypeRDMA = "rdma"

	adrfamIPv4 = "ipv4"
	adrfamIPv6 = "ipv6"

	// fabricNamespaceID is the namespace number inside each subsystem. Each
	// subsystem carries exactly one namespace (one universe's fabric device),
	// so this never needs to vary.
	fabricNamespaceID = "1"
)

// NewFabric builds a fabric manager from the agent configuration.
func NewFabric(cfg Config, nodeID uint32, connector Connector) *Fabric {
	if connector == nil {
		connector = fabricsConnector{path: fabricsControlPath}
	}

	return &Fabric{
		nvmetRoot:     cfg.NvmetRoot,
		sysClassNvme:  "/sys/class/nvme",
		connector:     connector,
		nodeID:        nodeID,
		nqnPrefix:     cfg.NQNPrefix,
		addr:          cfg.FabricAddr,
		port:          cfg.FabricPort,
		rdmaPort:      cfg.RDMAPort,
		devicePresent: blockDevicePresent,
	}
}

// blockDevicePresent reports whether a ublk block device node exists.
//
// The ublk driver deletes the block device as soon as the process serving it
// goes away, however it goes away, so the presence of the node is a faithful
// answer to "is the dataplane serving this export right now".
func blockDevicePresent(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

// SetRDMAAddr declares the address this node's RDMA target listens on, or
// clears it when empty.
//
// It is a setter rather than a constructor argument because the address lives
// in a node annotation the operator or an administrator may change at any time,
// while the fabric manager is built once at startup. Calling it with the same
// value twice is free; changing it republishes the RDMA port on the next
// reconcile.
func (f *Fabric) SetRDMAAddr(addr string) {
	f.rdmaAddr = addr
}

// SubsystemNQN is the NQN a node publishes a universe under.
//
// Both the universe and the publishing node appear in the name because a
// universe's namespace is published once per member: an initiator has to be
// able to name which member's copy it wants.
func (f *Fabric) SubsystemNQN(universe, node uint32) string {
	return fmt.Sprintf("%s.u%d.n%d", f.nqnPrefix, universe, node)
}

// HostNQN is the NQN a node connects as. It carries no universe: a node has one
// identity, and which universes that identity may reach is decided by the
// allowed_hosts list on each subsystem rather than by the name it presents.
func (f *Fabric) HostNQN(node uint32) string {
	return fmt.Sprintf("%s.host.n%d", f.nqnPrefix, node)
}

// hostIDNamespace is the UUID namespace racer derives NVMe host ids in. It is
// an arbitrary but fixed value whose only job is to keep ids derived from a
// racer host NQN from colliding with ids derived from the same string
// elsewhere.
var hostIDNamespace = [16]byte{
	0x2f, 0x1b, 0x9c, 0x4e, 0x7a, 0x63, 0x4c, 0x8d,
	0x9e, 0x0f, 0x5b, 0xa2, 0xd7, 0x36, 0x81, 0x44,
}

// HostID is the NVMe host identifier a node presents when it connects.
//
// The kernel refuses a host NQN that arrives with an id it has already seen
// under a different name, and an omitted id is the machine-wide default, so a
// node that presents its own NQN has to present its own id with it. The id is
// derived from the NQN rather than generated so that it is the same after a
// restart: a reconnect then lands on the existing host entry instead of leaving
// a new one behind on every pod lifetime.
func (f *Fabric) HostID(node uint32) string {
	return derivedUUID(hostIDNamespace, f.HostNQN(node))
}

// namespaceUUIDNamespace is the UUID namespace racer derives NVMe namespace
// identifiers in. It is a different arbitrary constant from hostIDNamespace so
// that a subsystem NQN and a host NQN can never name the same identifier.
var namespaceUUIDNamespace = [16]byte{
	0x8c, 0x74, 0x2d, 0x11, 0x35, 0xe8, 0x4b, 0x1a,
	0xa6, 0x52, 0xc3, 0x90, 0x1f, 0x4d, 0x77, 0x0b,
}

// NamespaceUUID is the identifier this node publishes for a subsystem's single
// namespace. It is derived from the subsystem NQN so that the namespace keeps
// its identity across agent restarts; see ensureNamespace for why a peer that
// sees it change never recovers.
func (f *Fabric) NamespaceUUID(nqn string) string {
	return derivedUUID(namespaceUUIDNamespace, nqn)
}

// derivedUUID is RFC 4122's name-based (version 5) UUID.
//
// SHA-1 here is a naming function, not a security one: the input is a public
// node identifier and nothing is authenticated by the result.
func derivedUUID(namespace [16]byte, name string) string {
	sum := sha1.Sum(append(namespace[:], name...)) //nolint:gosec // name-based identifier, not a digest of secrets

	var u [16]byte

	copy(u[:], sum[:16])

	u[6] = (u[6] & 0x0f) | 0x50
	u[8] = (u[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// Address is the transport address peers dial to reach this node's TCP target.
func (f *Fabric) Address() string {
	return net.JoinHostPort(f.addr, strconv.Itoa(f.port))
}

// RDMAAddress normalises an rdma-addr annotation into the address to dial.
//
// A bare host is completed with this node's RDMA service id rather than the
// peer's, which is unknowable from here. That is sound because the service id
// is a cluster-wide setting: an administrator who moves it moves it everywhere,
// and one who does not never sees it.
func (f *Fabric) RDMAAddress(addr string) string {
	if addr == "" {
		return ""
	}

	host, port := splitRDMAAddr(addr, f.rdmaPort)

	return net.JoinHostPort(host, strconv.Itoa(port))
}

// Reconcile drives the fabric to the plan and reports what it achieved.
//
// It is deliberately tolerant: a failure to publish or attach one link is
// recorded and the rest of the plan is still applied. A universe whose
// attachment failed simply has no entry in the returned Attachments, and the
// caller renders a config without that peer, which racer accepts (a group short
// a replica is degraded, not invalid). The alternative, refusing to render
// anything until every link is up, would let one unreachable node stall the
// whole zone.
func (f *Fabric) Reconcile(plan FabricPlan) (FabricState, error) {
	state := FabricState{Attachments: map[racerctrl.Attachment]string{}}

	var problems []error

	// Ports first: they are shared by every subsystem, and whether RDMA came
	// up decides what the exports below advertise.
	if err := f.ensurePorts(); err != nil {
		problems = append(problems, err)
	}

	for _, export := range plan.Exports {
		nqn := f.SubsystemNQN(export.UniverseID, f.nodeID)

		if err := f.publish(nqn, export); err != nil {
			problems = append(problems, fmt.Errorf("publish universe %d: %w", export.UniverseID, err))
			continue
		}

		state.Exports = append(state.Exports, racerctrl.FabricExport{
			UniverseID: export.UniverseID,
			DeviceID:   export.DeviceID,
			NQN:        nqn,
			Addr:       f.Address(),
			RDMAAddr:   f.AdvertisedRDMAAddr(),
		})
	}

	if err := f.pruneSubsystems(plan); err != nil {
		problems = append(problems, err)
	}

	for _, imp := range plan.Imports {
		path, err := f.attach(imp)
		if err != nil {
			problems = append(problems, fmt.Errorf(
				"attach universe %d peer %d: %w", imp.UniverseID, imp.PeerNodeID, err))

			continue
		}

		state.Attachments[racerctrl.Attachment{Universe: imp.UniverseID, Peer: imp.PeerNodeID}] = path
	}

	if err := f.pruneControllers(plan); err != nil {
		problems = append(problems, err)
	}

	return state, errors.Join(problems...)
}

// publish creates or updates one subsystem and its single namespace.
func (f *Fabric) publish(nqn string, export FabricExportRequest) error {
	subsystem := filepath.Join(f.nvmetRoot, "subsystems", nqn)

	if err := ensureDir(subsystem); err != nil {
		return err
	}

	// configfs materializes these when the subsystem is created, so the mkdirs
	// below are no-ops there. They are here so that the tree this code needs is
	// stated rather than assumed, and so it can be driven against an ordinary
	// directory.
	for _, child := range []string{"allowed_hosts", "namespaces"} {
		if err := ensureDir(filepath.Join(subsystem, child)); err != nil {
			return err
		}
	}

	// Deny by default before the namespace is enabled, so there is no window
	// in which the device is reachable by anything that asks.
	if err := writeAttr(filepath.Join(subsystem, "attr_allow_any_host"), "0"); err != nil {
		return err
	}

	if err := f.ensureAllowedHosts(subsystem, export.AllowedNodes); err != nil {
		return err
	}

	if err := f.ensureNamespace(subsystem, nqn, racerctrl.BlockDevicePath(export.DeviceID)); err != nil {
		return err
	}

	return f.linkSubsystem(nqn)
}

// ensureAllowedHosts makes the subsystem's allowed_hosts exactly the given node
// ids. Both halves matter: the symlinks that should be there are created, and
// any that should not be are removed, because a stale symlink is a node that
// was evicted from the universe but can still read every page in it.
func (f *Fabric) ensureAllowedHosts(subsystem string, nodes []uint32) error {
	allowedDir := filepath.Join(subsystem, "allowed_hosts")

	want := make(map[string]struct{}, len(nodes))

	for _, id := range nodes {
		if id == 0 {
			continue
		}

		want[f.HostNQN(id)] = struct{}{}
	}

	// The node always admits itself: racer reads its own fabric device through
	// the same path as any peer's, so a universe of one still needs a link.
	want[f.HostNQN(f.nodeID)] = struct{}{}

	existing, err := os.ReadDir(allowedDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", allowedDir, err)
	}

	for _, entry := range existing {
		if _, keep := want[entry.Name()]; keep {
			delete(want, entry.Name())
			continue
		}

		if err := os.Remove(filepath.Join(allowedDir, entry.Name())); err != nil {
			return fmt.Errorf("revoke host %s: %w", entry.Name(), err)
		}
	}

	for _, nqn := range sortedKeys(want) {
		host := filepath.Join(f.nvmetRoot, "hosts", nqn)

		if err := ensureDir(host); err != nil {
			return err
		}

		if err := os.Symlink(host, filepath.Join(allowedDir, nqn)); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("admit host %s: %w", nqn, err)
		}
	}

	return nil
}

// ensureNamespace points the subsystem's namespace at the local ublk device.
//
// device_path is only writable while the namespace is disabled, so a change of
// backing device means disabling, repointing and re-enabling. That is a real
// interruption for anyone attached, which is why racer's fabric device minor is
// derived from the universe id and never moves once assigned.
//
// The namespace also carries an identifier, and that identifier has to be the
// same every time this node publishes this universe. nvmet mints a random uuid
// when a namespace is created, and the host driver treats a namespace whose
// identifiers changed as a different namespace: it refuses to attach it, logs
// "identifiers changed for nsid 1", and leaves the peer with a live controller
// and no block device, permanently. That is exactly what an agent restart looks
// like from a peer that stayed connected through it, because the peer's
// controller survives on ctrl_loss_tmo and reconnects to a subsystem this code
// has just recreated. Deriving the uuid from the subsystem NQN makes the
// namespace the same namespace across restarts, which is what it is.
func (f *Fabric) ensureNamespace(subsystem, nqn, devicePath string) error {
	ns := filepath.Join(subsystem, "namespaces", fabricNamespaceID)

	if err := ensureDir(ns); err != nil {
		return err
	}

	current, err := readAttr(filepath.Join(ns, "device_path"))
	if err != nil {
		return err
	}

	enabled, err := readAttr(filepath.Join(ns, "enable"))
	if err != nil {
		return err
	}

	// A kernel too old to carry a settable namespace uuid simply keeps the one
	// it minted, which is the behaviour this code replaces rather than depends
	// on, so its absence is not an error.
	uuid, uuidSettable, err := readOptionalAttr(filepath.Join(ns, "device_uuid"))
	if err != nil {
		return err
	}

	wantedUUID := f.NamespaceUUID(nqn)

	// An enabled namespace over a device that is gone has to be taken down,
	// and taken down promptly.
	//
	// The target holds the block device open for as long as the namespace is
	// enabled. The ublk driver will not hand a minor back while anything holds
	// the device that used to be there, so a namespace still open on the
	// export of a dataplane that died stops that dataplane from ever
	// recreating it: every restart asks for the same minor, is told it exists,
	// asks for it to be removed, and waits out its reclaim window against an
	// opener that is this very process. Disabling first breaks that deadlock.
	//
	// It is also the right thing to do on its own terms. A namespace whose
	// backing device has gone answers peers with errors rather than data, so
	// there is nothing to preserve by leaving it up.
	if !f.devicePresent(devicePath) {
		if enabled == "1" {
			if err := writeAttr(filepath.Join(ns, "enable"), "0"); err != nil {
				return err
			}
		}

		return fmt.Errorf("%s is not exported by the dataplane yet", devicePath)
	}

	stale := current != devicePath || (uuidSettable && uuid != wantedUUID)

	if !stale && enabled == "1" {
		return nil
	}

	if stale {
		if enabled == "1" {
			if err := writeAttr(filepath.Join(ns, "enable"), "0"); err != nil {
				return err
			}
		}

		if current != devicePath {
			if err := writeAttr(filepath.Join(ns, "device_path"), devicePath); err != nil {
				return err
			}
		}

		if uuidSettable && uuid != wantedUUID {
			if err := writeAttr(filepath.Join(ns, "device_uuid"), wantedUUID); err != nil {
				return err
			}
		}
	}

	return writeAttr(filepath.Join(ns, "enable"), "1")
}

// fabricPort is one nvmet listening port: a transport and the address it
// answers on.
type fabricPort struct {
	// id is the configfs port directory name. nvmet identifies a port by a
	// small integer of its own choosing rather than by the address on it, so
	// this is a preference and not a name: ensurePorts resolves it against
	// what is already in the tree and records the answer.
	id int

	trtype  string
	adrfam  string
	traddr  string
	trsvcid string
}

// maxPortID is the largest nvmet port id. The kernel carries a port id as a
// 16-bit field, and zero is not a legal directory name for one.
const maxPortID = 65535

// tcpPort is the port every node publishes on, the floor every peer can reach.
func (f *Fabric) tcpPort() fabricPort {
	id := f.tcpPortID
	if id == 0 {
		id = f.port
	}

	return fabricPort{
		id:      id,
		trtype:  fabricTrtypeTCP,
		adrfam:  adrfamOf(f.addr),
		traddr:  f.addr,
		trsvcid: strconv.Itoa(f.port),
	}
}

// rdmaPortSpec is the RDMA port this node publishes, if it declares one.
func (f *Fabric) rdmaPortSpec() (fabricPort, bool) {
	if f.rdmaAddr == "" {
		return fabricPort{}, false
	}

	host, port := splitRDMAAddr(f.rdmaAddr, f.rdmaPort)

	id := f.rdmaPortID
	if id == 0 {
		id = port
	}

	return fabricPort{
		id:      id,
		trtype:  fabricTrtypeRDMA,
		adrfam:  adrfamOf(host),
		traddr:  host,
		trsvcid: strconv.Itoa(port),
	}, true
}

// livePorts lists the ports subsystems are linked into this pass.
func (f *Fabric) livePorts() []fabricPort {
	ports := []fabricPort{f.tcpPort()}

	if spec, ok := f.rdmaPortSpec(); ok && f.rdmaLive {
		ports = append(ports, spec)
	}

	return ports
}

// AdvertisedRDMAAddr is the RDMA address to publish for peers, empty until the
// RDMA port is actually listening.
//
// Advertising only what was achieved is what makes a missing or broken RDMA
// setup cost latency rather than connectivity: a peer selects RDMA on the
// strength of this address, so a node that could not bring its port up is
// simply dialled over TCP, and starts being dialled over RDMA the moment a
// later pass succeeds.
func (f *Fabric) AdvertisedRDMAAddr() string {
	if !f.rdmaLive {
		return ""
	}

	spec, ok := f.rdmaPortSpec()
	if !ok {
		return ""
	}

	return net.JoinHostPort(spec.traddr, spec.trsvcid)
}

// ensurePorts creates this node's listening ports and reports whether RDMA came
// up. A TCP failure is fatal to the pass; an RDMA failure is not, because TCP
// alone is a working fabric.
func (f *Fabric) ensurePorts() error {
	f.rdmaLive = false

	id, err := f.ensurePort(f.tcpPort())
	if err != nil {
		return err
	}

	f.tcpPortID = id

	spec, ok := f.rdmaPortSpec()
	if !ok {
		return nil
	}

	id, err = f.ensurePort(spec)
	if err != nil {
		return fmt.Errorf(
			"publish rdma port %d (continuing over tcp): %w: "+
				"load the RDMA target and host modules (modprobe nvmet_rdma nvme_rdma) "+
				"and check that %s is an address on an RDMA-capable interface",
			spec.id, err, spec.traddr)
	}

	f.rdmaPortID = id
	f.rdmaLive = true

	return nil
}

// ensurePort finds or creates the nvmet port carrying one transport address and
// returns the configfs id it lives at.
//
// The id nvmet gives a port is arbitrary. Using the service port number for it
// reads well and is what this code prefers, but it is only a preference: the id
// is not derived from the address, so two targets that want the same service
// port on different addresses cannot both hold it. That is not hypothetical
// even on a dedicated node, where an operator or another storage system may
// already own the id, and it is the normal case when several racer nodes share
// one kernel. Worse, the transport attributes are only writable before the
// first subsystem is linked, so a port claimed by someone else cannot simply be
// corrected: linking into it would silently publish this node's namespaces on
// another node's address, which is exactly the misattachment R7 forbids.
//
// So the address is the identity. An existing port whose transport, family,
// address and service id all match is this node's port whoever created it; an
// existing port with nothing written to it yet is adopted and configured; and
// otherwise a fresh id is taken, starting from the preferred one.
func (f *Fabric) ensurePort(spec fabricPort) (int, error) {
	root := filepath.Join(f.nvmetRoot, "ports")

	if err := ensureDir(root); err != nil {
		return 0, err
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", root, err)
	}

	taken := make(map[int]struct{}, len(entries))
	adopt := 0

	for _, entry := range entries {
		id, convErr := strconv.Atoi(entry.Name())
		if convErr != nil || id <= 0 {
			continue
		}

		taken[id] = struct{}{}

		// A port whose attributes cannot be read is somebody else's problem:
		// it is not this node's port and it cannot be adopted, but the id it
		// sits on is still taken. Refusing to publish over it would let one
		// unreadable entry take the whole fabric down.
		state, err := f.readPort(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}

		if state == spec.address() {
			return id, nil
		}

		// A port with no transport written to it is one somebody created and
		// did not finish, most likely this node in a pass that died between
		// the mkdir and the writes. Reuse it rather than leaving litter.
		if state.trtype == "" && adopt == 0 {
			adopt = id
		}
	}

	if adopt != 0 {
		return adopt, f.writePort(filepath.Join(root, strconv.Itoa(adopt)), spec)
	}

	for id := spec.id; id <= maxPortID; id++ {
		if _, ok := taken[id]; ok {
			continue
		}

		dir := filepath.Join(root, strconv.Itoa(id))

		if err := ensureDir(dir); err != nil {
			return 0, err
		}

		return id, f.writePort(dir, spec)
	}

	return 0, fmt.Errorf("no free nvmet port id at or above %d for %s://%s:%s",
		spec.id, spec.trtype, spec.traddr, spec.trsvcid)
}

// address is the part of a port spec that identifies it: everything but the id.
func (p fabricPort) address() fabricPort {
	p.id = 0

	return p
}

// readPort reads back the transport attributes of an existing port.
func (f *Fabric) readPort(dir string) (fabricPort, error) {
	var spec fabricPort

	for _, item := range []struct {
		attr  string
		field *string
	}{
		{"addr_trtype", &spec.trtype},
		{"addr_adrfam", &spec.adrfam},
		{"addr_traddr", &spec.traddr},
		{"addr_trsvcid", &spec.trsvcid},
	} {
		value, err := readAttr(filepath.Join(dir, item.attr))
		if err != nil {
			return fabricPort{}, err
		}

		*item.field = value
	}

	return spec, nil
}

// writePort sets a port's transport attributes.
//
// The transport is written last. nvmet only takes a port live when a subsystem
// is linked into it, so the order is not load-bearing for correctness, but
// leaving the transport until the address is in place keeps every intermediate
// state one nvmet would refuse to enable rather than one it would enable on the
// wrong address.
func (f *Fabric) writePort(dir string, spec fabricPort) error {
	if err := ensureDir(filepath.Join(dir, "subsystems")); err != nil {
		return err
	}

	for _, item := range [][2]string{
		{"addr_adrfam", spec.adrfam},
		{"addr_traddr", spec.traddr},
		{"addr_trsvcid", spec.trsvcid},
		{"addr_trtype", spec.trtype},
	} {
		if err := writeAttr(filepath.Join(dir, item[0]), item[1]); err != nil {
			return err
		}
	}

	return nil
}

// linkSubsystem exposes one subsystem on exactly the ports that came up, so a
// single namespace answers on both transports at once without retaining a link
// to an address that has been withdrawn.
func (f *Fabric) linkSubsystem(nqn string) error {
	target := filepath.Join(f.nvmetRoot, "subsystems", nqn)
	want := map[string]struct{}{}

	for _, spec := range f.livePorts() {
		port := strconv.Itoa(spec.id)
		want[port] = struct{}{}
		link := filepath.Join(f.nvmetRoot, "ports", port, "subsystems", nqn)

		if err := os.Symlink(target, link); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("link %s into port %d: %w", nqn, spec.id, err)
		}
	}

	ports, err := os.ReadDir(filepath.Join(f.nvmetRoot, "ports"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s/ports: %w", f.nvmetRoot, err)
	}

	for _, port := range ports {
		if !port.IsDir() {
			continue
		}

		if _, keep := want[port.Name()]; keep {
			continue
		}

		link := filepath.Join(f.nvmetRoot, "ports", port.Name(), "subsystems", nqn)
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("unlink %s from port %s: %w", nqn, port.Name(), err)
		}
	}

	return nil
}

// pruneSubsystems removes published subsystems the plan no longer names.
//
// Only subsystems this node published are considered: the NQN carries the
// publishing node id, so another node's subsystems on a shared target (or a
// hand-made one an operator put there) are left alone.
func (f *Fabric) pruneSubsystems(plan FabricPlan) error {
	keep := make(map[string]struct{}, len(plan.Exports))

	for _, export := range plan.Exports {
		keep[f.SubsystemNQN(export.UniverseID, f.nodeID)] = struct{}{}
	}

	root := filepath.Join(f.nvmetRoot, "subsystems")

	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("read %s: %w", root, err)
	}

	ours := f.nqnPrefix + ".u"
	suffix := fmt.Sprintf(".n%d", f.nodeID)

	var problems []error

	for _, entry := range entries {
		name := entry.Name()

		if !strings.HasPrefix(name, ours) || !strings.HasSuffix(name, suffix) {
			continue
		}

		if _, ok := keep[name]; ok {
			continue
		}

		if err := f.removeSubsystem(name); err != nil {
			problems = append(problems, err)
		}
	}

	return errors.Join(problems...)
}

// removeSubsystem tears one subsystem down in the order configfs requires:
// unlink from every port, revoke every host, disable and drop the namespace,
// then remove the subsystem itself.
func (f *Fabric) removeSubsystem(nqn string) error {
	subsystem := filepath.Join(f.nvmetRoot, "subsystems", nqn)

	// Every port this node could have linked it into, not just the ones it
	// publishes now: an RDMA address that was withdrawn must still have its
	// link cleaned up, or configfs refuses to remove the subsystem.
	ports, err := os.ReadDir(filepath.Join(f.nvmetRoot, "ports"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s/ports: %w", f.nvmetRoot, err)
	}

	for _, port := range ports {
		link := filepath.Join(f.nvmetRoot, "ports", port.Name(), "subsystems", nqn)
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("unlink %s from port %s: %w", nqn, port.Name(), err)
		}
	}

	allowed := filepath.Join(subsystem, "allowed_hosts")

	hosts, err := os.ReadDir(allowed)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", allowed, err)
	}

	for _, host := range hosts {
		if err := os.Remove(filepath.Join(allowed, host.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("revoke host %s: %w", host.Name(), err)
		}
	}

	ns := filepath.Join(subsystem, "namespaces", fabricNamespaceID)

	if _, err := os.Stat(ns); err == nil {
		if err := writeAttr(filepath.Join(ns, "enable"), "0"); err != nil {
			return err
		}

		if err := removeObject(ns); err != nil {
			return fmt.Errorf("remove namespace of %s: %w", nqn, err)
		}
	}

	for _, child := range []string{"allowed_hosts", "namespaces"} {
		if err := removeObject(filepath.Join(subsystem, child)); err != nil {
			return fmt.Errorf("remove %s of %s: %w", child, nqn, err)
		}
	}

	if err := removeObject(subsystem); err != nil {
		return fmt.Errorf("remove subsystem %s: %w", nqn, err)
	}

	return nil
}

// removeObject removes a configfs object directory.
//
// configfs lets an object's directory be removed while its attribute files are
// still there, and refuses to let those files be unlinked on their own, so the
// plain rmdir is the whole operation there. The retry exists so the same code
// drives an ordinary directory, where the attributes this file wrote are real
// files and do hold the directory open. Directories and symlinks are never
// swept: a namespace that is still linked into a port must fail loudly rather
// than be forced.
func removeObject(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}

	entries, readErr := os.ReadDir(path)
	if readErr != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return err
		}
	}

	for _, entry := range entries {
		if removeErr := os.Remove(filepath.Join(path, entry.Name())); removeErr != nil {
			return err
		}
	}

	return os.Remove(path)
}

// attach connects one remote namespace and reports the local device path it
// showed up as.
//
// The path is reported exactly as the kernel named it rather than through a
// symlink of our own. R7 asks that paths be stable because an unchanged path
// keeps racer's open file descriptor across a reload, and the kernel name is
// stable for as long as the controller is: it changes only when the controller
// is genuinely torn down and reconnected, which is precisely the case where the
// descriptor must be reopened anyway.
func (f *Fabric) attach(imp FabricImportRequest) (string, error) {
	if imp.NQN == "" {
		return "", errors.New("peer has not published an NQN yet")
	}

	host, port, err := splitFabricAddr(imp.Addr)
	if err != nil {
		return "", err
	}

	trtype := imp.Trtype
	if trtype == "" {
		trtype = fabricTrtypeTCP
	}

	endpoint := controllerEndpoint{trtype: trtype, traddr: host, trsvcid: port}

	controller, stale, err := f.findController(imp.NQN, endpoint)
	if err != nil {
		return "", err
	}

	if controller != "" {
		return f.namespacePath(controller)
	}

	for _, name := range stale {
		if err := f.connector.Disconnect(name); err != nil {
			return "", fmt.Errorf("disconnect stale controller %s: %w", name, err)
		}
	}

	err = f.connector.Connect(ConnectRequest{
		NQN:     imp.NQN,
		HostNQN: f.HostNQN(f.nodeID),
		HostID:  f.HostID(f.nodeID),
		Traddr:  host,
		Trsvcid: port,
		Trtype:  trtype,
		Adrfam:  adrfamOf(host),
	})
	if err != nil {
		return "", err
	}

	controller, _, err = f.findController(imp.NQN, endpoint)
	if err != nil {
		return "", err
	}

	if controller == "" {
		return "", fmt.Errorf("connected to %s but no controller appeared", imp.NQN)
	}

	return f.namespacePath(controller)
}

// controllerEndpoint is the kernel-visible identity of an NVMe-oF connection.
// The subsystem NQN alone is not enough: changing address or transport requires
// replacing the controller because its reconnect parameters are immutable.
type controllerEndpoint struct {
	trtype  string
	traddr  string
	trsvcid string
}

// findController locates a controller this node attached at the requested
// endpoint. Controllers for the same subsystem at another endpoint are returned
// separately so attach can replace them.
//
// The host NQN is part of the match, not just the subsystem's. /sys/class/nvme
// is one namespace for the whole kernel, so a controller reached by a different
// host identity is not this node's to reuse or to tear down: adopting it would
// make this node's device path depend on another host's lifetime, and pruning
// it would cut a link this node never made.
func (f *Fabric) findController(nqn string, want controllerEndpoint) (string, []string, error) {
	entries, err := os.ReadDir(f.sysClassNvme)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, nil
		}

		return "", nil, fmt.Errorf("read %s: %w", f.sysClassNvme, err)
	}

	var stale []string

	for _, entry := range entries {
		ours, err := f.ownsController(entry.Name())
		if err != nil {
			return "", nil, err
		}

		if !ours {
			continue
		}

		subsys, err := readAttr(filepath.Join(f.sysClassNvme, entry.Name(), "subsysnqn"))
		if err != nil {
			return "", nil, err
		}

		if subsys != nqn {
			continue
		}

		endpoint, err := f.readControllerEndpoint(entry.Name())
		if err != nil {
			return "", nil, err
		}

		if endpoint == want {
			return entry.Name(), stale, nil
		}

		stale = append(stale, entry.Name())
	}

	return "", stale, nil
}

// readControllerEndpoint reads the transport and destination from sysfs. The
// address attribute may also contain source-side fields, which are deliberately
// ignored because they are not part of the requested target endpoint.
func (f *Fabric) readControllerEndpoint(controller string) (controllerEndpoint, error) {
	dir := filepath.Join(f.sysClassNvme, controller)

	transport, err := readAttr(filepath.Join(dir, "transport"))
	if err != nil {
		return controllerEndpoint{}, err
	}

	address, err := readAttr(filepath.Join(dir, "address"))
	if err != nil {
		return controllerEndpoint{}, err
	}

	values := map[string]string{}

	for _, field := range strings.Split(address, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
		if ok {
			values[key] = value
		}
	}

	return controllerEndpoint{
		trtype:  transport,
		traddr:  values["traddr"],
		trsvcid: values["trsvcid"],
	}, nil
}

// ownsController reports whether a controller was attached by this node.
func (f *Fabric) ownsController(controller string) (bool, error) {
	host, err := readAttr(filepath.Join(f.sysClassNvme, controller, "hostnqn"))
	if err != nil {
		return false, err
	}

	return host == f.HostNQN(f.nodeID), nil
}

// namespacePath resolves a controller to the block device of its single
// namespace. racer publishes exactly one namespace per subsystem, so finding
// more than one means something other than racer created the subsystem and the
// safe answer is to refuse rather than guess.
//
// There are two shapes to find it in. A controller whose subsystem does not
// advertise multiple controllers carries its namespace directly, as
// /sys/class/nvme/nvmeX/nvmeXnY. nvmet always advertises multiple controllers,
// so in practice racer's own targets take the other shape: the kernel builds a
// multipath head for the namespace, hides the per-controller device under a
// nvmeXcYnZ name that is not a /dev node at all, and puts the usable device
// beside the controller under its nvme-subsystem. Following only the first
// shape would have every attach report "no namespace yet" against a target that
// is working perfectly.
func (f *Fabric) namespacePath(controller string) (string, error) {
	found, err := namespacesIn(filepath.Join(f.sysClassNvme, controller), controller+"n")
	if err != nil {
		return "", err
	}

	if len(found) == 0 {
		found, err = f.multipathNamespaces(controller)
		if err != nil {
			return "", err
		}
	}

	switch len(found) {
	case 0:
		return "", fmt.Errorf("controller %s has no namespace yet", controller)
	case 1:
		return "/dev/" + found[0], nil
	default:
		return "", fmt.Errorf("controller %s carries %d namespaces, expected exactly one", controller, len(found))
	}
}

// multipathNamespaces lists the head devices of the subsystem a controller
// belongs to.
func (f *Fabric) multipathNamespaces(controller string) ([]string, error) {
	entries, err := os.ReadDir(sysClassNvmeSubsystem)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("read %s: %w", sysClassNvmeSubsystem, err)
	}

	for _, entry := range entries {
		dir := filepath.Join(sysClassNvmeSubsystem, entry.Name())

		if _, err := os.Stat(filepath.Join(dir, controller)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return nil, fmt.Errorf("stat %s: %w", filepath.Join(dir, controller), err)
		}

		return namespacesIn(dir, "nvme")
	}

	return nil, nil
}

// namespaceName matches a namespace block device, nvme<controller>n<nsid>.
var namespaceName = regexp.MustCompile(`^nvme[0-9]+n[0-9]+$`)

// namespacesIn lists the namespace block devices in a sysfs directory, sorted.
// The prefix narrows the search to one controller's own devices where that
// distinction exists.
func namespacesIn(dir, prefix string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	var found []string

	for _, entry := range entries {
		name := entry.Name()

		if strings.HasPrefix(name, prefix) && namespaceName.MatchString(name) {
			found = append(found, name)
		}
	}

	sort.Strings(found)

	return found, nil
}

// pruneControllers disconnects racer subsystems or endpoints the plan no longer
// imports.
// Only controllers this node attached, whose subsystem NQN carries our prefix,
// are considered: a node that also mounts unrelated NVMe-oF storage keeps it,
// and a node sharing a kernel with another racer node keeps that node's links.
func (f *Fabric) pruneControllers(plan FabricPlan) error {
	keep := make(map[string]map[controllerEndpoint]struct{}, len(plan.Imports))
	keepAll := make(map[string]struct{})

	var problems []error

	for _, imp := range plan.Imports {
		host, port, err := splitFabricAddr(imp.Addr)
		if err != nil {
			// Attach already reported the malformed endpoint. Retain any existing
			// controller for this NQN rather than turning bad input into an
			// unrelated disconnect, while still pruning the rest of the plan.
			keepAll[imp.NQN] = struct{}{}
			problems = append(problems, fmt.Errorf("import %s endpoint: %w", imp.NQN, err))

			continue
		}

		trtype := imp.Trtype
		if trtype == "" {
			trtype = fabricTrtypeTCP
		}

		if keep[imp.NQN] == nil {
			keep[imp.NQN] = map[controllerEndpoint]struct{}{}
		}

		keep[imp.NQN][controllerEndpoint{trtype: trtype, traddr: host, trsvcid: port}] = struct{}{}
	}

	entries, err := os.ReadDir(f.sysClassNvme)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("read %s: %w", f.sysClassNvme, err)
	}

	// A node's own subsystem is attached by that node too: racer reads its own
	// fabric device through the fabric like any peer's.
	ours := f.nqnPrefix + ".u"

	for _, entry := range entries {
		mine, err := f.ownsController(entry.Name())
		if err != nil {
			problems = append(problems, err)
			continue
		}

		if !mine {
			continue
		}

		subsys, err := readAttr(filepath.Join(f.sysClassNvme, entry.Name(), "subsysnqn"))
		if err != nil {
			problems = append(problems, err)
			continue
		}

		if !strings.HasPrefix(subsys, ours) {
			continue
		}

		if _, ok := keepAll[subsys]; ok {
			continue
		}

		if endpoints := keep[subsys]; len(endpoints) > 0 {
			endpoint, endpointErr := f.readControllerEndpoint(entry.Name())
			if endpointErr != nil {
				problems = append(problems, endpointErr)

				continue
			}

			if _, ok := endpoints[endpoint]; ok {
				continue
			}
		}

		if err := f.connector.Disconnect(entry.Name()); err != nil {
			problems = append(problems, fmt.Errorf("disconnect %s: %w", subsys, err))
		}
	}

	return errors.Join(problems...)
}

// fabricsConnector is the real initiator, driving /dev/nvme-fabrics.
type fabricsConnector struct {
	path string
}

// Connect writes one connect line to /dev/nvme-fabrics.
//
// The kernel answers an already-established connection with EALREADY, which is
// success as far as this reconcile is concerned.
func (c fabricsConnector) Connect(req ConnectRequest) error {
	f, err := os.OpenFile(c.path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", c.path, err)
	}

	defer func() { _ = f.Close() }() //nolint:errcheck

	line := strings.Join([]string{
		"transport=" + req.Trtype,
		"traddr=" + req.Traddr,
		"trsvcid=" + req.Trsvcid,
		"nqn=" + req.NQN,
		"hostnqn=" + req.HostNQN,
		"hostid=" + req.HostID,
	}, ",")

	if _, err := f.WriteString(line); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}

		return fmt.Errorf("connect %s: %w", req.NQN, err)
	}

	return nil
}

// Disconnect asks the kernel to delete a controller.
func (c fabricsConnector) Disconnect(controller string) error {
	path := filepath.Join("/sys/class/nvme", controller, "delete_controller")

	if err := os.WriteFile(path, []byte("1"), 0o200); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

// splitFabricAddr splits a published "host:port" into its parts.
//
// net.SplitHostPort rather than a cut on the last colon because an IPv6
// literal is written [fd00::1]:4420 and a naive split would hand the kernel
// half an address.
func splitFabricAddr(addr string) (string, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || port == "" {
		return "", "", fmt.Errorf("peer address %q is not host:port", addr)
	}

	return host, port, nil
}

// splitRDMAAddr reads an operator-supplied RDMA address, which may name a port
// or leave it to the cluster default.
//
// The bare form is the common one: an administrator knows the IP of the node's
// RDMA NIC and has no reason to care which service id nvmet listens on, and
// leaving it out keeps every node in the cluster on the same one, which is what
// lets a peer dial without being told.
func splitRDMAAddr(addr string, fallback int) (string, int) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.Trim(addr, "[]"), fallback
	}

	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 {
		return host, fallback
	}

	return host, value
}

// adrfamOf is the nvmet address family of a transport address. Anything that
// does not parse as an IP is reported as IPv4, which is what a hostname in a
// v4 cluster resolves to and what nvmet will reject loudly if it is wrong.
func adrfamOf(host string) string {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip != nil && ip.To4() == nil {
		return adrfamIPv6
	}

	return adrfamIPv4
}

// ensureDir creates a configfs directory, treating an existing one as success.
// configfs turns mkdir into "create this object", so this is how every nvmet
// object comes into being.
func ensureDir(path string) error {
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create %s: %w", path, err)
	}

	return nil
}

// readAttr reads a configfs attribute, reporting a missing one as empty. An
// attribute that is not there yet is indistinguishable from one that is unset
// for every decision this file makes.
func readAttr(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}

		return "", fmt.Errorf("read %s: %w", path, err)
	}

	return strings.TrimSpace(string(raw)), nil
}

// readOptionalAttr reads an attribute that a kernel may not have at all,
// reporting whether it is there. readAttr cannot answer this: it folds a
// missing file into the empty string, which several nvmet attributes use as a
// legitimate unset value.
func readOptionalAttr(path string) (string, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("read %s: %w", path, err)
	}

	return strings.TrimSpace(string(raw)), true, nil
}

func writeAttr(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write %s=%q: %w", path, value, err)
	}

	return nil
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}
