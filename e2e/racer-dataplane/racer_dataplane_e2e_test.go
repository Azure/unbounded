//go:build e2e

package racerdpe2e

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	racerconfig "github.com/Azure/unbounded/api/racer"
)

// Block budgets. Racer state is cumulative within a run: an immutable block may be written
// once and freed once, and an OCC observation is recorded per node, so each step owns a
// disjoint slice of every extent rather than sharing block zero.
const (
	pageHoles    = 0
	pageWrite    = 10
	pageCross    = 20
	pageCache    = 30
	pageWarm     = 40
	pageOCC      = 50
	pageImm      = 60
	pageOrder    = 80
	pageCrash    = 90
	pageGateway  = 100
	pageReloaded = 130
)

// step is one named phase of the single ordered walk over the cluster.
type step struct {
	name string
	run  func(t *testing.T, h *harness)
}

// TestRacerDataplane boots six racer processes as two consensus groups across two zones of
// one universe and walks the whole dataplane contract over them. The steps are ordered on
// purpose: they share one cluster, and later steps rely on state earlier ones left behind.
func TestRacerDataplane(t *testing.T) {
	h := newHarness(t)
	h.boot()

	steps := []step{
		{"boot", stepBoot},
		{"holes", stepHoles},
		{"write", stepWrite},
		{"write-cross-zone", stepWriteCrossZone},
		{"cache-policy", stepCachePolicy},
		{"warm-push", stepWarmPush},
		{"occ", stepOCC},
		{"immutable", stepImmutable},
		{"immutable-stripes", stepImmutableStripes},
		{"device-ordering", stepDeviceOrdering},
		{"durability", stepDurability},
		{"gateway-failover", stepGatewayFailover},
		{"reload", stepReload},
		{"shutdown", stepShutdown},
	}

	for _, s := range steps {
		trace("step %s: begin", s.name)

		started := time.Now()

		ok := t.Run(s.name, func(t *testing.T) { s.run(t, h) })

		trace("step %s: %s in %.1fs", s.name, map[bool]string{true: "passed", false: "FAILED"}[ok], time.Since(started).Seconds())

		if !ok {
			h.diagnose()
			t.Fatalf("step %q failed; the remaining steps depend on it", s.name)
		}
	}
}

// pageOff is the byte offset of a block of an extent on a given device slot.
func pageOff(slot int, extent uint32, page int) int64 {
	return offsetOf(slot, extent) + int64(page)*blockSize
}

// pageLen is the size of one block of an extent.
func pageLen(extent uint32) int {
	return blockSize
}

// put writes one whole page of an extent from a node and insists it lands.
func put(t *testing.T, n *node, slot int, extent uint32, page int, seed byte) {
	t.Helper()

	if err := putErr(n, slot, extent, page, seed); err != nil {
		t.Fatalf("node %d: writing extent %d page %d: %v", n.id, extent, page, err)
	}
}

// putErr writes one whole page and hands back whatever the device said.
func putErr(n *node, slot int, extent uint32, page int, seed byte) error {
	return n.open(slot).write(pageOff(slot, extent, page), pattern(seed, pageLen(extent)))
}

// occRetries bounds how many observations a contended operation is allowed to burn.
const occRetries = 16

// rw rewrites one whole mutable block. The guard on an OCC write is the writer's
// own prior read, so the page is observed first, and a write that some other node overtook
// in between is retried against a fresh observation rather than reported.
func rw(t *testing.T, n *node, slot int, extent uint32, page int, seed byte) {
	t.Helper()

	for i := 0; ; i++ {
		_, err := getErr(n, slot, extent, page)
		if err == nil {
			err = putErr(n, slot, extent, page, seed)
			if err == nil {
				return
			}
		}

		// A read that lands mid repair reports the same conflict a write does, so both
		// legs are retried against a fresh observation rather than reported.
		if errnoOf(err) != unix.EAGAIN || i == occRetries {
			t.Fatalf("node %d: rewriting extent %d page %d: %v", n.id, extent, page, err)
		}

		time.Sleep(pollInterval)
	}
}

// fill writes one whole page of an immutable extent for the first time.
//
// Write once is a guarded round like any other, so a proposal can lose its ballot to a
// concurrent repair or heal sweep and come back EAGAIN without anything having been
// written. That is indistinguishable at the device from the refusal a genuine second write
// earns, so the page itself is asked: while it still reads as a hole nothing was chosen and
// the write is offered again.
func fill(t *testing.T, n *node, slot int, extent uint32, page int, seed byte) {
	t.Helper()

	want := pattern(seed, pageLen(extent))

	for i := 0; ; i++ {
		err := putErr(n, slot, extent, page, seed)
		if err == nil {
			return
		}

		if errnoOf(err) != unix.EAGAIN || i == occRetries {
			t.Fatalf("node %d: filling extent %d page %d: %v", n.id, extent, page, err)
		}

		time.Sleep(pollInterval)

		got, err := getErr(n, slot, extent, page)
		if err != nil {
			t.Fatalf("node %d: reading back extent %d page %d: %v", n.id, extent, page, err)
		}

		if bytes.Equal(got, want) {
			return
		}

		if !bytes.Equal(got, zeros(len(got))) {
			t.Fatalf("node %d: extent %d page %d was filled by somebody else", n.id, extent, page)
		}
	}
}

// get reads one whole page and requires it to be served.
//
// A read that lands while a repair round is in flight reports the same conflict a losing
// write does, which says nothing about the page and everything about the timing, so it is
// asked again. Every test that means to observe a refusal calls getErr instead.
func get(t *testing.T, n *node, slot int, extent uint32, page int) []byte {
	t.Helper()

	for i := 0; ; i++ {
		got, err := getErr(n, slot, extent, page)
		if err == nil {
			return got
		}

		if errnoOf(err) != unix.EAGAIN || i == occRetries {
			t.Fatalf("node %d: reading extent %d page %d: %v", n.id, extent, page, err)
		}

		time.Sleep(pollInterval)
	}
}

// getErr reads one whole page and hands back whatever the device said.
func getErr(n *node, slot int, extent uint32, page int) ([]byte, error) {
	return n.open(slot).read(pageOff(slot, extent, page), pageLen(extent))
}

// same insists two buffers match, naming the first byte that differs.
func same(t *testing.T, what string, got, want []byte) {
	t.Helper()

	if bytes.Equal(got, want) {
		return
	}

	if len(got) != len(want) {
		t.Fatalf("%s: got %d bytes, want %d", what, len(got), len(want))
	}

	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: byte %d is 0x%02x, want 0x%02x", what, i, got[i], want[i])
		}
	}
}

// wantErrno insists an operation failed with exactly the errno the contract names.
func wantErrno(t *testing.T, what string, err error, want unix.Errno) {
	t.Helper()

	if err == nil {
		t.Fatalf("%s: succeeded, want %v", what, want)
	}

	if got := errnoOf(err); got != want {
		t.Fatalf("%s: failed with %v, want %v", what, err, want)
	}
}

// stepBoot proves every process came up whole and agrees on the topology it was handed.
func stepBoot(t *testing.T, h *harness) {
	for _, n := range h.all() {
		s := n.sample()

		for series, want := range map[string]uint64{
			"racer_config_generation":               h.gen,
			"racer_node_id":                         uint64(n.id),
			"racer_universes":                       1,
			"racer_devices":                         uint64(len(devices)),
			"racer_extents":                         uint64(len(extents)),
			"racer_peers":                           uint64(len(nodeIDs) - 1),
			"racer_topology_epoch":                  epoch,
			"racer_control_broadcast_stalled_total": 0,
			"racer_config_rejected_total":           0,
		} {
			if got, ok := s[series]; !ok {
				t.Errorf("node %d: %s is not published", n.id, series)
			} else if got != want {
				t.Errorf("node %d: %s is %d, want %d", n.id, series, got, want)
			}
		}

		if s[`racer_alloc_slots{class="mutable"}`] == 0 {
			t.Errorf("node %d: carved no 4 KiB slots out of its store", n.id)
		}

		if s["racer_workers"] == 0 {
			t.Errorf("node %d: reports no workers", n.id)
		}
	}

	// Zone B homes immutable extents, so its nodes must have carved immutable slabs.
	for _, n := range h.zone(zoneB) {
		if n.sample()[`racer_alloc_slots{class="immutable"}`] == 0 {
			t.Errorf("node %d homes immutable extents but carved no immutable slots", n.id)
		}
	}
}

// stepHoles proves an address that nobody has written reads as zeroes from every node,
// whether it is homed here or across the gateway.
func stepHoles(t *testing.T, h *harness) {
	for _, n := range h.all() {
		for _, d := range devices {
			for _, id := range d.extents {
				got := get(t, n, d.slot, id, pageHoles)
				same(t, fmt.Sprintf("node %d slot %d extent %d hole", n.id, d.slot, id), got, zeros(pageLen(id)))
			}
		}
	}
}

// stepWrite proves the plain read-write path inside the zone that homes the extent: a page
// reads back the way it was written, from every member of its group, and a later writer
// that observes the page first replaces it.
func stepWrite(t *testing.T, h *harness) {
	one, two, three := h.node(1), h.node(2), h.node(3)

	rw(t, one, slotPair, extRWA, pageWrite, 0x11)

	for _, n := range h.zone(zoneA) {
		same(t, fmt.Sprintf("node %d sees the first write", n.id), get(t, n, slotPair, extRWA, pageWrite), pattern(0x11, blockSize))
	}

	rw(t, two, slotPair, extRWA, pageWrite, 0x22)

	for _, n := range h.zone(zoneA) {
		same(t, fmt.Sprintf("node %d sees the second write", n.id), get(t, n, slotPair, extRWA, pageWrite), pattern(0x22, blockSize))
	}

	// A run of distinct pages, so the group is exercised beyond a single address.
	for i := 1; i <= 8; i++ {
		rw(t, three, slotPair, extRWA, pageWrite+i, byte(0x40+i))
	}

	for i := 1; i <= 8; i++ {
		same(t, fmt.Sprintf("page %d read back", pageWrite+i), get(t, one, slotPair, extRWA, pageWrite+i), pattern(byte(0x40+i), blockSize))
	}

	if h.sum(`racer_paxos_accept_total{result="ok"}`) == 0 {
		t.Fatalf("no guarded accept was counted anywhere")
	}
}

// stepWriteCrossZone proves the gateway path: a node of the far zone is not a member of the
// group that owns the page, yet its write lands and every member sees it.
func stepWriteCrossZone(t *testing.T, h *harness) {
	settle()

	before := h.sumZone(zoneA, `racer_paxos_accept_total{result="ok"}`)
	unavailable := h.sum(`racer_gateway_fallback_total{reason="unavailable"}`)

	// Zone B drives an extent homed in zone A.
	for i, n := range h.zone(zoneB) {
		rw(t, n, slotPair, extRWA, pageCross+i, byte(0x50+i))
	}

	for i := range h.zone(zoneB) {
		for _, n := range h.zone(zoneA) {
			same(t, fmt.Sprintf("node %d sees the far write to page %d", n.id, pageCross+i),
				get(t, n, slotPair, extRWA, pageCross+i), pattern(byte(0x50+i), blockSize))
		}
	}

	// Zone A drives an extent homed in zone B.
	for i, n := range h.zone(zoneA) {
		rw(t, n, slotPair, extRWB, pageCross+i, byte(0x60+i))
	}

	for i := range h.zone(zoneA) {
		for _, n := range h.zone(zoneB) {
			same(t, fmt.Sprintf("node %d sees the far write to page %d", n.id, pageCross+i),
				get(t, n, slotPair, extRWB, pageCross+i), pattern(byte(0x60+i), blockSize))
		}
	}

	settle()

	if got := h.sumZone(zoneA, `racer_paxos_accept_total{result="ok"}`); got <= before {
		t.Fatalf("zone A accepted nothing for the writes forwarded to it: %d then %d", before, got)
	}

	if got := h.sum(`racer_gateway_fallback_total{reason="unavailable"}`); got != unavailable {
		t.Fatalf("a gateway ring ran out while every node was up: %d then %d", unavailable, got)
	}
}

// stepCachePolicy proves the admission policy rather than the cache's contents. Demand
// admission (cache_admit plus the frequency sketch) only engages for an immutable block
// read by a node that sits in its home zone without being one of its group's three members:
// the cache client is gated on the reader holding no seat in the group, and a cross-zone
// read short circuits into the gateway path before that gate is ever reached, where the
// only page served locally is one whose extent names our zone among its warm zones. Every
// node in this cluster is a member of its zone's one group, so no demand cache client
// exists anywhere in it. What that leaves provable here is the policy itself: mutable
// blocks are refused and counted, and none is admitted. Proactive push
// caching, the one path that fills a cache across a zone boundary, is the next step.
func stepCachePolicy(t *testing.T, h *harness) {
	const admits = `racer_cache_admit_total`

	const rejects = `racer_cache_reject_total{reason="policy"}`

	owner, far := h.node(1), h.node(4)

	rw(t, owner, slotPair, extRWA, pageCache, 0x71)

	settle()

	admitBase := h.sum(admits)
	rejectBase := h.sum(rejects)

	hammer := func(n *node, slot int, extent uint32, page, rounds int) {
		t.Helper()

		for i := 0; i < rounds; i++ {
			if _, err := getErr(n, slot, extent, page); err != nil {
				t.Fatalf("node %d: reading extent %d page %d: %v", n.id, extent, page, err)
			}
		}
	}

	// A mutable extent is refused however hot it gets. The refusal is
	// counted on the member that computes the width rather than on the node that would
	// have held the page, so the whole cluster is summed.
	deadline := time.Now().Add(metricTimeout)

	for {
		hammer(far, slotOCC, extOCCA, pageCache, 64)
		settle()

		if h.sum(rejects) > rejectBase {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("an extent that opted out of caching was never counted as refused: %d", rejectBase)
		}
	}

	// Another mutable extent is equally ineligible, however often it is read.
	hammer(far, slotPair, extRWA, pageCache, 256)
	settle()

	if got := h.sum(admits); got != admitBase {
		t.Fatalf("a page was admitted with no cache client in the cluster: %d then %d", admitBase, got)
	}

	// With nothing cached anywhere, the far reader is answered by the owning group every
	// time, so a rewrite in the home zone is visible to it at once.
	same(t, "the far reader sees the page", get(t, far, slotPair, extRWA, pageCache), pattern(0x71, blockSize))

	rw(t, owner, slotPair, extRWA, pageCache, 0x72)

	same(t, "the far reader sees the rewrite", get(t, far, slotPair, extRWA, pageCache), pattern(0x72, blockSize))
}

// stepWarmPush proves proactive caching. Committing an immutable page of an extent that
// names a warm zone pushes it there without anything on the write path waiting, and a
// reader in that zone then finds it already local. An identical extent that names no warm
// zone is the control.
func stepWarmPush(t *testing.T, h *harness) {
	const sent = `racer_warm_total{result="sent"}`

	const taken = `racer_warm_total{result="taken"}`

	const hits = `racer_cache_lookup_total{result="hit"}`

	const admits = `racer_cache_admit_total`

	settle()

	sentBase := h.sumZone(zoneA, sent)
	takenBase := h.sumZone(zoneB, taken)

	for i := 0; i < 4; i++ {
		fill(t, h.node(1), slotImm, extImmWarmA, pageWarm+i, byte(0x80+i))
	}

	h.awaitSumZone(zoneA, sent, sentBase+1, metricTimeout)
	h.awaitSumZone(zoneB, taken, takenBase+1, metricTimeout)

	settle()

	hitBase := h.sumZone(zoneB, hits)

	for i := 0; i < 4; i++ {
		for _, n := range h.zone(zoneB) {
			same(t, fmt.Sprintf("node %d reads the warmed page", n.id),
				get(t, n, slotImm, extImmWarmA, pageWarm+i), pattern(byte(0x80+i), blockSize))
		}
	}

	h.awaitSumZone(zoneB, hits, hitBase+1, metricTimeout)

	// The control: same kind, same zone, no warm_zones and no admission. Nothing is
	// pushed, nothing is admitted in the far zone, and a reader there is answered by the
	// owning group rather than out of a cache it was never given anything to fill.
	settle()

	sentBase = h.sumZone(zoneA, sent)
	hitBase = h.sumZone(zoneB, hits)
	admitBase := h.sumZone(zoneB, admits)

	fill(t, h.node(1), slotImm, extImmPlain, pageWarm, 0x90)

	settle()
	settle()

	if got := h.sumZone(zoneA, sent); got != sentBase {
		t.Fatalf("an extent naming no warm zone was pushed anyway: %d then %d", sentBase, got)
	}

	if got := h.sumZone(zoneB, admits); got != admitBase {
		t.Fatalf("an extent naming no warm zone was admitted anyway: %d then %d", admitBase, got)
	}

	for i := 0; i < 8; i++ {
		same(t, "the unwarmed page still reads correctly across the gateway",
			get(t, h.node(4), slotImm, extImmPlain, pageWarm), pattern(0x90, blockSize))
	}

	settle()

	if got := h.sumZone(zoneB, hits); got != hitBase {
		t.Fatalf("an unwarmed page was served from cache: %d then %d", hitBase, got)
	}

	// And the push works in the other direction too.
	settle()

	sentBase = h.sumZone(zoneB, sent)
	takenBase = h.sumZone(zoneA, taken)

	for i := 0; i < 4; i++ {
		fill(t, h.node(4), slotImm, extImmWarmB, pageWarm+i, byte(0xa0+i))
	}

	h.awaitSumZone(zoneB, sent, sentBase+1, metricTimeout)
	h.awaitSumZone(zoneA, taken, takenBase+1, metricTimeout)

	for i := 0; i < 4; i++ {
		for _, n := range h.zone(zoneA) {
			same(t, fmt.Sprintf("node %d reads the page warmed into its zone", n.id),
				get(t, n, slotImm, extImmWarmB, pageWarm+i), pattern(byte(0xa0+i), blockSize))
		}
	}
}

// stepOCC proves optimistic concurrency. The guard on an OCC write is the writer's own
// prior read, so a write with nothing observed is refused, and a writer whose observation
// has been overtaken is refused too until it looks again.
func stepOCC(t *testing.T, h *harness) {
	const conflicts = "racer_paxos_guard_conflicts_total"

	one, two := h.node(1), h.node(2)

	settle()

	base := h.sum(conflicts)

	// No prior read on this node, so there is nothing to guard on.
	wantErrno(t, "an OCC write with no observation", putErr(one, slotOCC, extOCCA, pageOCC, 0xb1), unix.EAGAIN)

	// A hole is an observation of version zero, which is what a first write guards on.
	same(t, "the OCC page starts empty", get(t, one, slotOCC, extOCCA, pageOCC), zeros(blockSize))
	put(t, one, slotOCC, extOCCA, pageOCC, 0xb1)
	same(t, "the OCC write landed", get(t, one, slotOCC, extOCCA, pageOCC), pattern(0xb1, blockSize))

	// Two readers race. Both observe the same version; the first to write wins and the
	// second is refused until it re-reads.
	page := pageOCC + 1

	same(t, "node 1 observes the page", get(t, one, slotOCC, extOCCA, page), zeros(blockSize))
	same(t, "node 2 observes the page", get(t, two, slotOCC, extOCCA, page), zeros(blockSize))

	put(t, one, slotOCC, extOCCA, page, 0xb2)

	wantErrno(t, "the loser's write on a stale observation", putErr(two, slotOCC, extOCCA, page, 0xb3), unix.EAGAIN)

	same(t, "the loser re-reads the winner's bytes", get(t, two, slotOCC, extOCCA, page), pattern(0xb2, blockSize))
	put(t, two, slotOCC, extOCCA, page, 0xb3)
	same(t, "the loser's second attempt lands", get(t, one, slotOCC, extOCCA, page), pattern(0xb3, blockSize))

	// An observation that has been overtaken twice is no better than one overtaken once.
	page = pageOCC + 2

	same(t, "node 2 observes a fresh page", get(t, two, slotOCC, extOCCA, page), zeros(blockSize))

	same(t, "node 1 observes it too", get(t, one, slotOCC, extOCCA, page), zeros(blockSize))
	put(t, one, slotOCC, extOCCA, page, 0xb4)
	same(t, "node 1 observes its own write", get(t, one, slotOCC, extOCCA, page), pattern(0xb4, blockSize))
	put(t, one, slotOCC, extOCCA, page, 0xb5)

	wantErrno(t, "a write two versions behind", putErr(two, slotOCC, extOCCA, page, 0xb6), unix.EAGAIN)

	// The same rules apply when the page is homed in the other zone: a cross-zone read
	// records the observation locally, which is what makes the far write possible at all.
	page = pageOCC + 3

	wantErrno(t, "a cross-zone OCC write with no observation", putErr(one, slotOCC, extOCCB, page, 0xc1), unix.EAGAIN)

	same(t, "the far OCC page starts empty", get(t, one, slotOCC, extOCCB, page), zeros(blockSize))
	put(t, one, slotOCC, extOCCB, page, 0xc1)

	for _, n := range h.zone(zoneB) {
		same(t, fmt.Sprintf("node %d sees the far OCC write", n.id), get(t, n, slotOCC, extOCCB, page), pattern(0xc1, blockSize))
	}

	// Every member of zone B has now observed the page, so a write from one of them
	// conflicts with the one that overtook it.
	put(t, h.node(4), slotOCC, extOCCB, page, 0xc2)
	wantErrno(t, "a far member on a stale observation", putErr(h.node(5), slotOCC, extOCCB, page, 0xc3), unix.EAGAIN)

	settle()

	if got := h.sum(conflicts); got <= base {
		t.Fatalf("no guard conflict was counted: %d then %d", base, got)
	}
}

// stepImmutable proves write-once and free-once. An immutable page takes one write, is
// readable from every node, refuses a second write, and once discarded reads as a hole
// that cannot be refilled however often it is asked for.
func stepImmutable(t *testing.T, h *harness) {
	one := h.node(1)

	fill(t, one, slotImm, extImmPlain, pageImm, 0xd1)

	for _, n := range h.all() {
		same(t, fmt.Sprintf("node %d reads the immutable page", n.id),
			get(t, n, slotImm, extImmPlain, pageImm), pattern(0xd1, blockSize))
	}

	wantErrno(t, "rewriting an immutable page", putErr(one, slotImm, extImmPlain, pageImm, 0xd2), unix.EAGAIN)

	same(t, "the refused rewrite changed nothing", get(t, one, slotImm, extImmPlain, pageImm), pattern(0xd1, blockSize))

	off := pageOff(slotImm, extImmPlain, pageImm)
	if err := one.open(slotImm).discard(off, blockSize); err != nil {
		t.Fatalf("freeing an immutable page: %v", err)
	}

	for _, n := range h.all() {
		same(t, fmt.Sprintf("node %d reads the freed page as a hole", n.id),
			get(t, n, slotImm, extImmPlain, pageImm), zeros(blockSize))
	}

	// Freeing it again is not an error. An acceptor that already holds the tombstone
	// reports the trim applied rather than conflicting, because a fan-out that retried a
	// trim it had already delivered would otherwise fail on its own success. Free-once is
	// a statement about the page, and the page is gone: it cannot be refilled.
	if err := one.open(slotImm).discard(off, blockSize); err != nil {
		t.Fatalf("freeing an immutable page twice: %v", err)
	}

	wantErrno(t, "refilling a freed immutable page", putErr(one, slotImm, extImmPlain, pageImm, 0xd3), unix.EAGAIN)

	same(t, "the refused refill changed nothing", get(t, one, slotImm, extImmPlain, pageImm), zeros(blockSize))

	// The same rules hold when the write came across the gateway.
	page := pageImm + 1

	fill(t, h.node(4), slotImm, extImmPlain, page, 0xd4)
	wantErrno(t, "rewriting an immutable page from the far zone", putErr(h.node(5), slotImm, extImmPlain, page, 0xd5), unix.EAGAIN)

	for _, n := range h.all() {
		same(t, fmt.Sprintf("node %d reads the far-written immutable page", n.id),
			get(t, n, slotImm, extImmPlain, page), pattern(0xd4, blockSize))
	}
}

// stepImmutableStripes proves that immutable placement groups blocks by absolute 4 MiB
// stripe without changing their 4 KiB addressability or write-once state. Two blocks at
// opposite ends of one stripe and one in the next hold independent bytes, and discarding
// one leaves its stripe sibling untouched.
func stepImmutableStripes(t *testing.T, h *harness) {
	far, owner := h.node(1), h.node(4)

	for _, block := range []int{0, 1023} {
		fill(t, owner, slotStripe, extStripe, block, byte(0xe1+block%2))
	}
	fill(t, far, slotStripe, extStripe, 1024, 0xe5)

	for _, n := range h.all() {
		for block, seed := range map[int]byte{0: 0xe1, 1023: 0xe2, 1024: 0xe5} {
			same(t, fmt.Sprintf("node %d reads immutable stripe block %d", n.id, block),
				get(t, n, slotStripe, extStripe, block), pattern(seed, blockSize))
		}
	}

	wantErrno(t, "rewriting one immutable stripe block",
		putErr(owner, slotStripe, extStripe, 0, 0xe4), unix.EAGAIN)

	if err := owner.open(slotStripe).discard(pageOff(slotStripe, extStripe, 0), blockSize); err != nil {
		t.Fatalf("discarding one immutable stripe block: %v", err)
	}

	same(t, "the discarded stripe block reads as a hole",
		get(t, owner, slotStripe, extStripe, 0), zeros(blockSize))
	same(t, "the stripe sibling survives the discard",
		get(t, owner, slotStripe, extStripe, 1023), pattern(0xe2, blockSize))
}

// stepDeviceOrdering proves that a device is an ordered list of whole extents and nothing
// more: two hosts may concatenate the same extents the other way round and still address
// the same bytes.
func stepDeviceOrdering(t *testing.T, h *harness) {
	one, three, four := h.node(1), h.node(3), h.node(4)

	rw(t, one, slotPair, extRWA, pageOrder, 0xf1)
	rw(t, four, slotPair, extRWB, pageOrder, 0xf2)

	same(t, "the reversed device finds the zone A extent",
		get(t, three, slotReverse, extRWA, pageOrder), pattern(0xf1, blockSize))
	same(t, "the reversed device finds the zone B extent",
		get(t, three, slotReverse, extRWB, pageOrder), pattern(0xf2, blockSize))

	// And a write through the reversed device is seen through the forward one.
	rw(t, three, slotReverse, extRWA, pageOrder+1, 0xf3)
	same(t, "the forward device sees the reversed device's write",
		get(t, one, slotPair, extRWA, pageOrder+1), pattern(0xf3, blockSize))

	// The two devices really are laid out differently.
	if offsetOf(slotPair, extRWA) == offsetOf(slotReverse, extRWA) {
		t.Fatalf("the reversed device is not actually reversed")
	}
}

// stepDurability proves that losing a member loses nothing. A node is killed outright, the
// survivors keep serving and keep accepting, and the returning node is replayed by
// anti-entropy rather than serving what it missed.
func stepDurability(t *testing.T, h *harness) {
	const repairs = `racer_heal_repair_total{result="ok"}`

	one, two, three, four := h.node(1), h.node(2), h.node(3), h.node(4)

	rw(t, one, slotPair, extRWA, pageCrash, 0x21)
	fill(t, one, slotImm, extImmPlain, pageCrash, 0x22)

	settle()

	repairBase := h.sum(repairs)

	three.stop(false)

	// Everything acknowledged before the crash is still there, from the survivors and
	// from across the gateway.
	same(t, "a survivor still has the rewritable page", get(t, two, slotPair, extRWA, pageCrash), pattern(0x21, blockSize))
	same(t, "a survivor still has the immutable page", get(t, two, slotImm, extImmPlain, pageCrash), pattern(0x22, blockSize))
	same(t, "the far zone still reads the rewritable page", get(t, four, slotPair, extRWA, pageCrash), pattern(0x21, blockSize))

	// Two of three is a quorum, so the group keeps accepting while the third is gone.
	for i := 1; i <= 16; i++ {
		rw(t, two, slotPair, extRWA, pageCrash+i, byte(0x30+i))
	}

	fill(t, one, slotImm, extImmPlain, pageCrash+1, 0x2a)

	for i := 1; i <= 16; i++ {
		same(t, fmt.Sprintf("page %d written without the third member", pageCrash+i),
			get(t, one, slotPair, extRWA, pageCrash+i), pattern(byte(0x30+i), blockSize))
	}

	// The survivors have to let go of the dead node's fabric export before it can be
	// published again. See harness.detach for why that is a property of the host and not
	// of racer.
	h.detach(three.id)

	three.restart()

	h.attach()

	// The returning member catches up from the two that stayed.
	h.awaitSum(repairs, repairBase+1, healTimeout)

	for i := 0; i <= 16; i++ {
		want := pattern(byte(0x30+i), blockSize)
		if i == 0 {
			want = pattern(0x21, blockSize)
		}

		same(t, fmt.Sprintf("the replayed member has page %d", pageCrash+i),
			get(t, three, slotPair, extRWA, pageCrash+i), want)
	}

	same(t, "the replayed member has the immutable page", get(t, three, slotImm, extImmPlain, pageCrash), pattern(0x22, blockSize))
}

// stepGatewayFailover proves the gateway ring. With one gateway of the far zone no longer
// answering, a cross-zone read falls through to the next gateway in the ring and still
// returns the page.
//
// Only the read half is driven with a gateway down, and that is a property of this host
// rather than of racer. Every node here reaches its peers by opening the block device those
// peers export on this one machine, so a peer that exits leaves an export of zero capacity
// behind under five open file descriptors. The kernel answers a read past the end of a
// device with a short read, which is the transport failure the ring is written to retry,
// but it answers a write past the end with ENOSPC, which is a report about storage and is
// passed to the guest untouched. A real deployment reaches a peer across its own NVMe-oF
// namespace, which dies with the target and fails every command alike. The pages are
// therefore written while the ring is whole and only read once it is short a gateway.
func stepGatewayFailover(t *testing.T, h *harness) {
	const (
		retries = `racer_gateway_fallback_total{reason="retry"}`
		// Enough distinct addresses that the rendezvous ring puts the gateway that is
		// about to go away first for at least one of them.
		pages = 16
	)

	one, six := h.node(1), h.node(6)

	for i := 0; i < pages; i++ {
		fill(t, one, slotStripe, extStripe, pageGateway+i, byte(0x10+i))
	}

	settle()

	retryBase := h.sumZone(zoneA, retries)

	six.stop(false)

	for i := 0; i < pages; i++ {
		same(t, fmt.Sprintf("page %d survived the missing gateway", pageGateway+i),
			get(t, one, slotStripe, extStripe, pageGateway+i), pattern(byte(0x10+i), blockSize))
	}

	h.awaitSumZone(zoneA, retries, retryBase+1, metricTimeout)

	h.detach(six.id)
	six.restart()
	h.attach()

	// The zone is whole again and every page is still there, including on the node that
	// was away while they were read.
	for i := 0; i < pages; i++ {
		same(t, fmt.Sprintf("page %d after the gateway returned", pageGateway+i),
			get(t, six, slotStripe, extStripe, pageGateway+i), pattern(byte(0x10+i), blockSize))
	}
}

// stepReload proves the configuration contract: a newer generation is picked up from the
// file the control plane renames into place, an older one is ignored, one the node cannot
// honour is refused and counted, and the metrics endpoint answers nothing it was not asked
// for.
func stepReload(t *testing.T, h *harness) {
	// An immutable extent can opt into caching on a live reload, with no restart anywhere.
	h.reconfigure(func(_ *node, cfg *racerconfig.NodeConfig) {
		for _, e := range cfg.Universes[0].Extents {
			if e.Id == extImmPlain {
				e.CacheAdmit = 1
			}
		}
	})

	owner := h.node(1)

	// A generation at or below the one in force is not an error, it is a redelivery of a
	// file the node already moved past, so it is ignored in silence and nothing moves.
	refusedBase := owner.sample()["racer_config_rejected_total"]

	owner.write(h.config(owner, h.gen-1, true))
	settle()

	if s := owner.sample(); s["racer_config_generation"] != h.gen {
		t.Fatalf("a stale configuration moved the generation: %d", s["racer_config_generation"])
	}

	// A newer generation the node cannot honour is a different matter: it is refused
	// wholesale and counted, and the configuration in force is untouched. A store may grow
	// and never shrink, so halving it is a file no node will take.
	shrunk := h.config(owner, h.gen+1, true)
	shrunk.Node.Store.SizeBytes /= 2

	owner.write(shrunk)

	deadline := time.Now().Add(metricTimeout)

	for {
		s := owner.sample()
		if s["racer_config_rejected_total"] > refusedBase {
			if s["racer_config_generation"] != h.gen {
				t.Fatalf("a refused configuration still moved the generation: %d", s["racer_config_generation"])
			}

			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("an unacceptable configuration was not refused: %d", refusedBase)
		}

		time.Sleep(100 * time.Millisecond)
	}

	// Put the current generation back so nothing downstream reads a stale file.
	owner.write(h.config(owner, h.gen, true))

	// The endpoint serves metrics and nothing else.
	code, _, err := httpGet(owner.addr, "/nope")
	if err != nil {
		t.Fatalf("requesting an unknown path: %v", err)
	}

	if code != http.StatusNotFound {
		t.Fatalf("an unknown path returned %d, want %d", code, http.StatusNotFound)
	}
}

// stepShutdown proves every process goes down on the signal it is given and gives its ublk
// exports back to the kernel.
func stepShutdown(t *testing.T, h *harness) {
	for _, n := range h.all() {
		n.stop(true)
	}

	for _, n := range h.all() {
		for _, minor := range n.exports() {
			if _, err := os.Stat(blockPath(minor)); err == nil {
				t.Errorf("node %d left %s behind", n.id, blockPath(minor))
			}
		}
	}
}
