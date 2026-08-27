//go:build e2e

// Package racerdpe2e drives the racer dataplane directly, with no Kubernetes and no
// control plane in the picture.
//
// The suite starts six real `racer serve` processes on the local host. They form one
// universe split across two zones of three nodes each, so the universe holds exactly two
// consensus groups: one trio per zone. Every node exports its universe fabric namespace
// and five client block devices as ublk minors, and the peers open each other's fabric
// minors directly. On a single host that removes any need for NVMe-oF, nvmet, brd or a
// dedicated filesystem: the stores are plain files on the repository's own ext4 scratch
// directory, which honours O_DIRECT and RWF_DSYNC.
//
// The point of the suite is the dataplane contract that the kind-based e2e/racer suite
// does not touch at all: OCC guard semantics, immutable write-once and free-once blocks,
// immutable stripe placement, cache admission policy, proactive cross-zone warm push, and gateway
// routing between the two zones. It shares no code, no fixtures, no scratch
// directory, no ublk minor range and no Makefile target with e2e/racer, and it is written
// only against racer's published contract: the api/racer protobuf schema, the `racer
// serve` command line, the `/dev/ublkb<minor>` exports that the config names, and the
// Prometheus exposition on METRICS_ADDR.
package racerdpe2e

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

const (
	// universeID is the single universe every node joins.
	universeID = 1
	// epoch rides in the trailer of every routed write.
	epoch = 1
	// blockSize is the logical block every racer device exports.
	blockSize = 4096
	// storeBytes is the backing file length every node is held at. A node refuses to
	// start when its store is short of what its own zone's extents need, and racer sizes
	// for metadata as well as pages, so this is comfortably above the roughly 516 MiB the
	// heavier of the two zones asks for rather than trimmed to it.
	storeBytes = 1 << 30
	// openFiles is the file descriptor limit each racer process is given. Sudo drops the
	// soft limit to 1024, which racer exhausts while building a generation: the inotify
	// instance the config watcher wants is then refused, the watcher thread reports
	// "config watch stopped" and the process exits 1 a few milliseconds after it started
	// serving. Anything above the default is enough; this is the hard limit.
	openFiles = 1 << 20
	// zoneA and zoneB are the two fault domains.
	zoneA = 1
	zoneB = 2
	// cpusPerNode is how many logical CPUs each racer process is pinned to. Racer folds
	// SMT siblings, so four logical CPUs is two workers, which is plenty for a test and
	// keeps six processes from each spinning up a worker per core on a large host.
	cpusPerNode = 4
)

// Timeouts. The whole suite is meant to finish inside five minutes, so every wait is
// short enough that a wedged step reports rather than sits. Anything racer does in
// response to a local request is milliseconds; the generous ones below are only generous
// because they cover a process start, a config reload, or an anti-entropy sweep.
const (
	// startTimeout covers spawning racer, formatting a store and opening ublk.
	startTimeout = 20 * time.Second
	// reloadTimeout covers an inotify pickup plus a generation swap.
	reloadTimeout = 15 * time.Second
	// deviceTimeout covers the kernel creating or removing a ublk minor.
	deviceTimeout = 20 * time.Second
	// stopTimeout covers a graceful shutdown.
	stopTimeout = 20 * time.Second
	// metricTimeout covers a counter catching up with work already acknowledged. A
	// publish tick is 250 ms, so this is many ticks of slack.
	metricTimeout = 15 * time.Second
	// healTimeout covers an anti-entropy sweep noticing and replaying a wiped member,
	// which is the one background process in racer slow enough to need real patience.
	healTimeout = 60 * time.Second
	// pollInterval is how often any of the above re-checks.
	pollInterval = 20 * time.Millisecond
)

// nodeIDs is the fixed roster. Nodes 1-3 are zone A, nodes 4-6 are zone B, and each
// node's position in its zone is its cohort.
var nodeIDs = []uint32{1, 2, 3, 4, 5, 6}

// Extent identifiers. Every extent lives in exactly one zone and is described identically
// in all six configs, so a node that does not store an extent still routes for it.
const (
	extRWA      = 1 // Mutable homed in zone A.
	extRWB      = 2 // Mutable homed in zone B, the mirror image of extRWA.
	extOCCA     = 3 // Mutable homed in zone A, exercised for OCC semantics.
	extOCCB     = 4 // Mutable homed in zone B, exercised across the gateway.
	extImmWarmA = 5 // Immutable in zone A, warmed proactively into zone B.
	extImmPlain = 6 // Immutable in zone A with no policy at all: the control.
	extImmWarmB = 7 // Immutable in zone B, warmed proactively into zone A.
	extStripe   = 8 // Two immutable placement stripes homed in zone B.
)

// Device slots. A node's ublk minor is fabricMinor(id)+slot, so the fabric namespace and
// every client device of every node land on a distinct minor across the whole host.
const (
	slotPair    = 1 // extents extRWA, extRWB.
	slotOCC     = 2 // extents extOCCA, extOCCB.
	slotImm     = 3 // extents extImmWarmA, extImmPlain, extImmWarmB.
	slotStripe  = 4 // extent extStripe.
	slotReverse = 5 // extents extRWB, extRWA: the same pair, mapped the other way round.
)

// extentSpec is one row of the address space. It is the single source of truth for both
// the configs the harness installs and the offsets the tests compute.
type extentSpec struct {
	id     uint32
	base   uint64 // First logical block of the extent within the universe.
	blocks uint64 // Length in 4 KiB logical blocks.
	kind   racerconfig.Kind
	zone   uint32
	admit  uint32
	warm   []uint32
}

// extents is the whole address space. Bases are disjoint, and every immutable extent
// starts on a 1024-block placement stripe boundary.
var extents = []extentSpec{
	{id: extRWA, base: 0, blocks: 512, kind: racerconfig.Kind_MUTABLE, zone: zoneA},
	{id: extRWB, base: 512, blocks: 512, kind: racerconfig.Kind_MUTABLE, zone: zoneB},
	{id: extOCCA, base: 1024, blocks: 512, kind: racerconfig.Kind_MUTABLE, zone: zoneA},
	{id: extOCCB, base: 1536, blocks: 512, kind: racerconfig.Kind_MUTABLE, zone: zoneB},
	{id: extImmWarmA, base: 2048, blocks: 512, kind: racerconfig.Kind_IMMUTABLE, zone: zoneA, admit: 1, warm: []uint32{zoneB}},
	{id: extImmPlain, base: 3072, blocks: 512, kind: racerconfig.Kind_IMMUTABLE, zone: zoneA},
	{id: extImmWarmB, base: 4096, blocks: 512, kind: racerconfig.Kind_IMMUTABLE, zone: zoneB, admit: 1, warm: []uint32{zoneA}},
	{id: extStripe, base: 5120, blocks: 2048, kind: racerconfig.Kind_IMMUTABLE, zone: zoneB, admit: 1},
}

// deviceSpec is one exported block device: a slot and the extents concatenated onto it.
type deviceSpec struct {
	slot    int
	extents []uint32
}

// devices is what every node exports. Two hosts mapping one extent set in different
// orders is legal and slotReverse proves it.
var devices = []deviceSpec{
	{slot: slotPair, extents: []uint32{extRWA, extRWB}},
	{slot: slotOCC, extents: []uint32{extOCCA, extOCCB}},
	{slot: slotImm, extents: []uint32{extImmWarmA, extImmPlain, extImmWarmB}},
	{slot: slotStripe, extents: []uint32{extStripe}},
	{slot: slotReverse, extents: []uint32{extRWB, extRWA}},
}

// extentByID looks a row up in the address space table.
func extentByID(id uint32) extentSpec {
	for _, e := range extents {
		if e.id == id {
			return e
		}
	}

	panic(fmt.Sprintf("racer-dataplane: no extent %d in the table", id))
}

// deviceBySlot looks an exported device up by slot.
func deviceBySlot(slot int) deviceSpec {
	for _, d := range devices {
		if d.slot == slot {
			return d
		}
	}

	panic(fmt.Sprintf("racer-dataplane: no device in slot %d", slot))
}

// offsetOf is the byte offset within the device in the given slot at which the given
// extent starts. Tests address pages relative to this rather than hard-coding numbers, so
// slotReverse needs no special case.
func offsetOf(slot int, extent uint32) int64 {
	var off uint64

	for _, id := range deviceBySlot(slot).extents {
		if id == extent {
			return int64(off * blockSize)
		}

		off += extentByID(id).blocks
	}

	panic(fmt.Sprintf("racer-dataplane: device in slot %d does not map extent %d", slot, extent))
}

// fabricMinor is the ublk minor a node exports its fabric namespace on. Slots 1 through 5
// follow it, so node 1 owns 310-315, node 2 owns 320-325 and so on up to node 6 at
// 360-365. The range is deliberately clear of anything else on the host.
func fabricMinor(id uint32) uint32 {
	return 300 + 10*id
}

// deviceMinor is the ublk minor a node exports a client device on.
func deviceMinor(id uint32, slot int) uint32 {
	return fabricMinor(id) + uint32(slot)
}

// blockPath is the block device a ublk minor appears at.
func blockPath(minor uint32) string {
	return fmt.Sprintf("/dev/ublkb%d", minor)
}

// zoneOf is the fault domain a node belongs to.
func zoneOf(id uint32) uint32 {
	if id <= 3 {
		return zoneA
	}

	return zoneB
}

// cohortOf is the node's column in its zone's catalog. Member order in a group is
// normative, so this is also its position in the trio.
func cohortOf(id uint32) racerconfig.Cohort {
	return racerconfig.Cohort((id - 1) % 3)
}

// trioOf is the single consensus group of a zone.
func trioOf(zone uint32) *racerconfig.Trio {
	if zone == zoneA {
		return &racerconfig.Trio{Cohort_0: 1, Cohort_1: 2, Cohort_2: 3}
	}

	return &racerconfig.Trio{Cohort_0: 4, Cohort_1: 5, Cohort_2: 6}
}

// gatewaysOf is the zone's published gateway list. Every member of a zone is a gateway
// for it, which is what lets the failover step take one away and still be served.
func gatewaysOf(zone uint32) []uint32 {
	if zone == zoneA {
		return []uint32{1, 2, 3}
	}

	return []uint32{4, 5, 6}
}

// membersOf is the nodes of a zone.
func membersOf(zone uint32) []uint32 {
	return gatewaysOf(zone)
}

// other is the zone a node routes to.
func other(zone uint32) uint32 {
	if zone == zoneA {
		return zoneB
	}

	return zoneA
}

// harness owns the six processes, the scratch directory and the built binary.
type harness struct {
	t     *testing.T
	root  string
	exe   string
	nodes map[uint32]*node
	gen   uint64
}

// node is one `racer serve` process.
type node struct {
	h     *harness
	id    uint32
	zone  uint32
	dir   string
	store string
	cfg   string
	pidf  string
	cmd   *exec.Cmd
	log   *logbuf
	addr  string
	pid   int
	live  bool
	devs  map[int]*dev
}

// traceStart anchors every trace line to the start of the run so a reader can see how
// long each phase took without doing arithmetic on wall clocks.
var traceStart = time.Now()

// traceMu keeps interleaved writers from tearing each other's lines apart.
var traceMu sync.Mutex

// trace writes a timestamped line straight to stderr. The testing package buffers
// t.Logf until the enclosing (sub)test finishes, which hides all progress made by the
// shared harness before the first subtest runs. Writing to stderr directly means a run
// that wedges still says where it wedged, so an iteration costs one look at the tail of
// the log rather than one whole timeout.
func trace(format string, args ...any) {
	traceMu.Lock()
	defer traceMu.Unlock()

	fmt.Fprintf(os.Stderr, "[%7.2fs] %s\n", time.Since(traceStart).Seconds(), fmt.Sprintf(format, args...))
}

// logbuf accumulates a child's stdout and stderr so a test can wait for a line racer
// promises to print. Every line is echoed through trace as it arrives, so a child that
// dies or complains is visible immediately rather than at the next assertion.
type logbuf struct {
	mu    sync.Mutex
	tag   string
	lines []string
}

func (l *logbuf) add(s string) {
	l.mu.Lock()
	tag := l.tag
	l.lines = append(l.lines, s)
	l.mu.Unlock()

	trace("%s| %s", tag, s)
}

// mark is the current end of the log, so a later wait can ignore earlier occurrences.
func (l *logbuf) mark() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.lines)
}

// find returns the first line at or after from containing sub.
func (l *logbuf) find(from int, sub string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for i := from; i < len(l.lines); i++ {
		if strings.Contains(l.lines[i], sub) {
			return l.lines[i], true
		}
	}

	return "", false
}

// text is the whole log, for failure messages.
func (l *logbuf) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return strings.Join(l.lines, "\n")
}

// wait polls for a line containing sub.
func (l *logbuf) wait(from int, sub string, d time.Duration) (string, error) {
	started := time.Now()
	deadline := started.Add(d)
	next := started.Add(2 * time.Second)

	for {
		if s, ok := l.find(from, sub); ok {
			return s, nil
		}

		now := time.Now()
		if now.After(deadline) {
			return "", fmt.Errorf("no line containing %q within %s; log:\n%s", sub, d, l.text())
		}

		if now.After(next) {
			trace("%s: still waiting for %q (%.0fs of %s)", l.tag, sub, now.Sub(started).Seconds(), d)

			next = now.Add(2 * time.Second)
		}

		time.Sleep(pollInterval)
	}
}

// repoRoot walks up from the working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}

		dir = parent
	}
}

// requireKernel skips unless this host can host ublk devices at all.
func requireKernel(t *testing.T) {
	t.Helper()

	f, err := os.Open("/dev/ublk-control")
	if err == nil {
		_ = f.Close()
	} else if !errors.Is(err, os.ErrPermission) {
		t.Skipf("skipping: /dev/ublk-control is not usable: %v", err)
	}

	raw, err := os.ReadFile("/sys/module/ublk_drv/parameters/ublks_max")
	if err != nil {
		t.Skipf("skipping: ublk_drv is not loaded: %v", err)
	}

	max, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Skipf("skipping: cannot read ublks_max: %v", err)
	}

	// Six nodes export six minors each.
	if max < len(nodeIDs)*(len(devices)+1) {
		t.Skipf("skipping: ublks_max is %d, too few for %d exports", max, len(nodeIDs)*(len(devices)+1))
	}
}

// requireRoot skips unless the harness can start privileged children without a prompt.
// The test body itself stays unprivileged; it reaches the devices because the harness
// relaxes their mode once the kernel has created them.
func requireRoot(t *testing.T) {
	t.Helper()

	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		t.Skipf("skipping: passwordless sudo is required to drive ublk: %v", err)
	}
}

// requireInotify makes sure root can still open an inotify instance per node. Racer
// watches its config file with one, and treats losing the watch as fatal: the watcher
// thread reports "config watch stopped" and the process exits 1, which on a boot looks
// like a node that served for ten milliseconds and vanished. A developer box that has
// been running kind, containerd and a few editors sits at root's default ceiling of 128
// instances, so six more are refused with EMFILE. Raise it rather than skip; the setting
// is a per-user cap on a cheap kernel object and nothing else on the host wants it low.
// It is deliberately not restored, since lowering it again would break whatever claimed
// instances in the meantime.
func requireInotify(t *testing.T) {
	t.Helper()

	const knob = "/proc/sys/fs/inotify/max_user_instances"

	want := 4 * len(nodeIDs)

	raw, err := os.ReadFile(knob)
	if err != nil {
		t.Skipf("skipping: cannot read %s: %v", knob, err)
	}

	have, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Skipf("skipping: cannot parse %s: %v", knob, err)
	}

	// The ceiling counts every instance root already holds, not just ours, so headroom
	// matters more than the raw number. Anything under a few hundred is worth raising.
	if have >= 1024 {
		return
	}

	trace("harness: raising fs.inotify.max_user_instances from %d to 1024", have)

	if out, err := sudo(t, "sysctl", "-w", "fs.inotify.max_user_instances=1024"); err != nil {
		t.Skipf("skipping: %s is %d and cannot be raised (need at least %d free): %v: %s",
			knob, have, want, err, strings.TrimSpace(out))
	}
}

// sudo runs a privileged command and returns its combined output.
func sudo(t *testing.T, args ...string) (string, error) {
	t.Helper()

	out, err := exec.Command("sudo", append([]string{"-n"}, args...)...).CombinedOutput()

	return string(out), err
}

// buildRacer compiles the dataplane binary unless E2E_SKIP_BUILD asks for the one already
// there.
func buildRacer(t *testing.T, root string) string {
	t.Helper()

	exe := filepath.Join(root, "cmd", "racer", "target", "release", "racer")

	if os.Getenv("E2E_SKIP_BUILD") != "" {
		if _, err := os.Stat(exe); err != nil {
			t.Fatalf("E2E_SKIP_BUILD is set but %s is missing: %v", exe, err)
		}

		return exe
	}

	cmd := exec.Command("cargo", "build", "--release", "--manifest-path", filepath.Join(root, "cmd", "racer", "Cargo.toml"))
	cmd.Dir = root

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cargo build failed: %v\n%s", err, out)
	}

	if _, err := os.Stat(exe); err != nil {
		t.Fatalf("cargo build produced no %s: %v", exe, err)
	}

	return exe
}

// newHarness prepares the scratch tree and registers teardown. Nothing is started yet.
func newHarness(t *testing.T) *harness {
	t.Helper()

	trace("harness: checking kernel prerequisites")
	requireKernel(t)
	requireRoot(t)
	requireInotify(t)

	root := repoRoot(t)
	scratch := filepath.Join(root, "tmp", "racer-dataplane-e2e")

	// A previous crashed run may have left minors behind. Anything still present under
	// our range is a leak we cannot use, so say so rather than fail obscurely later.
	reapStale(t)

	trace("harness: clearing scratch %s", scratch)

	if out, err := sudo(t, "rm", "-rf", scratch); err != nil {
		t.Fatalf("clearing %s: %v\n%s", scratch, err, out)
	}

	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", scratch, err)
	}

	trace("harness: resolving racer binary")

	h := &harness{t: t, root: scratch, exe: buildRacer(t, root), nodes: map[uint32]*node{}, gen: 1}

	trace("harness: racer binary is %s", h.exe)

	for _, id := range nodeIDs {
		dir := filepath.Join(scratch, fmt.Sprintf("n%d", id))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}

		h.nodes[id] = &node{
			h:     h,
			id:    id,
			zone:  zoneOf(id),
			dir:   dir,
			store: filepath.Join(dir, "store.img"),
			cfg:   filepath.Join(dir, "node.pb"),
			pidf:  filepath.Join(dir, "racer.pid"),
			log:   &logbuf{tag: fmt.Sprintf("n%d", id)},
		}
	}

	t.Cleanup(h.teardown)

	return h
}

// reapStale reports any minor in our range left behind by an earlier run.
//
// It does not fail the suite. A node reclaims a minor whose exporter died before it
// exports its own, so a leak from a previous run is cleaned up by the very node that
// wants the number back. Only a minor still served by a live process would stop a
// boot, and that is reported where it happens, with the pid that holds it.
func reapStale(t *testing.T) {
	t.Helper()

	for _, id := range nodeIDs {
		for slot := 0; slot <= len(devices); slot++ {
			p := blockPath(fabricMinor(id) + uint32(slot))
			if _, err := os.Stat(p); err == nil {
				trace("harness: %s was left behind by an earlier run, reclaiming it", p)
			}
		}
	}
}

// all returns the nodes in roster order.
func (h *harness) all() []*node {
	out := make([]*node, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		out = append(out, h.nodes[id])
	}

	return out
}

// zone returns the nodes of one fault domain, in roster order.
func (h *harness) zone(z uint32) []*node {
	var out []*node

	for _, id := range membersOf(z) {
		out = append(out, h.nodes[id])
	}

	return out
}

// node looks one process up.
func (h *harness) node(id uint32) *node {
	n, ok := h.nodes[id]
	if !ok {
		h.t.Fatalf("no node %d", id)
	}

	return n
}

// live returns the nodes currently serving.
func (h *harness) live() []*node {
	var out []*node

	for _, n := range h.all() {
		if n.live {
			out = append(out, n)
		}
	}

	return out
}

// config renders one node's whole configuration at the given generation. withPeers is
// false only for the first generation: a node cannot open a peer's fabric namespace
// before that peer has created it, so the cluster boots isolated and is wired afterwards.
func (h *harness) config(n *node, gen uint64, withPeers bool) *racerconfig.NodeConfig {
	u := &racerconfig.Universe{
		Id:             universeID,
		Epoch:          epoch,
		Catalog:        []*racerconfig.Trio{trioOf(n.zone)},
		FabricDeviceId: fabricMinor(n.id),
	}

	// A universe's catalog covers our own zone only. The other zone is reachable through
	// its published gateways and nothing else.
	u.Zones = []*racerconfig.Zone{{Id: other(n.zone), Gateways: gatewaysOf(other(n.zone))}}

	if withPeers {
		for _, id := range nodeIDs {
			if id == n.id {
				continue
			}

			u.Peers = append(u.Peers, &racerconfig.Peer{Id: id, Device: blockPath(fabricMinor(id))})
		}
	}

	for _, e := range extents {
		u.Extents = append(u.Extents, &racerconfig.Extent{
			Id:         e.id,
			BaseLba:    e.base,
			Blocks:     e.blocks,
			Kind:       e.kind,
			Zone:       e.zone,
			CacheAdmit: e.admit,
			WarmZones:  append([]uint32(nil), e.warm...),
		})
	}

	cfg := &racerconfig.NodeConfig{
		Generation: gen,
		Node: &racerconfig.Node{
			Id:     n.id,
			Zone:   n.zone,
			Cohort: cohortOf(n.id).Enum(),
			Store:  &racerconfig.Store{SizeBytes: storeBytes},
		},
		Universes: []*racerconfig.Universe{u},
	}

	for _, d := range devices {
		cfg.Devices = append(cfg.Devices, &racerconfig.Device{
			Id:      deviceMinor(n.id, d.slot),
			Extents: append([]uint32(nil), d.extents...),
		})
	}

	return cfg
}

// write serialises a configuration into the node's directory by rename, which is the
// contract racer's inotify watch expects.
func (n *node) write(cfg *racerconfig.NodeConfig) {
	n.h.t.Helper()

	raw, err := proto.Marshal(cfg)
	if err != nil {
		n.h.t.Fatalf("node %d: marshalling config: %v", n.id, err)
	}

	next := n.cfg + ".next"
	if err := os.WriteFile(next, raw, 0o644); err != nil {
		n.h.t.Fatalf("node %d: writing %s: %v", n.id, next, err)
	}

	if err := os.Rename(next, n.cfg); err != nil {
		n.h.t.Fatalf("node %d: renaming over %s: %v", n.id, n.cfg, err)
	}
}

// start spawns the process. The pid file is written by a shell that then execs racer, so
// it names racer itself and not the sudo wrapper: signalling the right process matters to
// the crash and failover steps.
func (n *node) start() {
	t := n.h.t
	t.Helper()

	if n.live {
		t.Fatalf("node %d is already running", n.id)
	}

	_ = os.Remove(n.pidf)

	lo := int(n.id-1) * cpusPerNode
	cpus := fmt.Sprintf("%d-%d", lo, lo+cpusPerNode-1)

	script := fmt.Sprintf(
		"ulimit -n %d; echo $$ > %s; exec env METRICS_ADDR=127.0.0.1:0 RACER_STORE=%s %s serve %s",
		openFiles, n.pidf, n.store, n.h.exe, n.cfg,
	)

	cmd := exec.Command("sudo", "-n", "taskset", "-c", cpus, "/bin/sh", "-c", script)

	trace("n%d: spawning racer on cpus %s", n.id, cpus)

	// A restarted node has announced a listener before, on a port the kernel has since
	// taken back, so only the lines this incarnation writes may be read.
	mark := n.log.mark()

	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("node %d: stdout pipe: %v", n.id, err)
	}

	errPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("node %d: stderr pipe: %v", n.id, err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("node %d: starting racer: %v", n.id, err)
	}

	n.cmd = cmd
	n.live = true

	go drain(n.log, out)
	go drain(n.log, errPipe)

	line, err := n.log.wait(mark, "metrics -> ", startTimeout)
	if err != nil {
		t.Fatalf("node %d: never announced its metrics listener: %v", n.id, err)
	}

	n.addr = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "metrics -> "))

	n.pid = n.readPID()

	trace("n%d: serving, pid %d, metrics %s", n.id, n.pid, n.addr)

	n.publish()

	trace("n%d: exports ready", n.id)
}

// drain copies a child pipe into the log.
func drain(l *logbuf, r io.Reader) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for s.Scan() {
		l.add(s.Text())
	}
}

// readPID waits for the shell to have recorded racer's pid.
func (n *node) readPID() int {
	t := n.h.t
	t.Helper()

	deadline := time.Now().Add(startTimeout)

	for {
		raw, err := os.ReadFile(n.pidf)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("node %d: no pid in %s", n.id, n.pidf)
		}

		time.Sleep(pollInterval)
	}
}

// exports is every ublk minor this node owns.
func (n *node) exports() []uint32 {
	out := []uint32{fabricMinor(n.id)}
	for _, d := range devices {
		out = append(out, deviceMinor(n.id, d.slot))
	}

	return out
}

// publish waits for the kernel to create every minor this node's config names and relaxes
// its mode so the unprivileged test body can open it.
func (n *node) publish() {
	t := n.h.t
	t.Helper()

	var paths []string

	for _, minor := range n.exports() {
		p := blockPath(minor)
		waitForPath(t, p, deviceTimeout, n.log)
		paths = append(paths, p)
	}

	settleUdev(t)

	if out, err := sudo(t, append([]string{"chmod", "0666"}, paths...)...); err != nil {
		t.Fatalf("node %d: relaxing device modes: %v\n%s", n.id, err, out)
	}
}

// udevStuck records that the host's udev queue would not drain, so the rest of the run
// stops asking. A queue that is stuck once stays stuck.
var udevStuck bool

// settleUdev waits for udev to finish with the exports that just appeared.
//
// The kernel hands every new export to udev, which opens it looking for a partition
// table. An open still in flight when the exporting process dies deadlocks against the
// removal of the disk: the removal waits for the opener to let go, the opener waits for
// the removal to release the disk, and neither task can be killed. Draining the queue
// before the suite starts stopping nodes keeps the two apart, and doing it before the
// modes are relaxed stops udev putting them back.
//
// A queue that will not drain belongs to somebody else on a shared host, so it is
// reported once and then left alone.
func settleUdev(t *testing.T) {
	t.Helper()

	if udevStuck {
		return
	}

	if out, err := sudo(t, "udevadm", "settle", "--timeout=10"); err != nil {
		udevStuck = true

		trace("harness: udev will not settle, continuing without it: %v\n%s", err, out)
	}
}

// waitForPath polls until a device node shows up.
func waitForPath(t *testing.T, p string, d time.Duration, l *logbuf) {
	t.Helper()

	deadline := time.Now().Add(d)

	for {
		if _, err := os.Stat(p); err == nil {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared within %s; log:\n%s", p, d, l.text())
		}

		time.Sleep(pollInterval)
	}
}

// boot starts all six processes isolated, then wires the full peer mesh in a second
// generation once every fabric namespace exists.
func (h *harness) boot() {
	t := h.t
	t.Helper()

	trace("boot: starting six nodes unwired")

	for _, n := range h.all() {
		n.write(h.config(n, 1, false))
		n.start()
	}

	h.gen = 2

	trace("boot: wiring the peer mesh at generation %d", h.gen)

	for _, n := range h.all() {
		mark := n.log.mark()
		n.write(h.config(n, h.gen, true))

		if _, err := n.log.wait(mark, "racer: configuration applied", reloadTimeout); err != nil {
			t.Fatalf("node %d: never applied the wiring generation: %v", n.id, err)
		}
	}

	for _, n := range h.all() {
		n.awaitGeneration(h.gen)
	}

	trace("boot: six nodes at generation %d", h.gen)
}

// reconfigure installs a fresh generation everywhere, applying edit to each node's
// configuration first.
func (h *harness) reconfigure(edit func(n *node, cfg *racerconfig.NodeConfig)) {
	t := h.t
	t.Helper()

	h.gen++

	trace("reconfigure: installing generation %d", h.gen)

	for _, n := range h.live() {
		cfg := h.config(n, h.gen, true)
		if edit != nil {
			edit(n, cfg)
		}

		mark := n.log.mark()
		n.write(cfg)

		if _, err := n.log.wait(mark, "racer: configuration applied", reloadTimeout); err != nil {
			t.Fatalf("node %d: never applied generation %d: %v", n.id, h.gen, err)
		}
	}

	for _, n := range h.live() {
		n.awaitGeneration(h.gen)
	}

	trace("reconfigure: generation %d in force", h.gen)
}

// detach installs a generation in which every live node names each peer except one, and
// attach puts the full mesh back.
//
// This is an artefact of running the whole cluster on one host. A peer link is an open file
// on the exporting node's own ublk block device, so while the rest of the mesh holds a dead
// node's fabric minor open the kernel will not let that node re-export it, and a returning
// node fails with "device N is still open by a consumer of the export that died with our
// predecessor". A real deployment reaches a peer through its own NVMe-oF namespace, which
// the target's death takes with it, so nothing there has to be told. Only the links go: the
// catalog still names the absent node, because its length may not change across a reload.
func (h *harness) detach(id uint32) {
	h.t.Helper()

	h.reconfigure(func(_ *node, cfg *racerconfig.NodeConfig) {
		u := cfg.Universes[0]
		kept := make([]*racerconfig.Peer, 0, len(u.Peers))

		for _, p := range u.Peers {
			if p.Id != id {
				kept = append(kept, p)
			}
		}

		u.Peers = kept
	})
}

func (h *harness) attach() {
	h.t.Helper()

	h.reconfigure(nil)
}

// awaitGeneration blocks until the node reports the generation in force through metrics.
// Metrics lag work by up to one publishing tick, so this is the point at which a
// subsequent scrape can be trusted.
func (n *node) awaitGeneration(gen uint64) {
	t := n.h.t
	t.Helper()

	deadline := time.Now().Add(reloadTimeout)

	// What the last scrape said, so a node that comes up but never takes the file can be
	// told apart from one that never answers at all.
	var saw string

	for {
		s, err := n.trySample()
		switch {
		case err != nil:
			saw = "scrape failed: " + err.Error()
		case s["racer_config_generation"] == gen:
			return
		default:
			saw = fmt.Sprintf("generation %d, rejected %d",
				s["racer_config_generation"], s["racer_config_rejected_total"])
		}

		if time.Now().After(deadline) {
			t.Fatalf("node %d: never reached generation %d, last saw %s; log:\n%s",
				n.id, gen, saw, n.log.text())
		}

		time.Sleep(50 * time.Millisecond)
	}
}

// signal delivers a signal to racer itself.
func (n *node) signal(sig string) {
	t := n.h.t
	t.Helper()

	if out, err := sudo(t, "kill", "-"+sig, strconv.Itoa(n.pid)); err != nil {
		t.Fatalf("node %d: kill -%s %d: %v\n%s", n.id, sig, n.pid, err, out)
	}
}

// stop takes the node down and waits for its exports to disappear, so a later start can
// claim the same minors. graceful chooses SIGTERM over SIGKILL.
func (n *node) stop(graceful bool) {
	t := n.h.t
	t.Helper()

	if !n.live {
		return
	}

	trace("n%d: stopping (graceful=%v)", n.id, graceful)

	n.shut()

	if graceful {
		n.signal("TERM")
	} else {
		n.signal("KILL")
	}

	done := make(chan struct{})

	go func() {
		_ = n.cmd.Wait()

		close(done)
	}()

	select {
	case <-done:
	case <-time.After(stopTimeout):
		_, _ = sudo(t, "kill", "-KILL", strconv.Itoa(n.pid))

		<-done
	}

	n.live = false

	for _, minor := range n.exports() {
		p := blockPath(minor)
		deadline := time.Now().Add(deviceTimeout)

		for {
			if _, err := os.Stat(p); err != nil {
				break
			}

			if time.Now().After(deadline) {
				t.Fatalf("node %d: %s outlived the process", n.id, p)
			}

			time.Sleep(pollInterval)
		}
	}

	trace("n%d: stopped", n.id)
}

// restart brings a stopped node back with the generation currently in force.
func (n *node) restart() {
	n.h.t.Helper()

	trace("n%d: restarting at generation %d", n.id, n.h.gen)

	n.write(n.h.config(n, n.h.gen, true))
	n.start()
	n.awaitGeneration(n.h.gen)
}

// teardown stops everything that is still up and reports anything it leaked. It runs even
// when the test has already failed, so it never calls Fatalf.
func (h *harness) teardown() {
	t := h.t

	trace("teardown: begin")

	for _, n := range h.all() {
		if !n.live {
			continue
		}

		n.shut()

		_, _ = sudo(t, "kill", "-TERM", strconv.Itoa(n.pid))
	}

	for _, n := range h.all() {
		if !n.live {
			continue
		}

		done := make(chan struct{})

		go func() {
			_ = n.cmd.Wait()

			close(done)
		}()

		select {
		case <-done:
		case <-time.After(stopTimeout):
			t.Errorf("node %d did not exit on SIGTERM; log:\n%s", n.id, n.log.text())
			_, _ = sudo(t, "kill", "-KILL", strconv.Itoa(n.pid))

			<-done
		}

		n.live = false
	}

	for _, n := range h.all() {
		for _, minor := range n.exports() {
			p := blockPath(minor)

			gone := false

			for i := 0; i < 300; i++ {
				if _, err := os.Stat(p); err != nil {
					gone = true

					break
				}

				time.Sleep(100 * time.Millisecond)
			}

			if !gone {
				t.Errorf("node %d leaked %s", n.id, p)
			}
		}
	}

	if !t.Failed() {
		_, _ = sudo(t, "rm", "-rf", h.root)
	}
}

// dev is an open handle on one exported block device. Every access is O_DIRECT against a
// page-aligned buffer, which is what a racer export requires, and every failure surfaces
// as the raw errno so a test can name EAGAIN, EINVAL or ENOSPC directly.
type dev struct {
	t    *testing.T
	path string
	f    *os.File
}

// open attaches to one of a node's exported devices, reusing the handle for the life of
// the process. Stopping the node closes every handle, so a restart opens fresh ones.
func (n *node) open(slot int) *dev {
	t := n.h.t
	t.Helper()

	if d, ok := n.devs[slot]; ok {
		return d
	}

	p := blockPath(deviceMinor(n.id, slot))

	f, err := os.OpenFile(p, os.O_RDWR|unix.O_DIRECT, 0)
	if errors.Is(err, os.ErrPermission) {
		// udev reapplies its own mode whenever it gets around to the device, which can
		// be after the export was published and relaxed. Relax it again and retry, so a
		// slow udev delays the open rather than failing the suite.
		if out, cerr := sudo(t, "chmod", "0666", p); cerr != nil {
			t.Fatalf("node %d: relaxing %s: %v\n%s", n.id, p, cerr, out)
		}

		f, err = os.OpenFile(p, os.O_RDWR|unix.O_DIRECT, 0)
	}

	if err != nil {
		t.Fatalf("node %d: opening %s: %v", n.id, p, err)
	}

	d := &dev{t: t, path: p, f: f}

	if n.devs == nil {
		n.devs = map[int]*dev{}
	}

	n.devs[slot] = d

	return d
}

// shut releases every handle this node's exports are open on, which a stop must do before
// the kernel can remove the minors.
func (n *node) shut() {
	for _, d := range n.devs {
		_ = d.f.Close()
	}

	n.devs = nil
}

// aligned returns a zeroed buffer whose first byte sits on a block boundary.
func aligned(n int) []byte {
	raw := make([]byte, n+blockSize)
	off := int(uintptr(unsafe.Pointer(&raw[0])) % blockSize)

	if off != 0 {
		off = blockSize - off
	}

	return raw[off : off+n]
}

// read pulls length bytes from the device.
func (d *dev) read(off int64, length int) ([]byte, error) {
	buf := aligned(length)

	n, err := unix.Pread(int(d.f.Fd()), buf, off)
	if err != nil {
		return nil, err
	}

	if n != length {
		return nil, fmt.Errorf("short read of %d at %d: got %d bytes", length, off, n)
	}

	out := make([]byte, length)
	copy(out, buf)

	return out, nil
}

// write pushes a buffer to the device.
func (d *dev) write(off int64, p []byte) error {
	buf := aligned(len(p))
	copy(buf, p)

	n, err := unix.Pwrite(int(d.f.Fd()), buf, off)
	if err != nil {
		return err
	}

	if n != len(p) {
		return fmt.Errorf("short write of %d at %d: put %d bytes", len(p), off, n)
	}

	return nil
}

// blkdiscard is BLKDISCARD, the ioctl a guest uses to free a page.
const blkdiscard = 0x1277

// discard frees a range, which is how an immutable page is released.
func (d *dev) discard(off, length int64) error {
	rng := [2]uint64{uint64(off), uint64(length)}

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, d.f.Fd(), blkdiscard, uintptr(unsafe.Pointer(&rng[0])))
	if errno != 0 {
		return errno
	}

	return nil
}

// pattern is a deterministic page-filling byte sequence keyed by a seed.
func pattern(seed byte, length int) []byte {
	out := make([]byte, length)
	for i := range out {
		out[i] = seed ^ byte(i*31)
	}

	return out
}

// zeros is a run of holes, which is what an unwritten page reads as.
func zeros(length int) []byte {
	return make([]byte, length)
}

// samples is one node's scrape, keyed by the full series name including its labels.
type samples map[string]uint64

// httpGet performs a bare GET and returns the status code and body.
func httpGet(addr, path string) (int, string, error) {
	c := &http.Client{Timeout: 5 * time.Second}

	resp, err := c.Get("http://" + addr + path)
	if err != nil {
		return 0, "", err
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}

	return resp.StatusCode, string(body), nil
}

// trySample scrapes without failing the test, for the polling helpers.
func (n *node) trySample() (samples, error) {
	if n.addr == "" {
		return nil, errors.New("no metrics address")
	}

	code, body, err := httpGet(n.addr, "/metrics")
	if err != nil {
		return nil, err
	}

	if code != http.StatusOK {
		return nil, fmt.Errorf("scrape returned %d", code)
	}

	return parseExposition(body)
}

// sample scrapes the node and fails the test if it cannot.
func (n *node) sample() samples {
	t := n.h.t
	t.Helper()

	s, err := n.trySample()
	if err != nil {
		t.Fatalf("node %d: scrape failed: %v", n.id, err)
	}

	return s
}

// parseExposition reads the Prometheus text format and checks it as it goes: every series
// must be introduced by HELP then TYPE, the type must be one racer publishes, and no
// series may appear twice.
func parseExposition(body string) (samples, error) {
	out := samples{}
	seen := map[string]bool{}
	typed := map[string]bool{}
	helped := map[string]bool{}

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, _, ok := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			if !ok || name == "" {
				return nil, fmt.Errorf("malformed HELP line %q", line)
			}

			helped[name] = true
		case strings.HasPrefix(line, "# TYPE "):
			name, kind, ok := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			if !ok {
				return nil, fmt.Errorf("malformed TYPE line %q", line)
			}

			if !helped[name] {
				return nil, fmt.Errorf("TYPE for %q with no HELP before it", name)
			}

			if kind != "counter" && kind != "gauge" {
				return nil, fmt.Errorf("series %q has unexpected type %q", name, kind)
			}

			typed[name] = true
		case strings.HasPrefix(line, "#"):
			return nil, fmt.Errorf("unexpected comment %q", line)
		default:
			series, raw, ok := strings.Cut(line, " ")
			if !ok {
				return nil, fmt.Errorf("malformed sample %q", line)
			}

			name := series
			if i := strings.IndexByte(series, '{'); i >= 0 {
				name = series[:i]
			}

			if !typed[name] {
				return nil, fmt.Errorf("sample for %q with no TYPE before it", name)
			}

			if seen[series] {
				return nil, fmt.Errorf("series %q appears twice", series)
			}

			seen[series] = true

			v, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("series %q has non-integer value %q", series, raw)
			}

			out[series] = v
		}
	}

	if len(out) == 0 {
		return nil, errors.New("empty exposition")
	}

	return out, nil
}

// diagnose traces every non-zero consensus, gateway, cache and heal counter each live
// node is publishing. A failed step usually says only that some page came back with an
// errno; this says which subsystem produced it and on which node, which is the difference
// between reading the trace and re-running the suite with print statements.
func (h *harness) diagnose() {
	prefixes := []string{
		"racer_paxos_",
		"racer_gateway_",
		"racer_warm_",
		"racer_cache_",
		"racer_heal_",
		"racer_alloc_",
		"racer_config_",
		"racer_topology_",
	}

	settle()

	for _, n := range h.live() {
		s, err := n.trySample()
		if err != nil {
			trace("diag n%d: cannot scrape: %v", n.id, err)
			continue
		}

		keys := make([]string, 0, len(s))

		for k, v := range s {
			if v == 0 {
				continue
			}

			for _, p := range prefixes {
				if strings.HasPrefix(k, p) {
					keys = append(keys, fmt.Sprintf("%s=%d", k, v))
					break
				}
			}
		}

		sort.Strings(keys)
		trace("diag n%d zone %d: %s", n.id, n.zone, strings.Join(keys, " "))
	}
}

// sum adds a series across every live node.
func (h *harness) sum(series string) uint64 {
	h.t.Helper()

	var total uint64

	for _, n := range h.live() {
		total += n.sample()[series]
	}

	return total
}

// sumZone adds a series across the live nodes of one zone.
func (h *harness) sumZone(z uint32, series string) uint64 {
	h.t.Helper()

	var total uint64

	for _, n := range h.zone(z) {
		if n.live {
			total += n.sample()[series]
		}
	}

	return total
}

// settle waits out one metrics publishing interval, because core 0 alone writes the
// node-wide values and a scrape lags work by up to one tick.
func settle() {
	time.Sleep(400 * time.Millisecond)
}

// awaitSum polls until a series summed over the live nodes reaches want, and reports what
// it saw if it never does.
func (h *harness) awaitSum(series string, want uint64, d time.Duration) uint64 {
	h.t.Helper()

	started := time.Now()
	deadline := started.Add(d)
	next := started.Add(2 * time.Second)

	for {
		got := h.sum(series)
		if got >= want {
			return got
		}

		now := time.Now()
		if now.After(deadline) {
			h.t.Fatalf("%s reached only %d of %d within %s", series, got, want, d)
		}

		if now.After(next) {
			trace("waiting: %s is %d, want %d (%.0fs of %s)", series, got, want, now.Sub(started).Seconds(), d)

			next = now.Add(2 * time.Second)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// awaitSumZone is awaitSum restricted to one fault domain.
func (h *harness) awaitSumZone(z uint32, series string, want uint64, d time.Duration) uint64 {
	h.t.Helper()

	started := time.Now()
	deadline := started.Add(d)
	next := started.Add(2 * time.Second)

	for {
		got := h.sumZone(z, series)
		if got >= want {
			return got
		}

		now := time.Now()
		if now.After(deadline) {
			h.t.Fatalf("zone %d: %s reached only %d of %d within %s", z, series, got, want, d)
		}

		if now.After(next) {
			trace("waiting: zone %d %s is %d, want %d (%.0fs of %s)", z, series, got, want, now.Sub(started).Seconds(), d)

			next = now.Add(2 * time.Second)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// errnoOf unwraps whatever a device access returned into a bare errno.
func errnoOf(err error) unix.Errno {
	var errno unix.Errno
	if errors.As(err, &errno) {
		return errno
	}

	return 0
}
