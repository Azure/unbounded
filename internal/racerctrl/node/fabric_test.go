// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/Azure/unbounded/internal/racerctrl"
)

// fakeConnector records what the initiator was asked to do and pretends the
// controller appeared, by creating it under the fake /sys/class/nvme.
type fakeConnector struct {
	sysClassNvme string
	next         int
	requests     []ConnectRequest
	disconnected []string
	err          error
}

func (c *fakeConnector) Connect(req ConnectRequest) error {
	c.requests = append(c.requests, req)

	if c.err != nil {
		return c.err
	}

	name := "nvme" + strconv.Itoa(c.next)
	c.next++

	dir := filepath.Join(c.sysClassNvme, name)
	if err := os.MkdirAll(filepath.Join(dir, name+"n1"), 0o755); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(dir, "hostnqn"), []byte(req.HostNQN), 0o644); err != nil {
		return err
	}

	for name, value := range map[string]string{
		"subsysnqn": req.NQN,
		"transport": req.Trtype,
		"address":   "traddr=" + req.Traddr + ",trsvcid=" + req.Trsvcid + ",host_traddr=10.0.0.7",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			return err
		}
	}

	return nil
}

func (c *fakeConnector) Disconnect(controller string) error {
	c.disconnected = append(c.disconnected, controller)

	return os.RemoveAll(filepath.Join(c.sysClassNvme, controller))
}

// newTestFabric builds a Fabric over a fake nvmet configfs tree and a fake
// /sys/class/nvme. Only the directories nvmet itself creates when the target
// modules load are seeded.
func newTestFabric(t *testing.T, cfg Config) (*Fabric, *fakeConnector) {
	t.Helper()

	root := t.TempDir()
	sysClass := t.TempDir()

	for _, dir := range []string{"subsystems", "ports", "hosts"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("seed %s: %v", dir, err)
		}
	}

	cfg.NvmetRoot = root
	if cfg.NQNPrefix == "" {
		cfg.NQNPrefix = DefaultNQNPrefix
	}

	if cfg.FabricPort == 0 {
		cfg.FabricPort = DefaultFabricPort
	}

	if cfg.RDMAPort == 0 {
		cfg.RDMAPort = DefaultRDMAPort
	}

	connector := &fakeConnector{sysClassNvme: sysClass}

	fabric := NewFabric(cfg, 7, connector)
	fabric.sysClassNvme = sysClass

	// There is no ublk device behind any of these exports. The target half is
	// what these tests drive, so say the dataplane is serving; the tests that
	// care about a missing device say otherwise for themselves.
	fabric.devicePresent = func(string) bool { return true }

	return fabric, connector
}

// portAttrs reads one nvmet port's transport attributes, nil when the port was
// never created.
func portAttrs(t *testing.T, fabric *Fabric, id int) map[string]string {
	t.Helper()

	dir := filepath.Join(fabric.nvmetRoot, "ports", strconv.Itoa(id))

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil
	}

	attrs := map[string]string{}

	for _, attr := range []string{"addr_trtype", "addr_adrfam", "addr_traddr", "addr_trsvcid"} {
		value, err := readAttr(filepath.Join(dir, attr))
		if err != nil {
			t.Fatalf("read %s: %v", attr, err)
		}

		attrs[attr] = value
	}

	return attrs
}

// portLinks lists the ports a subsystem is linked into.
func portLinks(t *testing.T, fabric *Fabric, nqn string) []string {
	t.Helper()

	ports, err := os.ReadDir(filepath.Join(fabric.nvmetRoot, "ports"))
	if err != nil {
		t.Fatalf("read ports: %v", err)
	}

	var out []string

	for _, port := range ports {
		link := filepath.Join(fabric.nvmetRoot, "ports", port.Name(), "subsystems", nqn)
		if _, err := os.Lstat(link); err == nil {
			out = append(out, port.Name())
		}
	}

	sort.Strings(out)

	return out
}

// The deadlock this guards against: nvmet holds the block device open while a
// namespace is enabled, and the ublk driver will not hand a minor back while
// anything holds the device that used to be there. A namespace left enabled
// over the export of a dataplane that died therefore stops that dataplane from
// ever recreating it. The agent has to let go first.
func TestReconcileDisablesANamespaceWhoseDeviceIsGone(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

	plan := FabricPlan{
		Exports: []FabricExportRequest{{UniverseID: 1, DeviceID: 4, AllowedNodes: []uint32{7}}},
	}

	if _, err := fabric.Reconcile(plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ns := filepath.Join(fabric.nvmetRoot, "subsystems",
		fabric.SubsystemNQN(1, 7), "namespaces", fabricNamespaceID)

	if got := readTestAttr(t, filepath.Join(ns, "enable")); got != "1" {
		t.Fatalf("namespace enable is %q, want 1 before the dataplane dies", got)
	}

	fabric.devicePresent = func(string) bool { return false }

	state, err := fabric.Reconcile(plan)
	if err == nil {
		t.Fatal("expected an export over a missing device to be reported")
	}

	if len(state.Exports) != 0 {
		t.Fatalf("advertised %d exports over a missing device, want none", len(state.Exports))
	}

	if got := readTestAttr(t, filepath.Join(ns, "enable")); got != "0" {
		t.Fatalf("namespace enable is %q, want 0 once the device is gone", got)
	}

	// And it comes back on its own once the dataplane has the device again.
	fabric.devicePresent = func(string) bool { return true }

	if _, err := fabric.Reconcile(plan); err != nil {
		t.Fatalf("reconcile after recovery: %v", err)
	}

	if got := readTestAttr(t, filepath.Join(ns, "enable")); got != "1" {
		t.Fatalf("namespace enable is %q, want 1 after the device is back", got)
	}
}

// TestPublishGivesTheNamespaceAStableIdentifier pins the identifier a
// subsystem's namespace carries. nvmet mints a random uuid when a namespace is
// created, and a peer that reconnects to a namespace whose identifiers changed
// refuses to attach it, so an agent restart would strand every peer.
func TestPublishGivesTheNamespaceAStableIdentifier(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

	nqn := fabric.SubsystemNQN(1, 7)
	ns := filepath.Join(fabric.nvmetRoot, "subsystems", nqn, "namespaces", fabricNamespaceID)

	// A plain directory has no attributes, so the one the kernel would offer
	// is seeded here with the random value nvmet would have put in it.
	if err := os.MkdirAll(ns, 0o755); err != nil {
		t.Fatalf("seed namespace: %v", err)
	}

	if err := os.WriteFile(filepath.Join(ns, "device_uuid"),
		[]byte("bd1e6b7a-0c22-4c6e-9a5f-1d0f7b4a2c31"), 0o644); err != nil {
		t.Fatalf("seed device_uuid: %v", err)
	}

	plan := FabricPlan{
		Exports: []FabricExportRequest{{UniverseID: 1, DeviceID: 4, AllowedNodes: []uint32{7}}},
	}

	if _, err := fabric.Reconcile(plan); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	want := fabric.NamespaceUUID(nqn)
	if want == "" {
		t.Fatal("derived namespace uuid is empty")
	}

	if got := readTestAttr(t, filepath.Join(ns, "device_uuid")); got != want {
		t.Fatalf("device_uuid is %q, want the derived %q", got, want)
	}

	if got := readTestAttr(t, filepath.Join(ns, "enable")); got != "1" {
		t.Fatalf("namespace enable is %q, want 1", got)
	}

	// The identifier is restored rather than merely written once, because a
	// namespace recreated by nvmet arrives with a fresh random one.
	if err := os.WriteFile(filepath.Join(ns, "device_uuid"),
		[]byte("00000000-0000-0000-0000-000000000000"), 0o644); err != nil {
		t.Fatalf("scramble device_uuid: %v", err)
	}

	if _, err := fabric.Reconcile(plan); err != nil {
		t.Fatalf("reconcile again: %v", err)
	}

	if got := readTestAttr(t, filepath.Join(ns, "device_uuid")); got != want {
		t.Fatalf("device_uuid is %q after a reconcile, want the derived %q", got, want)
	}

	if got := readTestAttr(t, filepath.Join(ns, "enable")); got != "1" {
		t.Fatalf("namespace enable is %q after repointing, want 1", got)
	}
}

// TestPublishLeavesTheIdentifierAloneWhenTheKernelHasNone keeps the agent
// working on a kernel whose nvmet predates a settable namespace uuid.
func TestPublishLeavesTheIdentifierAloneWhenTheKernelHasNone(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

	if _, err := fabric.Reconcile(FabricPlan{
		Exports: []FabricExportRequest{{UniverseID: 1, DeviceID: 4, AllowedNodes: []uint32{7}}},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ns := filepath.Join(fabric.nvmetRoot, "subsystems",
		fabric.SubsystemNQN(1, 7), "namespaces", fabricNamespaceID)

	if _, err := os.Stat(filepath.Join(ns, "device_uuid")); !os.IsNotExist(err) {
		t.Fatalf("device_uuid was created where the kernel offers none: %v", err)
	}

	if got := readTestAttr(t, filepath.Join(ns, "enable")); got != "1" {
		t.Fatalf("namespace enable is %q, want 1", got)
	}
}

func readTestAttr(t *testing.T, path string) string {
	t.Helper()

	value, err := readAttr(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return value
}

func TestReconcilePublishesOverTCPOnly(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

	state, err := fabric.Reconcile(FabricPlan{
		Exports: []FabricExportRequest{{UniverseID: 1, DeviceID: 4, AllowedNodes: []uint32{7, 8}}},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(state.Exports) != 1 {
		t.Fatalf("published %d exports, want 1", len(state.Exports))
	}

	export := state.Exports[0]
	if export.Addr != "10.0.0.7:4420" {
		t.Fatalf("advertised %q, want 10.0.0.7:4420", export.Addr)
	}

	// No RDMA address was configured, so none is advertised and peers dial TCP.
	if export.RDMAAddr != "" {
		t.Fatalf("advertised RDMA address %q with no RDMA configured", export.RDMAAddr)
	}

	if links := portLinks(t, fabric, export.NQN); len(links) != 1 || links[0] != "4420" {
		t.Fatalf("subsystem linked into ports %v, want just 4420", links)
	}

	if attrs := portAttrs(t, fabric, DefaultFabricPort); attrs["addr_trtype"] != "tcp" || attrs["addr_adrfam"] != "ipv4" {
		t.Fatalf("tcp port attributes %v", attrs)
	}
}

func TestReconcilePublishesBothTransports(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})
	fabric.SetRDMAAddr("192.168.9.7")

	state, err := fabric.Reconcile(FabricPlan{
		Exports: []FabricExportRequest{{UniverseID: 1, DeviceID: 4, AllowedNodes: []uint32{7}}},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	export := state.Exports[0]

	// The bare address is completed with the cluster's RDMA service id, so a
	// peer can dial without being told which one this node chose.
	if export.RDMAAddr != "192.168.9.7:4421" {
		t.Fatalf("advertised RDMA address %q, want 192.168.9.7:4421", export.RDMAAddr)
	}

	links := portLinks(t, fabric, export.NQN)
	if len(links) != 2 {
		t.Fatalf("subsystem linked into ports %v, want both transports", links)
	}

	// One namespace, one device, answering on both ports at once.
	if attrs := portAttrs(t, fabric, DefaultRDMAPort); attrs["addr_trtype"] != "rdma" {
		t.Fatalf("rdma port attributes %v", attrs)
	}

	if attrs := portAttrs(t, fabric, DefaultFabricPort); attrs["addr_trtype"] != "tcp" {
		t.Fatalf("tcp port attributes %v", attrs)
	}
}

func TestReconcileUnlinksWithdrawnTargetPorts(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})
	fabric.SetRDMAAddr("192.168.9.7")

	plan := FabricPlan{
		Exports: []FabricExportRequest{{UniverseID: 1, DeviceID: 4, AllowedNodes: []uint32{7}}},
	}

	state, err := fabric.Reconcile(plan)
	if err != nil {
		t.Fatalf("publish over both transports: %v", err)
	}

	nqn := state.Exports[0].NQN
	if links := portLinks(t, fabric, nqn); len(links) != 2 {
		t.Fatalf("subsystem linked into ports %v, want both transports", links)
	}

	fabric.SetRDMAAddr("")

	if _, err := fabric.Reconcile(plan); err != nil {
		t.Fatalf("withdraw rdma: %v", err)
	}

	if links := portLinks(t, fabric, nqn); len(links) != 1 || links[0] != strconv.Itoa(DefaultFabricPort) {
		t.Fatalf("subsystem linked into ports %v after RDMA withdrawal, want only TCP", links)
	}

	fabric.SetRDMAAddr("192.168.10.7")

	if _, err := fabric.Reconcile(plan); err != nil {
		t.Fatalf("republish rdma at new address: %v", err)
	}

	links := portLinks(t, fabric, nqn)
	if len(links) != 2 {
		t.Fatalf("subsystem linked into ports %v after RDMA move, want current TCP and RDMA", links)
	}

	for _, port := range links {
		if attrs := portAttrs(t, fabric, mustAtoi(t, port)); attrs["addr_trtype"] == fabricTrtypeRDMA &&
			attrs["addr_traddr"] != "192.168.10.7" {
			t.Fatalf("subsystem remains linked to stale RDMA port %s: %v", port, attrs)
		}
	}
}

func TestReconcileDegradesToTCPWhenRDMAFails(t *testing.T) {
	// The RDMA service port is pinned to the last id configfs can hold and
	// that id is made unusable, so there is nowhere left for the RDMA port to
	// go. A missing nvmet_rdma looks the same from here: the port cannot be
	// published, and the pass has to carry on over TCP anyway.
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7", RDMAPort: 65535})
	fabric.SetRDMAAddr("192.168.9.7")

	blocked := filepath.Join(fabric.nvmetRoot, "ports", "65535")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatalf("block the rdma port: %v", err)
	}

	state, err := fabric.Reconcile(FabricPlan{
		Exports: []FabricExportRequest{{UniverseID: 1, DeviceID: 4, AllowedNodes: []uint32{7}}},
	})

	// The failure is reported, and the export still happened over TCP.
	if err == nil {
		t.Fatal("a failed RDMA port was not reported")
	}

	if len(state.Exports) != 1 {
		t.Fatalf("published %d exports, want the TCP one", len(state.Exports))
	}

	if state.Exports[0].RDMAAddr != "" {
		t.Fatalf("advertised RDMA address %q after the port failed", state.Exports[0].RDMAAddr)
	}

	if state.Exports[0].Addr == "" {
		t.Fatal("no TCP address advertised")
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})
	fabric.SetRDMAAddr("[fd00::7]:4421")

	plan := FabricPlan{
		Exports: []FabricExportRequest{{UniverseID: 1, DeviceID: 4, AllowedNodes: []uint32{7, 8}}},
	}

	first, err := fabric.Reconcile(plan)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	second, err := fabric.Reconcile(plan)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	if first.Exports[0] != second.Exports[0] {
		t.Fatalf("export moved from %+v to %+v", first.Exports[0], second.Exports[0])
	}

	// An IPv6 RDMA address is published as IPv6, not silently as v4.
	if attrs := portAttrs(t, fabric, 4421); attrs["addr_adrfam"] != "ipv6" {
		t.Fatalf("rdma port attributes %v, want an ipv6 address family", attrs)
	}
}

func TestPruneUnlinksFromEveryPort(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})
	fabric.SetRDMAAddr("192.168.9.7")

	state, err := fabric.Reconcile(FabricPlan{
		Exports: []FabricExportRequest{{UniverseID: 1, DeviceID: 4, AllowedNodes: []uint32{7}}},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	nqn := state.Exports[0].NQN

	// Withdrawing the RDMA address must not strand the link it left behind:
	// configfs refuses to remove a subsystem that is still linked into a port.
	fabric.SetRDMAAddr("")

	if _, err := fabric.Reconcile(FabricPlan{}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if links := portLinks(t, fabric, nqn); len(links) != 0 {
		t.Fatalf("subsystem is still linked into %v", links)
	}

	if _, err := os.Stat(filepath.Join(fabric.nvmetRoot, "subsystems", nqn)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subsystem survived the prune: %v", err)
	}
}

func TestPruneLeavesForeignSubsystemsAlone(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

	// Another node's subsystem on a shared target, and something that is not
	// racer's at all.
	foreign := []string{
		fabric.SubsystemNQN(1, 9),
		"nqn.2014-08.org.nvmexpress:uuid:something-else",
	}

	for _, nqn := range foreign {
		if err := os.Mkdir(filepath.Join(fabric.nvmetRoot, "subsystems", nqn), 0o755); err != nil {
			t.Fatalf("seed %s: %v", nqn, err)
		}
	}

	if _, err := fabric.Reconcile(FabricPlan{}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	for _, nqn := range foreign {
		if _, err := os.Stat(filepath.Join(fabric.nvmetRoot, "subsystems", nqn)); err != nil {
			t.Fatalf("prune removed %s: %v", nqn, err)
		}
	}
}

func TestAttachSelectsTheRequestedTransport(t *testing.T) {
	fabric, connector := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

	state, err := fabric.Reconcile(FabricPlan{
		Imports: []FabricImportRequest{
			{UniverseID: 1, PeerNodeID: 8, NQN: "nqn.test.u1.n8", Addr: "[fd00::8]:4421", Trtype: "rdma"},
			{UniverseID: 1, PeerNodeID: 9, NQN: "nqn.test.u1.n9", Addr: "10.0.0.9:4420"},
		},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(connector.requests) != 2 {
		t.Fatalf("issued %d connects, want 2", len(connector.requests))
	}

	rdma := connector.requests[0]
	if rdma.Trtype != "rdma" || rdma.Traddr != "fd00::8" || rdma.Trsvcid != "4421" || rdma.Adrfam != "ipv6" {
		t.Fatalf("rdma connect %+v", rdma)
	}

	// An import with no transport named is TCP, which is the transport every
	// node can be reached on.
	tcp := connector.requests[1]
	if tcp.Trtype != "tcp" || tcp.Traddr != "10.0.0.9" || tcp.Adrfam != "ipv4" {
		t.Fatalf("tcp connect %+v", tcp)
	}

	for _, peer := range []uint32{8, 9} {
		if state.Attachments[racerctrl.Attachment{Universe: 1, Peer: peer}] == "" {
			t.Fatalf("peer %d has no attachment", peer)
		}
	}
}

func TestAttachReplacesControllerWhenEndpointChanges(t *testing.T) {
	tests := []struct {
		name  string
		first FabricImportRequest
		next  FabricImportRequest
	}{
		{
			name:  "address",
			first: FabricImportRequest{UniverseID: 1, PeerNodeID: 8, NQN: "nqn.test.u1.n8", Addr: "10.0.0.8:4420"},
			next:  FabricImportRequest{UniverseID: 1, PeerNodeID: 8, NQN: "nqn.test.u1.n8", Addr: "10.0.1.8:4420"},
		},
		{
			name:  "rdma to tcp",
			first: FabricImportRequest{UniverseID: 1, PeerNodeID: 8, NQN: "nqn.test.u1.n8", Addr: "192.168.9.8:4421", Trtype: fabricTrtypeRDMA},
			next:  FabricImportRequest{UniverseID: 1, PeerNodeID: 8, NQN: "nqn.test.u1.n8", Addr: "10.0.0.8:4420", Trtype: fabricTrtypeTCP},
		},
		{
			name:  "tcp to rdma",
			first: FabricImportRequest{UniverseID: 1, PeerNodeID: 8, NQN: "nqn.test.u1.n8", Addr: "10.0.0.8:4420", Trtype: fabricTrtypeTCP},
			next:  FabricImportRequest{UniverseID: 1, PeerNodeID: 8, NQN: "nqn.test.u1.n8", Addr: "[fd00::8]:4421", Trtype: fabricTrtypeRDMA},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fabric, connector := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

			if _, err := fabric.Reconcile(FabricPlan{Imports: []FabricImportRequest{test.first}}); err != nil {
				t.Fatalf("initial attach: %v", err)
			}

			state, err := fabric.Reconcile(FabricPlan{Imports: []FabricImportRequest{test.next}})
			if err != nil {
				t.Fatalf("replace endpoint: %v", err)
			}

			if len(connector.disconnected) != 1 || connector.disconnected[0] != "nvme0" {
				t.Fatalf("disconnected controllers %v, want nvme0", connector.disconnected)
			}

			if len(connector.requests) != 2 {
				t.Fatalf("issued %d connects, want initial and replacement", len(connector.requests))
			}

			wantHost, wantPort, splitErr := splitFabricAddr(test.next.Addr)
			if splitErr != nil {
				t.Fatalf("split next endpoint: %v", splitErr)
			}

			wantTrtype := test.next.Trtype
			if wantTrtype == "" {
				wantTrtype = fabricTrtypeTCP
			}

			request := connector.requests[1]
			if request.Trtype != wantTrtype || request.Traddr != wantHost || request.Trsvcid != wantPort {
				t.Fatalf("replacement connect %+v, want %s %s:%s", request, wantTrtype, wantHost, wantPort)
			}

			if state.Attachments[racerctrl.Attachment{Universe: 1, Peer: 8}] != "/dev/nvme1n1" {
				t.Fatalf("replacement attachment: %v", state.Attachments)
			}
		})
	}
}

func TestAttachReusesUnchangedEndpoint(t *testing.T) {
	fabric, connector := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})
	imp := FabricImportRequest{
		UniverseID: 1,
		PeerNodeID: 8,
		NQN:        "nqn.test.u1.n8",
		Addr:       "[fd00::8]:4421",
		Trtype:     fabricTrtypeRDMA,
	}

	if _, err := fabric.Reconcile(FabricPlan{Imports: []FabricImportRequest{imp}}); err != nil {
		t.Fatalf("initial attach: %v", err)
	}

	if _, err := fabric.Reconcile(FabricPlan{Imports: []FabricImportRequest{imp}}); err != nil {
		t.Fatalf("idempotent attach: %v", err)
	}

	if len(connector.requests) != 1 || len(connector.disconnected) != 0 {
		t.Fatalf("unchanged endpoint connected %d and disconnected %v", len(connector.requests), connector.disconnected)
	}
}

func TestPruneDisconnectsStaleEndpointForDesiredNQN(t *testing.T) {
	fabric, connector := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

	old := FabricImportRequest{
		UniverseID: 1,
		PeerNodeID: 8,
		NQN:        fabric.SubsystemNQN(1, 8),
		Addr:       "10.0.0.8:4420",
	}
	if _, err := fabric.Reconcile(FabricPlan{Imports: []FabricImportRequest{old}}); err != nil {
		t.Fatalf("initial attach: %v", err)
	}

	// Exercise pruning independently of attach: the desired NQN remains, but at
	// another endpoint.
	next := FabricPlan{Imports: []FabricImportRequest{{
		UniverseID: 1,
		PeerNodeID: 8,
		NQN:        old.NQN,
		Addr:       "10.0.1.8:4420",
	}}}
	if err := fabric.pruneControllers(next); err != nil {
		t.Fatalf("prune stale endpoint: %v", err)
	}

	if len(connector.disconnected) != 1 || connector.disconnected[0] != "nvme0" {
		t.Fatalf("disconnected controllers %v, want stale nvme0", connector.disconnected)
	}
}

func TestPruneMalformedEndpointKeepsItsControllerAndPrunesOthers(t *testing.T) {
	fabric, connector := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})
	first := FabricImportRequest{
		UniverseID: 1,
		PeerNodeID: 8,
		NQN:        fabric.SubsystemNQN(1, 8),
		Addr:       "10.0.0.8:4420",
	}

	second := FabricImportRequest{
		UniverseID: 1,
		PeerNodeID: 9,
		NQN:        fabric.SubsystemNQN(1, 9),
		Addr:       "10.0.0.9:4420",
	}
	if _, err := fabric.Reconcile(FabricPlan{Imports: []FabricImportRequest{first, second}}); err != nil {
		t.Fatalf("initial attach: %v", err)
	}

	connector.disconnected = nil

	first.Addr = "malformed"
	if err := fabric.pruneControllers(FabricPlan{Imports: []FabricImportRequest{first}}); err == nil {
		t.Fatal("malformed endpoint was not reported")
	}

	if len(connector.disconnected) != 1 || connector.disconnected[0] != "nvme1" {
		t.Fatalf("disconnected controllers %v, want only obsolete nvme1", connector.disconnected)
	}
}

func TestSplitFabricAddr(t *testing.T) {
	tests := []struct {
		addr string
		host string
		port string
		bad  bool
	}{
		{addr: "10.0.0.7:4420", host: "10.0.0.7", port: "4420"},
		{addr: "[fd00::7]:4420", host: "fd00::7", port: "4420"},
		{addr: "node-7.example:4420", host: "node-7.example", port: "4420"},
		{addr: "10.0.0.7", bad: true},
		{addr: "", bad: true},
		{addr: "fd00::7:4420", bad: true},
	}

	for _, test := range tests {
		t.Run(test.addr, func(t *testing.T) {
			host, port, err := splitFabricAddr(test.addr)
			if test.bad {
				if err == nil {
					t.Fatalf("accepted %q as %s/%s", test.addr, host, port)
				}

				return
			}

			if err != nil {
				t.Fatalf("rejected %q: %v", test.addr, err)
			}

			if host != test.host || port != test.port {
				t.Fatalf("split %q into %s/%s, want %s/%s", test.addr, host, port, test.host, test.port)
			}
		})
	}
}

func TestSplitRDMAAddr(t *testing.T) {
	tests := []struct {
		addr string
		host string
		port int
	}{
		{addr: "10.0.0.7", host: "10.0.0.7", port: DefaultRDMAPort},
		{addr: "10.0.0.7:4433", host: "10.0.0.7", port: 4433},
		{addr: "fd00::7", host: "fd00::7", port: DefaultRDMAPort},
		{addr: "[fd00::7]", host: "fd00::7", port: DefaultRDMAPort},
		{addr: "[fd00::7]:4433", host: "fd00::7", port: 4433},
	}

	for _, test := range tests {
		t.Run(test.addr, func(t *testing.T) {
			host, port := splitRDMAAddr(test.addr, DefaultRDMAPort)
			if host != test.host || port != test.port {
				t.Fatalf("split %q into %s/%d, want %s/%d", test.addr, host, port, test.host, test.port)
			}
		})
	}
}

func TestAdrfamOf(t *testing.T) {
	tests := map[string]string{
		"10.0.0.7":  adrfamIPv4,
		"fd00::7":   adrfamIPv6,
		"[fd00::7]": adrfamIPv6,
		// A name is assumed v4: nvmet rejects a mismatch loudly, and guessing
		// v6 for every hostname would break every v4 cluster.
		"node-7.example": adrfamIPv4,
		"":               adrfamIPv4,
	}

	for host, want := range tests {
		if got := adrfamOf(host); got != want {
			t.Fatalf("adrfam of %q is %s, want %s", host, got, want)
		}
	}
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	n, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}

	return n
}

// Several racer nodes may share one kernel, and one nvmet target is shared by
// everything on the box. The next four tests cover what that costs.

func TestReconcileRefusesAForeignPortOnThePreferredID(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

	// Another node got to the preferred id first and published its own
	// address there. Linking into it would advertise this node's namespace on
	// the other node's address.
	foreign := filepath.Join(fabric.nvmetRoot, "ports", strconv.Itoa(DefaultFabricPort))
	if err := os.MkdirAll(filepath.Join(foreign, "subsystems"), 0o755); err != nil {
		t.Fatalf("seed the foreign port: %v", err)
	}

	for attr, value := range map[string]string{
		"addr_trtype":  "tcp",
		"addr_adrfam":  adrfamIPv4,
		"addr_traddr":  "10.0.0.8",
		"addr_trsvcid": strconv.Itoa(DefaultFabricPort),
	} {
		if err := os.WriteFile(filepath.Join(foreign, attr), []byte(value), 0o644); err != nil {
			t.Fatalf("seed %s: %v", attr, err)
		}
	}

	state, err := fabric.Reconcile(FabricPlan{
		Exports: []FabricExportRequest{{UniverseID: 1, DeviceID: 4, AllowedNodes: []uint32{7}}},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if links := portLinks(t, fabric, state.Exports[0].NQN); len(links) != 1 ||
		links[0] == strconv.Itoa(DefaultFabricPort) {
		t.Fatalf("subsystem linked into %v, want a port of its own", links)
	}

	// The other node's port is left exactly as it was found.
	if attrs := portAttrs(t, fabric, DefaultFabricPort); attrs["addr_traddr"] != "10.0.0.8" {
		t.Fatalf("foreign port attributes %v", attrs)
	}

	// And this node still advertises its own address, not the squatter's.
	if state.Exports[0].Addr != "10.0.0.7:"+strconv.Itoa(DefaultFabricPort) {
		t.Fatalf("advertised %q", state.Exports[0].Addr)
	}
}

func TestReconcileReusesAPortWithTheSameAddress(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

	// The same address under a different id is this node's port whoever made
	// it: the id carries no meaning, the address does.
	existing := filepath.Join(fabric.nvmetRoot, "ports", "12")
	if err := os.MkdirAll(filepath.Join(existing, "subsystems"), 0o755); err != nil {
		t.Fatalf("seed the port: %v", err)
	}

	for attr, value := range map[string]string{
		"addr_trtype":  "tcp",
		"addr_adrfam":  adrfamIPv4,
		"addr_traddr":  "10.0.0.7",
		"addr_trsvcid": strconv.Itoa(DefaultFabricPort),
	} {
		if err := os.WriteFile(filepath.Join(existing, attr), []byte(value), 0o644); err != nil {
			t.Fatalf("seed %s: %v", attr, err)
		}
	}

	state, err := fabric.Reconcile(FabricPlan{
		Exports: []FabricExportRequest{{UniverseID: 1, DeviceID: 4, AllowedNodes: []uint32{7}}},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if links := portLinks(t, fabric, state.Exports[0].NQN); len(links) != 1 || links[0] != "12" {
		t.Fatalf("subsystem linked into %v, want the existing port 12", links)
	}

	if portAttrs(t, fabric, DefaultFabricPort) != nil {
		t.Fatal("a second port was created for an address that already had one")
	}
}

func TestAttachLeavesAnotherHostsControllerAlone(t *testing.T) {
	fabric, connector := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

	// A controller onto the same target, opened by a different node sharing
	// this kernel. Adopting it would hand this node a device it is not
	// entitled to; disconnecting it would cut the other node off.
	foreign := filepath.Join(fabric.sysClassNvme, "nvme3")
	if err := os.MkdirAll(filepath.Join(foreign, "nvme3n1"), 0o755); err != nil {
		t.Fatalf("seed the foreign controller: %v", err)
	}

	for attr, value := range map[string]string{
		"subsysnqn": "nqn.test.u1.n9",
		"hostnqn":   fabric.HostNQN(11),
	} {
		if err := os.WriteFile(filepath.Join(foreign, attr), []byte(value), 0o644); err != nil {
			t.Fatalf("seed %s: %v", attr, err)
		}
	}

	state, err := fabric.Reconcile(FabricPlan{
		Imports: []FabricImportRequest{
			{UniverseID: 1, PeerNodeID: 9, NQN: "nqn.test.u1.n9", Addr: "10.0.0.9:4420"},
		},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(connector.requests) != 1 {
		t.Fatalf("issued %d connects, want one of this node's own", len(connector.requests))
	}

	if path := state.Attachments[racerctrl.Attachment{Universe: 1, Peer: 9}]; path == "/dev/nvme3n1" {
		t.Fatal("adopted the other node's controller")
	}

	// Pruning everything must still leave the stranger connected.
	if _, err := fabric.Reconcile(FabricPlan{}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	for _, name := range connector.disconnected {
		if name == "nvme3" {
			t.Fatal("disconnected the other node's controller")
		}
	}

	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("the other node's controller was removed: %v", err)
	}
}

func TestConnectCarriesAHostIDDerivedFromTheHostNQN(t *testing.T) {
	fabric, connector := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})

	if _, err := fabric.Reconcile(FabricPlan{
		Imports: []FabricImportRequest{
			{UniverseID: 1, PeerNodeID: 9, NQN: "nqn.test.u1.n9", Addr: "10.0.0.9:4420"},
		},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The kernel keys its host table by id, so an omitted or shared id is
	// rejected outright once anything else on the box has connected.
	req := connector.requests[0]
	if req.HostNQN != fabric.HostNQN(7) {
		t.Fatalf("connected as %q", req.HostNQN)
	}

	if req.HostID != fabric.HostID(7) {
		t.Fatalf("host id %q, want %q", req.HostID, fabric.HostID(7))
	}

	if len(req.HostID) != 36 || req.HostID[14] != '5' {
		t.Fatalf("host id %q is not a version 5 UUID", req.HostID)
	}

	// Different nodes, different ids: that is the whole point.
	if fabric.HostID(7) == fabric.HostID(8) {
		t.Fatal("two nodes derived the same host id")
	}

	// And the same node derives the same id every time it starts.
	if fabric.HostID(7) != NewFabric(Config{NQNPrefix: fabric.nqnPrefix}, 7, nil).HostID(7) {
		t.Fatal("host id is not stable across restarts")
	}
}
