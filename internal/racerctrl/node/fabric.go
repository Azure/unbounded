// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
}

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
	NQN      string
	HostNQN  string
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
		connector = fabricsConnector{path: "/dev/nvme-fabrics"}
	}

	return &Fabric{
		nvmetRoot:    cfg.NvmetRoot,
		sysClassNvme: "/sys/class/nvme",
		connector:    connector,
		nodeID:       nodeID,
		nqnPrefix:    cfg.NQNPrefix,
		addr:         cfg.FabricAddr,
		port:         cfg.FabricPort,
		rdmaPort:     cfg.RDMAPort,
	}
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

	if err := f.ensureNamespace(subsystem, racerctrl.BlockDevicePath(export.DeviceID)); err != nil {
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
func (f *Fabric) ensureNamespace(subsystem, devicePath string) error {
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

	if current == devicePath && enabled == "1" {
		return nil
	}

	if current != devicePath {
		if enabled == "1" {
			if err := writeAttr(filepath.Join(ns, "enable"), "0"); err != nil {
				return err
			}
		}

		if err := writeAttr(filepath.Join(ns, "device_path"), devicePath); err != nil {
			return err
		}
	}

	return writeAttr(filepath.Join(ns, "enable"), "1")
}

// fabricPort is one nvmet listening port: a transport and the address it
// answers on.
type fabricPort struct {
	// id is the configfs port directory name. nvmet identifies a port by a
	// small integer, so the service port number doubles as the id: it is
	// already unique per transport here, and it makes the configfs tree
	// readable next to an `ss -lnt`.
	id int

	trtype  string
	adrfam  string
	traddr  string
	trsvcid string
}

// tcpPort is the port every node publishes on, the floor every peer can reach.
func (f *Fabric) tcpPort() fabricPort {
	return fabricPort{
		id:      f.port,
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

	return fabricPort{
		id:      port,
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

	if err := f.ensurePort(f.tcpPort()); err != nil {
		return err
	}

	spec, ok := f.rdmaPortSpec()
	if !ok {
		return nil
	}

	if err := f.ensurePort(spec); err != nil {
		return fmt.Errorf(
			"publish rdma port %d (continuing over tcp): %w: "+
				"load the RDMA target and host modules (modprobe nvmet_rdma nvme_rdma) "+
				"and check that %s is an address on an RDMA-capable interface",
			spec.id, err, spec.traddr)
	}

	f.rdmaLive = true

	return nil
}

// ensurePort creates one nvmet port and sets its transport attributes.
//
// All of racer's subsystems share a port per transport: they differ by NQN, not
// by address, and a per-subsystem port would burn one listening socket per
// universe for no benefit.
func (f *Fabric) ensurePort(spec fabricPort) error {
	port := filepath.Join(f.nvmetRoot, "ports", strconv.Itoa(spec.id))

	if err := ensureDir(port); err != nil {
		return err
	}

	// As with a subsystem's children: configfs creates this, and saying so
	// costs one tolerated EEXIST.
	if err := ensureDir(filepath.Join(port, "subsystems")); err != nil {
		return err
	}

	// The transport attributes are only writable before any subsystem is
	// linked, so they are written unconditionally on a freshly created port
	// and skipped once it carries links.
	links, err := os.ReadDir(filepath.Join(port, "subsystems"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s/subsystems: %w", port, err)
	}

	if len(links) > 0 {
		return nil
	}

	for attr, value := range map[string]string{
		"addr_trtype":  spec.trtype,
		"addr_adrfam":  spec.adrfam,
		"addr_traddr":  spec.traddr,
		"addr_trsvcid": spec.trsvcid,
	} {
		if err := writeAttr(filepath.Join(port, attr), value); err != nil {
			return err
		}
	}

	return nil
}

// linkSubsystem exposes one subsystem on every port that came up, so a single
// namespace answers on both transports at once.
func (f *Fabric) linkSubsystem(nqn string) error {
	target := filepath.Join(f.nvmetRoot, "subsystems", nqn)

	for _, spec := range f.livePorts() {
		link := filepath.Join(f.nvmetRoot, "ports", strconv.Itoa(spec.id), "subsystems", nqn)

		if err := os.Symlink(target, link); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("link %s into port %d: %w", nqn, spec.id, err)
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

	if controller, ok, err := f.findController(imp.NQN); err != nil {
		return "", err
	} else if ok {
		return f.namespacePath(controller)
	}

	err = f.connector.Connect(ConnectRequest{
		NQN:     imp.NQN,
		HostNQN: f.HostNQN(f.nodeID),
		Traddr:  host,
		Trsvcid: port,
		Trtype:  trtype,
		Adrfam:  adrfamOf(host),
	})
	if err != nil {
		return "", err
	}

	controller, ok, err := f.findController(imp.NQN)
	if err != nil {
		return "", err
	}

	if !ok {
		return "", fmt.Errorf("connected to %s but no controller appeared", imp.NQN)
	}

	return f.namespacePath(controller)
}

// findController locates an attached controller by the subsystem it carries.
func (f *Fabric) findController(nqn string) (string, bool, error) {
	entries, err := os.ReadDir(f.sysClassNvme)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("read %s: %w", f.sysClassNvme, err)
	}

	for _, entry := range entries {
		subsys, err := readAttr(filepath.Join(f.sysClassNvme, entry.Name(), "subsysnqn"))
		if err != nil {
			return "", false, err
		}

		if subsys == nqn {
			return entry.Name(), true, nil
		}
	}

	return "", false, nil
}

// namespacePath resolves a controller to the block device of its single
// namespace. racer publishes exactly one namespace per subsystem, so finding
// more than one means something other than racer created the subsystem and the
// safe answer is to refuse rather than guess.
func (f *Fabric) namespacePath(controller string) (string, error) {
	dir := filepath.Join(f.sysClassNvme, controller)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", dir, err)
	}

	var found []string

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, controller+"n") {
			found = append(found, name)
		}
	}

	sort.Strings(found)

	switch len(found) {
	case 0:
		return "", fmt.Errorf("controller %s has no namespace yet", controller)
	case 1:
		return "/dev/" + found[0], nil
	default:
		return "", fmt.Errorf("controller %s carries %d namespaces, expected exactly one", controller, len(found))
	}
}

// pruneControllers disconnects racer subsystems the plan no longer imports.
// Only controllers whose subsystem NQN carries our prefix are considered, so a
// node that also mounts unrelated NVMe-oF storage keeps it.
func (f *Fabric) pruneControllers(plan FabricPlan) error {
	keep := make(map[string]struct{}, len(plan.Imports))

	for _, imp := range plan.Imports {
		keep[imp.NQN] = struct{}{}
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

	var problems []error

	for _, entry := range entries {
		subsys, err := readAttr(filepath.Join(f.sysClassNvme, entry.Name(), "subsysnqn"))
		if err != nil {
			problems = append(problems, err)
			continue
		}

		if !strings.HasPrefix(subsys, ours) {
			continue
		}

		if _, ok := keep[subsys]; ok {
			continue
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
