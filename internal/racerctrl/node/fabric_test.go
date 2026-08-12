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

	return os.WriteFile(filepath.Join(dir, "subsysnqn"), []byte(req.NQN), 0o644)
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

func TestReconcileDegradesToTCPWhenRDMAFails(t *testing.T) {
	fabric, _ := newTestFabric(t, Config{FabricAddr: "10.0.0.7"})
	fabric.SetRDMAAddr("192.168.9.7")

	// A port directory the kernel refuses to create is what a missing
	// nvmet_rdma looks like from here. Standing in a plain file for it makes
	// the mkdir fail the same way.
	blocked := filepath.Join(fabric.nvmetRoot, "ports", strconv.Itoa(DefaultRDMAPort))
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
