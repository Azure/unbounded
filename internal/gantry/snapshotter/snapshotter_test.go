// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package snapshotter

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	gdigest "github.com/Azure/unbounded/internal/gantry/digest"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// fakeCatalog is an in-memory stand-in for the cluster blob index.
type fakeCatalog struct {
	mu     sync.Mutex
	blobs  map[catalog.Digest]catalog.Blob
	chains map[catalog.Digest]catalog.Digest
	syncs  int
	// onSync publishes records the way another node's ingest would.
	onSync  func(c *fakeCatalog)
	syncErr error
}

func newCatalog() *fakeCatalog {
	return &fakeCatalog{
		blobs:  map[catalog.Digest]catalog.Blob{},
		chains: map[catalog.Digest]catalog.Digest{},
	}
}

func (c *fakeCatalog) publish(chainID, diffID catalog.Digest, addr segment.Address) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.blobs[diffID] = catalog.Blob{DiffID: diffID, Address: addr, Generation: 1}
	c.chains[chainID] = diffID
}

func (c *fakeCatalog) Resolve(chainID catalog.Digest) (catalog.Blob, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	diffID, ok := c.chains[chainID]
	if !ok {
		return catalog.Blob{}, false
	}

	b, ok := c.blobs[diffID]

	return b, ok
}

func (c *fakeCatalog) Blob(diffID catalog.Digest) (catalog.Blob, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	b, ok := c.blobs[diffID]

	return b, ok
}

func (c *fakeCatalog) Sync() (bool, error) {
	c.mu.Lock()
	c.syncs++
	hook := c.onSync
	err := c.syncErr
	c.mu.Unlock()

	if err != nil {
		return false, err
	}

	if hook != nil {
		hook(c)
	}

	return true, nil
}

func (c *fakeCatalog) syncCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.syncs
}

// fakeMapper records which layers were mapped and hands back a stable path.
type fakeMapper struct {
	mu       sync.Mutex
	root     string
	ensured  map[string]int
	pruned   []map[string]struct{}
	err      error
	pruneErr error
}

func newMapper(t *testing.T) *fakeMapper {
	t.Helper()

	return &fakeMapper{root: t.TempDir(), ensured: map[string]int{}}
}

func (m *fakeMapper) Name(layer catalog.Digest, addr segment.Address) string {
	return "gsnap-" + layer.Short() + addr.Fingerprint()
}

func (m *fakeMapper) Root() string { return m.root }

func (m *fakeMapper) Ensure(_ context.Context, layer catalog.Digest, addr segment.Address) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return "", m.err
	}

	name := m.Name(layer, addr)
	m.ensured[name]++

	return filepath.Join(m.root, name), nil
}

func (m *fakeMapper) Prune(_ context.Context, keep map[string]struct{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.pruneErr != nil {
		return m.pruneErr
	}

	m.pruned = append(m.pruned, keep)

	return nil
}

func (m *fakeMapper) prunes() []map[string]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.pruned
}

// fakeQueue records ingest submissions.
type fakeQueue struct {
	mu       sync.Mutex
	requests []ingest.Request
	accept   bool
}

func newQueue() *fakeQueue { return &fakeQueue{accept: true} }

func (q *fakeQueue) Submit(req ingest.Request) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.requests = append(q.requests, req)

	return q.accept
}

func (q *fakeQueue) all() []ingest.Request {
	q.mu.Lock()
	defer q.mu.Unlock()

	return append([]ingest.Request(nil), q.requests...)
}

type harness struct {
	sn  *Snapshotter
	cat *fakeCatalog
	m   *fakeMapper
	q   *fakeQueue
	ctx context.Context

	logs *logSink

	mu      sync.Mutex
	adopted []AdoptOutcome
}

// outcomes reports every adoption outcome the snapshotter has reported so far.
func (h *harness) outcomes() []AdoptOutcome {
	h.mu.Lock()
	defer h.mu.Unlock()

	return append([]AdoptOutcome(nil), h.adopted...)
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	h := &harness{cat: newCatalog(), m: newMapper(t), q: newQueue(), logs: &logSink{}}

	sn, err := New(Options{
		Root:         t.TempDir(),
		Catalog:      h.cat,
		Mapper:       h.m,
		Queue:        h.q,
		MountOptions: []string{"index=off"},
		Logger:       slog.New(slog.NewTextHandler(h.logs, nil)),
		Observe: func(outcome AdoptOutcome) {
			h.mu.Lock()
			defer h.mu.Unlock()

			h.adopted = append(h.adopted, outcome)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = sn.Close() })

	h.sn = sn
	h.ctx = namespaces.WithNamespace(t.Context(), "testing")

	return h
}

// logSink captures what the snapshotter logs. slog writes from whichever
// goroutine is logging, so the buffer needs its own lock.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.Write(p)
}

// count reports how many times substr appears in everything logged so far.
func (s *logSink) count(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return strings.Count(s.buf.String(), substr)
}

// digestOf builds a digest whose every byte is b, plus its string form.
func digestOf(b byte) catalog.Digest {
	var d catalog.Digest
	for i := range d {
		d[i] = b
	}

	return d
}

// digestAt builds a distinct digest for layer n. digestOf only has 256 values
// and a deep stack needs two digests per layer, so tag separates the chain IDs
// from the diff IDs and n varies the rest.
func digestAt(tag byte, n int) catalog.Digest {
	d := digestOf(tag)
	binary.BigEndian.PutUint32(d[1:5], uint32(n)) //nolint:gosec // n is a small test index.

	return d
}

func addrOf(page uint32) segment.Address {
	return segment.Address{Segment: 1, PageOffset: page, PageCount: 1, ByteLength: 1 << 20}
}

// layerDigestString is a compressed layer digest in the form the CRI label uses.
func layerDigestString(b byte) string { return digestOf(b).String() }

func TestNewValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		opts Options
	}{
		{"no root", Options{Catalog: newCatalog(), Mapper: &fakeMapper{}}},
		{"no catalog", Options{Root: t.TempDir(), Mapper: &fakeMapper{}}},
		{"no mapper", Options{Root: t.TempDir(), Catalog: newCatalog()}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if _, err := New(tc.opts); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestPrepareMissUnpacksLocally(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	chain := digestOf(1)

	mounts, err := h.sn.Prepare(h.ctx, "extract-0", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: chain.String(),
		LabelDiffID:      digestOf(2).String(),
	}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if len(mounts) != 1 || mounts[0].Type != "bind" {
		t.Fatalf("expected a single bind mount for a rootless layer, got %+v", mounts)
	}

	info, err := h.sn.Stat(h.ctx, "extract-0")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if info.Kind != snapshots.KindActive {
		t.Fatalf("kind = %v, want active", info.Kind)
	}

	if _, err := h.sn.Stat(h.ctx, chain.String()); err == nil {
		t.Fatal("the committed snapshot must not exist on a miss")
	}
}

func TestPrepareHitSkipsTheLayer(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain, diffID := digestOf(1), digestOf(2)
	h.cat.publish(chain, diffID, addrOf(0))

	_, err := h.sn.Prepare(h.ctx, "extract-0", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: chain.String(),
		LabelDiffID:      diffID.String(),
	}))
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("Prepare error = %v, want already exists", err)
	}

	// containerd only believes the layer is done if Stat then succeeds.
	info, err := h.sn.Stat(h.ctx, chain.String())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if info.Kind != snapshots.KindCommitted {
		t.Fatalf("kind = %v, want committed", info.Kind)
	}

	if got := info.Labels[LabelBlob]; got != diffID.String() {
		t.Fatalf("blob label = %q, want %q", got, diffID.String())
	}

	if _, ok := info.Labels[LabelSnapshotRef]; ok {
		t.Fatal("the ref label must not be persisted")
	}

	if _, err := h.sn.Stat(h.ctx, "extract-0"); err == nil {
		t.Fatal("the active key must be gone after adoption")
	}

	usage, err := h.sn.Usage(h.ctx, chain.String())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage.Size != int64(addrOf(0).ByteLength) {
		t.Fatalf("usage = %d, want %d", usage.Size, addrOf(0).ByteLength)
	}

	// The layer was mapped before the promise was made. Adopting a layer this
	// node cannot read would be unrecoverable, because containerd never
	// revisits a chain ID it has been told already exists.
	name := h.m.Name(diffID, addrOf(0))
	if h.m.ensured[name] != 1 {
		t.Fatalf("adoption mapped %q %d times, want 1", name, h.m.ensured[name])
	}
}

func TestPrepareRefusesToAdoptALayerItCannotMap(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain, diffID := digestOf(1), digestOf(2)
	h.cat.publish(chain, diffID, addrOf(0))

	// The catalog knows the blob, but this node cannot reach it: the segment
	// holding it is not exported here, or device mapper refuses the target.
	h.m.err = errors.New("segment 1 is not exported on this node")

	mounts, err := h.sn.Prepare(h.ctx, "extract-0", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: chain.String(),
		LabelDiffID:      diffID.String(),
	}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Falling back to the ordinary unpack path is slow but always works.
	if len(mounts) == 0 {
		t.Fatal("expected the ordinary unpack path")
	}

	if _, err := h.sn.Stat(h.ctx, chain.String()); err == nil {
		t.Fatal("an unmappable layer must not be adopted: the snapshot would be permanently unstartable")
	}

	if _, err := h.sn.Stat(h.ctx, "extract-0"); err != nil {
		t.Fatalf("the active snapshot must exist for containerd to unpack into: %v", err)
	}
}

// The hit rate is the number that says whether the snapshotter is doing
// anything at all, so the outcome the daemon counts has to be the one Prepare
// acted on. Classifying it separately from the switch would let the two drift.
func TestPrepareReportsAdoptionOutcomes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain, diffID := digestOf(1), digestOf(2)
	labels := snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: chain.String(),
		LabelDiffID:      diffID.String(),
	})

	// A miss: the catalog has never heard of this chain.
	if _, err := h.sn.Prepare(h.ctx, "extract-0", "", labels); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// A hit: another node published it, and this node adopts it.
	h.cat.publish(chain, diffID, addrOf(0))

	if _, err := h.sn.Prepare(h.ctx, "extract-1", "", labels); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("Prepare: %v, want AlreadyExists", err)
	}

	// The chain is now committed locally, so adopting it again collides.
	if _, err := h.sn.Prepare(h.ctx, "extract-2", "", labels); !errdefs.IsAlreadyExists(err) {
		t.Fatalf("Prepare: %v, want AlreadyExists", err)
	}

	// A failure: the layer is in the catalog but cannot be mapped here.
	other, otherDiff := digestOf(3), digestOf(4)
	h.cat.publish(other, otherDiff, addrOf(1))
	h.m.err = errors.New("segment 1 is not exported on this node")

	if _, err := h.sn.Prepare(h.ctx, "extract-3", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: other.String(),
		LabelDiffID:      otherDiff.String(),
	})); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	want := []AdoptOutcome{AdoptMiss, AdoptHit, AdoptExists, AdoptFailed}
	if got := h.outcomes(); !slices.Equal(got, want) {
		t.Fatalf("outcomes = %v, want %v", got, want)
	}
}

// Prepare only consults the observer when it tried to adopt. A ref that cannot
// be a chain ID, or no ref at all, is containerd doing something else.
func TestPrepareReportsNothingWhenItDoesNotTryToAdopt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if _, err := h.sn.Prepare(h.ctx, "extract-0", ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if got := h.outcomes(); len(got) != 0 {
		t.Fatalf("outcomes = %v, want none", got)
	}
}

func TestAdoptOutcomeString(t *testing.T) {
	t.Parallel()

	cases := map[AdoptOutcome]string{
		AdoptMiss:        "miss",
		AdoptHit:         "hit",
		AdoptExists:      "exists",
		AdoptFailed:      "failed",
		AdoptOutcome(99): "unknown(99)",
	}

	for outcome, want := range cases {
		if got := outcome.String(); got != want {
			t.Errorf("AdoptOutcome(%d).String() = %q, want %q", int(outcome), got, want)
		}
	}
}

func TestPrepareHitOnANonSHA256Ref(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	mounts, err := h.sn.Prepare(h.ctx, "extract-0", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: "not-a-digest",
	}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if len(mounts) == 0 {
		t.Fatal("expected the ordinary unpack path")
	}

	if h.cat.syncCount() != 0 {
		t.Fatal("a ref that cannot be a chain ID must not touch the catalog")
	}

	// Still a miss, not a failure: a ref that cannot be a chain ID is one the
	// cluster could never have, and there is nothing to alert on.
	if got := h.outcomes(); !slices.Equal(got, []AdoptOutcome{AdoptMiss}) {
		t.Fatalf("outcomes = %v, want one miss", got)
	}
}

func TestPrepareResyncsOnAMiss(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain, diffID := digestOf(1), digestOf(2)

	// Another node publishes the layer between this node's last sync and now.
	h.cat.onSync = func(c *fakeCatalog) {
		c.blobs[diffID] = catalog.Blob{DiffID: diffID, Address: addrOf(0), Generation: 1}
		c.chains[chain] = diffID
		c.onSync = nil
	}

	_, err := h.sn.Prepare(h.ctx, "extract-0", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: chain.String(),
	}))
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("Prepare error = %v, want already exists after resync", err)
	}

	if h.cat.syncCount() != 1 {
		t.Fatalf("syncs = %d, want 1", h.cat.syncCount())
	}
}

func TestMissSyncIsRateLimited(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	for i := range 5 {
		chain := digestOf(byte(i + 10))

		_, err := h.sn.Prepare(h.ctx, fmt.Sprintf("extract-%d", i), "", snapshots.WithLabels(map[string]string{
			LabelSnapshotRef: chain.String(),
		}))
		if err != nil {
			t.Fatalf("Prepare %d: %v", i, err)
		}
	}

	if got := h.cat.syncCount(); got != 1 {
		t.Fatalf("syncs = %d, want 1 across a burst of misses", got)
	}
}

func TestSyncFailureIsNotFatal(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.cat.syncErr = errors.New("catalog unreadable")

	_, err := h.sn.Prepare(h.ctx, "extract-0", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: digestOf(1).String(),
	}))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
}

// adopt publishes a layer and drives the Prepare that adopts it.
func (h *harness) adopt(t *testing.T, key, parent string, chain, diffID catalog.Digest, page uint32) {
	t.Helper()

	h.cat.publish(chain, diffID, addrOf(page))

	_, err := h.sn.Prepare(h.ctx, key, parent, snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: chain.String(),
		LabelDiffID:      diffID.String(),
	}))
	if !errdefs.IsAlreadyExists(err) {
		t.Fatalf("adopt %s: error = %v, want already exists", chain.Short(), err)
	}
}

func TestMountsStackClusterLayers(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain0, diff0 := digestOf(1), digestOf(0x11)
	chain1, diff1 := digestOf(2), digestOf(0x22)

	h.adopt(t, "extract-0", "", chain0, diff0, 0)
	h.adopt(t, "extract-1", chain0.String(), chain1, diff1, 1)

	mounts, err := h.sn.Prepare(h.ctx, "container-1", chain1.String())
	if err != nil {
		t.Fatalf("Prepare container: %v", err)
	}

	if len(mounts) != 1 || mounts[0].Type != "overlay" {
		t.Fatalf("expected one overlay mount, got %+v", mounts)
	}

	lower := optionValue(t, mounts[0].Options, "lowerdir=")
	dirs := strings.Split(lower, ":")

	if len(dirs) != 2 {
		t.Fatalf("lowerdir has %d entries, want 2: %q", len(dirs), lower)
	}

	// Nearest parent first, which for overlayfs means the topmost layer.
	if want := filepath.Join(h.m.root, h.m.Name(diff1, addrOf(1))); dirs[0] != want {
		t.Fatalf("lowerdir[0] = %q, want %q", dirs[0], want)
	}

	if want := filepath.Join(h.m.root, h.m.Name(diff0, addrOf(0))); dirs[1] != want {
		t.Fatalf("lowerdir[1] = %q, want %q", dirs[1], want)
	}

	if !hasOption(mounts[0].Options, "upperdir=") || !hasOption(mounts[0].Options, "workdir=") {
		t.Fatalf("an active snapshot needs a local upper and work dir: %v", mounts[0].Options)
	}

	if !hasOption(mounts[0].Options, "index=off") {
		t.Fatalf("configured mount options were dropped: %v", mounts[0].Options)
	}

	// Mounts must return the same thing without re-mapping anything.
	again, err := h.sn.Mounts(h.ctx, "container-1")
	if err != nil {
		t.Fatalf("Mounts: %v", err)
	}

	if again[0].Options[len(again[0].Options)-1] != mounts[0].Options[len(mounts[0].Options)-1] {
		t.Fatal("Mounts disagreed with Prepare")
	}

	// Ensure is idempotent, so the count only records how often it was asked:
	// once when the layer was adopted, then once per assembled mount.
	for name, n := range h.m.ensured {
		if n != 3 {
			t.Fatalf("layer %s mapped %d times, want 3 (adopt, Prepare, Mounts)", name, n)
		}
	}
}

func TestCommonRoot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a    string
		b    string
		want string
	}{
		{"same dir", "/var/lib/gs", "/var/lib/gs", "/var/lib/gs"},
		{"sibling", "/var/lib/gs", "/var/lib/gs/l", "/var/lib/gs"},
		{"shared parent", "/var/lib/gs/snapshots", "/var/lib/gs/l", "/var/lib/gs"},
		{"only the root", "/var/lib/gs", "/run/gs/l", ""},
		{"trailing slash", "/var/lib/gs/", "/var/lib/gs/l/", "/var/lib/gs"},
		{"relative", "var/lib/gs", "/var/lib/gs/l", ""},
		{"partial element", "/var/lib/gsnap", "/var/lib/gs", "/var/lib"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := commonRoot(tc.a, tc.b); got != tc.want {
				t.Fatalf("commonRoot(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCheckOptionsCountsWhatTheKernelSees(t *testing.T) {
	t.Parallel()

	// One option of exactly the limit is the largest thing that fits, because
	// checkOptions charges a comma for every option after the first.
	s := &Snapshotter{}

	if err := s.checkOptions([]string{strings.Repeat("a", mountDataLimit)}, 1); err != nil {
		t.Fatalf("an option at the limit was refused: %v", err)
	}

	if err := s.checkOptions([]string{strings.Repeat("a", mountDataLimit+1)}, 1); err == nil {
		t.Fatal("an option past the limit was accepted")
	}

	// Two options whose lengths sum to exactly the limit only overflow it
	// because of the comma between them.
	head := strings.Repeat("a", mountDataLimit-1000)
	tail := strings.Repeat("b", 1000)

	if err := s.checkOptions([]string{head, tail}, 1); err == nil {
		t.Fatal("checkOptions did not charge for the separating comma")
	}
}

func TestCheckOptionsCreditsTheSharedPrefix(t *testing.T) {
	t.Parallel()

	// Every lowerdir under a common root loses that root plus its slash once
	// containerd chdirs, so a stack that overflows without the credit fits
	// with it.
	const root = "/var/lib/gantry-snapshotter"

	layers := 8
	dirs := make([]string, 0, layers)

	for i := range layers {
		dirs = append(dirs, fmt.Sprintf("%s/l/%s", root, strings.Repeat("x", 500)+fmt.Sprint(i)))
	}

	options := []string{"lowerdir=" + strings.Join(dirs, ":")}

	bare := &Snapshotter{}
	if err := bare.checkOptions(options, layers); err == nil {
		t.Fatal("a stack with no shared root should not fit in one page")
	}

	shared := &Snapshotter{lowerRoot: root}
	if err := shared.checkOptions(options, layers); err != nil {
		t.Fatalf("the shared root was not credited: %v", err)
	}

	// A single lowerdir is never compacted, so it gets no credit.
	if err := shared.checkOptions(options, 1); err == nil {
		t.Fatal("a lone lowerdir was credited a compaction that never happens")
	}
}

func TestMountsRefusesAStackTooDeepForOnePage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if h.sn.lowerRoot == "" {
		t.Fatalf("harness roots share no parent: %q and %q", h.sn.root, h.m.root)
	}

	// Each extra layer costs its mapped path, a colon, and the shared root it
	// gives back. Overshoot so the refusal cannot be an off-by-one.
	per := len(h.m.root) + 1 + len(h.m.Name(digestOf(0), addrOf(0))) + 1 - (len(h.sn.lowerRoot) + 1)
	if per <= 0 {
		t.Fatalf("a mapped layer costs %d bytes, the test cannot overflow", per)
	}

	layers := mountDataLimit/per + 8

	parent := ""

	for i := range layers {
		chain, diff := digestAt(1, i), digestAt(0x11, i)
		h.adopt(t, fmt.Sprintf("extract-%d", i), parent, chain, diff, uint32(i)) //nolint:gosec // i is bounded by layers.
		parent = chain.String()
	}

	// A shallower stack still works, so the refusal is about depth and not
	// about the chain being broken.
	if _, err := h.sn.Prepare(h.ctx, "shallow", digestAt(1, 3).String()); err != nil {
		t.Fatalf("a four layer stack was refused: %v", err)
	}

	_, err := h.sn.Prepare(h.ctx, "container", parent)
	if err == nil {
		t.Fatalf("a %d layer stack was accepted", layers)
	}

	if !errdefs.IsFailedPrecondition(err) {
		t.Fatalf("Prepare failed with %v, want a failed precondition", err)
	}

	// The refusal must not leave the key behind, or the pod can never retry.
	if _, err := h.sn.Stat(h.ctx, "container"); err == nil {
		t.Fatal("a refused Prepare left its snapshot in the metastore")
	}
}

func TestMountsMixLocalAndClusterLayers(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain0, diff0 := digestOf(1), digestOf(0x11)
	h.adopt(t, "extract-0", "", chain0, diff0, 0)

	// The second layer misses, so it is unpacked locally.
	chain1 := digestOf(2)

	if _, err := h.sn.Prepare(h.ctx, "extract-1", chain0.String(), snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: chain1.String(),
		LabelDiffID:      digestOf(0x22).String(),
	})); err != nil {
		t.Fatalf("Prepare miss: %v", err)
	}

	if err := h.sn.Commit(h.ctx, chain1.String(), "extract-1"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	mounts, err := h.sn.Prepare(h.ctx, "container-1", chain1.String())
	if err != nil {
		t.Fatalf("Prepare container: %v", err)
	}

	dirs := strings.Split(optionValue(t, mounts[0].Options, "lowerdir="), ":")
	if len(dirs) != 2 {
		t.Fatalf("lowerdir has %d entries, want 2", len(dirs))
	}

	if !strings.HasPrefix(dirs[0], h.sn.root) {
		t.Fatalf("the locally unpacked layer should be a local directory, got %q", dirs[0])
	}

	if !strings.HasPrefix(dirs[1], h.m.root) {
		t.Fatalf("the adopted layer should be a mapping, got %q", dirs[1])
	}
}

func TestViewOfASingleClusterLayer(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain0, diff0 := digestOf(1), digestOf(0x11)
	h.adopt(t, "extract-0", "", chain0, diff0, 3)

	mounts, err := h.sn.View(h.ctx, "view-0", chain0.String())
	if err != nil {
		t.Fatalf("View: %v", err)
	}

	if len(mounts) != 1 || mounts[0].Type != "bind" {
		t.Fatalf("expected a bind mount, got %+v", mounts)
	}

	if want := filepath.Join(h.m.root, h.m.Name(diff0, addrOf(3))); mounts[0].Source != want {
		t.Fatalf("source = %q, want %q", mounts[0].Source, want)
	}

	if !hasOption(mounts[0].Options, "ro") {
		t.Fatalf("a view must be read only: %v", mounts[0].Options)
	}
}

func TestMountsFailWhenABlobVanishes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain0, diff0 := digestOf(1), digestOf(0x11)
	h.adopt(t, "extract-0", "", chain0, diff0, 0)

	h.cat.mu.Lock()
	delete(h.cat.blobs, diff0)
	h.cat.mu.Unlock()

	_, err := h.sn.Prepare(h.ctx, "container-1", chain0.String())
	if !errdefs.IsNotFound(err) {
		t.Fatalf("error = %v, want not found", err)
	}
}

func TestMountsFailWhenMappingFails(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain0, diff0 := digestOf(1), digestOf(0x11)
	h.adopt(t, "extract-0", "", chain0, diff0, 0)

	h.m.err = errors.New("dmsetup exploded")

	if _, err := h.sn.Prepare(h.ctx, "container-1", chain0.String()); err == nil {
		t.Fatal("expected the mapping failure to surface")
	}

	// The snapshot must not be left behind, so the retry can use the same key.
	if _, err := h.sn.Stat(h.ctx, "container-1"); err == nil {
		t.Fatal("the failed snapshot should have been rolled back")
	}
}

func TestCommitQueuesIngest(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain, diffID := digestOf(1), digestOf(2)
	layer := layerDigestString(3)

	if _, err := h.sn.Prepare(h.ctx, "extract-0", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: chain.String(),
		LabelDiffID:      diffID.String(),
		LabelLayerDigest: layer,
	})); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if err := h.sn.Commit(h.ctx, chain.String(), "extract-0"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	reqs := h.q.all()
	if len(reqs) != 1 {
		t.Fatalf("submitted %d requests, want 1", len(reqs))
	}

	if reqs[0].DiffID != diffID || reqs[0].ChainID != chain {
		t.Fatalf("request = %+v", reqs[0])
	}

	if reqs[0].Layer.String() != layer {
		t.Fatalf("layer = %q, want %q", reqs[0].Layer.String(), layer)
	}
}

func TestCommitSkipsIngestWithoutAnnotations(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	chain := digestOf(1)

	if _, err := h.sn.Prepare(h.ctx, "extract-0", "", snapshots.WithLabels(map[string]string{
		LabelSnapshotRef: chain.String(),
		LabelDiffID:      digestOf(2).String(),
	})); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if err := h.sn.Commit(h.ctx, chain.String(), "extract-0"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if got := len(h.q.all()); got != 0 {
		t.Fatalf("submitted %d requests, want 0 without a layer digest", got)
	}
}

// A node whose containerd is not passing layer annotations publishes nothing,
// forever, while looking perfectly healthy. That has to be said out loud, and
// it has to be said once rather than once per layer of every image.
func TestCommitWarnsOnceWhenAnnotationsAreOff(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	for i := range 3 {
		chain, key := digestAt(1, i), fmt.Sprintf("extract-%d", i)

		if _, err := h.sn.Prepare(h.ctx, key, "", snapshots.WithLabels(map[string]string{
			LabelSnapshotRef: chain.String(),
			LabelDiffID:      digestAt(2, i).String(),
		})); err != nil {
			t.Fatalf("Prepare: %v", err)
		}

		if err := h.sn.Commit(h.ctx, chain.String(), key); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	if got := h.logs.count(LabelLayerDigest); got != 1 {
		t.Fatalf("warned about the missing annotation %d times, want exactly 1", got)
	}

	if got := h.logs.count("disable_snapshot_annotations"); got != 1 {
		t.Fatalf("named the containerd setting %d times, want exactly 1", got)
	}
}

func TestCommitOfAContainerLayerIsNotIngested(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if _, err := h.sn.Prepare(h.ctx, "container-1", ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if err := h.sn.Commit(h.ctx, "committed-container", "container-1"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if got := len(h.q.all()); got != 0 {
		t.Fatalf("submitted %d requests, want 0", got)
	}

	// A container's writable layer is not an image layer, so it is not
	// evidence of anything being misconfigured.
	if got := h.logs.count(LabelLayerDigest); got != 0 {
		t.Fatalf("warned %d times about an ordinary container layer", got)
	}
}

func TestCommitRecordsLocalUsage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if _, err := h.sn.Prepare(h.ctx, "container-1", ""); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	id := h.idOf(t, "container-1")

	if err := os.WriteFile(filepath.Join(h.sn.upperPath(id), "payload"), make([]byte, 8192), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := h.sn.Commit(h.ctx, "committed", "container-1"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	usage, err := h.sn.Usage(h.ctx, "committed")
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage.Size < 8192 {
		t.Fatalf("usage = %d, want at least 8192", usage.Size)
	}

	if usage.Inodes < 2 {
		t.Fatalf("inodes = %d, want at least 2", usage.Inodes)
	}
}

func TestUpdateProtectsTheBlobLabel(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain, diffID := digestOf(1), digestOf(2)
	h.adopt(t, "extract-0", "", chain, diffID, 0)

	cases := []struct {
		name       string
		labels     map[string]string
		fieldpaths []string
	}{
		{"repoint", map[string]string{LabelBlob: digestOf(9).String()}, []string{"labels"}},
		{"drop via labels", map[string]string{}, []string{"labels"}},
		{"drop via full update", map[string]string{}, nil},
		{"repoint one label", map[string]string{LabelBlob: digestOf(9).String()}, []string{"labels." + LabelBlob}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.sn.Update(h.ctx, snapshots.Info{Name: chain.String(), Labels: tc.labels}, tc.fieldpaths...)
			if !errdefs.IsInvalidArgument(err) {
				t.Fatalf("error = %v, want invalid argument", err)
			}
		})
	}

	// An unrelated label still updates, and the blob label survives.
	updated, err := h.sn.Update(h.ctx, snapshots.Info{
		Name:   chain.String(),
		Labels: map[string]string{LabelBlob: diffID.String(), "team": "gantry"},
	}, "labels")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if updated.Labels["team"] != "gantry" {
		t.Fatalf("labels = %v", updated.Labels)
	}

	if updated.Labels[LabelBlob] != diffID.String() {
		t.Fatalf("blob label lost: %v", updated.Labels)
	}
}

func TestUpdateIgnoresUnrelatedFieldpaths(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain, diffID := digestOf(1), digestOf(2)
	h.adopt(t, "extract-0", "", chain, diffID, 0)

	if _, err := h.sn.Update(h.ctx, snapshots.Info{Name: chain.String()}, "labels.team"); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestRemoveAndCleanup(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain, diffID := digestOf(1), digestOf(2)
	h.adopt(t, "extract-0", "", chain, diffID, 0)

	if _, err := h.sn.Prepare(h.ctx, "container-1", chain.String()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	dir := filepath.Join(h.sn.snapshotDir(), h.idOf(t, "container-1"))
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("snapshot dir: %v", err)
	}

	if err := h.sn.Remove(h.ctx, "container-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if err := h.sn.Cleanup(h.ctx); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("orphan directory survived cleanup: %v", err)
	}

	prunes := h.m.prunes()
	if len(prunes) != 1 {
		t.Fatalf("prunes = %d, want 1", len(prunes))
	}

	// The adopted layer is still referenced, so its mapping must be kept.
	if _, ok := prunes[0][h.m.Name(diffID, addrOf(0))]; !ok {
		t.Fatalf("keep set %v is missing the live layer", prunes[0])
	}
}

func TestCleanupSkipsPruningWhenALayerIsUnresolved(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain, diffID := digestOf(1), digestOf(2)
	h.adopt(t, "extract-0", "", chain, diffID, 0)

	h.cat.mu.Lock()
	delete(h.cat.blobs, diffID)
	h.cat.mu.Unlock()

	if err := h.sn.Cleanup(h.ctx); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if got := len(h.m.prunes()); got != 0 {
		t.Fatalf("pruned %d times, want 0 while a layer is unresolved", got)
	}
}

func TestWalk(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain, diffID := digestOf(1), digestOf(2)
	h.adopt(t, "extract-0", "", chain, diffID, 0)

	var names []string

	if err := h.sn.Walk(h.ctx, func(_ context.Context, i snapshots.Info) error {
		names = append(names, i.Name)

		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(names) != 1 || names[0] != chain.String() {
		t.Fatalf("walked %v", names)
	}
}

func TestIngestRequest(t *testing.T) {
	t.Parallel()

	chain, diffID, layer := digestOf(1), digestOf(2), digestOf(3)

	cases := []struct {
		name   string
		key    string
		labels map[string]string
		want   ingestReason
	}{
		{
			name: "complete",
			key:  chain.String(),
			labels: map[string]string{
				LabelDiffID:      diffID.String(),
				LabelLayerDigest: layer.String(),
			},
			want: reasonIngest,
		},
		{
			name:   "container layer",
			key:    "committed-container",
			labels: map[string]string{LabelDiffID: diffID.String(), LabelLayerDigest: layer.String()},
			want:   reasonSkip,
		},
		{
			name:   "no diff id",
			key:    chain.String(),
			labels: map[string]string{LabelLayerDigest: layer.String()},
			want:   reasonSkip,
		},
		{
			name:   "no layer digest",
			key:    chain.String(),
			labels: map[string]string{LabelDiffID: diffID.String()},
			want:   reasonNoAnnotations,
		},
		{
			name:   "no labels",
			key:    chain.String(),
			labels: nil,
			want:   reasonSkip,
		},
		{
			name:   "bad diff id",
			key:    chain.String(),
			labels: map[string]string{LabelDiffID: "sha512:beef", LabelLayerDigest: layer.String()},
			want:   reasonSkip,
		},
		{
			name:   "unreadable layer digest",
			key:    chain.String(),
			labels: map[string]string{LabelDiffID: diffID.String(), LabelLayerDigest: "not-a-digest"},
			want:   reasonSkip,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req, reason := ingestRequest(tc.key, tc.labels)
			if reason != tc.want {
				t.Fatalf("reason = %v, want %v", reason, tc.want)
			}

			if reason != reasonIngest {
				return
			}

			if req.ChainID != chain || req.DiffID != diffID {
				t.Fatalf("request = %+v", req)
			}
		})
	}
}

func TestDiskUsage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "a"), make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := os.Link(filepath.Join(dir, "a"), filepath.Join(dir, "b")); err != nil {
		t.Fatalf("link: %v", err)
	}

	usage, err := diskUsage(t.Context(), dir)
	if err != nil {
		t.Fatalf("diskUsage: %v", err)
	}

	// The directory plus one file: the hard link must not be counted twice.
	if usage.Inodes != 2 {
		t.Fatalf("inodes = %d, want 2", usage.Inodes)
	}

	if usage.Size < 4096 {
		t.Fatalf("size = %d, want at least 4096", usage.Size)
	}
}

func TestDiskUsageOfAMissingDirectory(t *testing.T) {
	t.Parallel()

	usage, err := diskUsage(t.Context(), filepath.Join(t.TempDir(), "gone"))
	if err != nil {
		t.Fatalf("diskUsage: %v", err)
	}

	if usage != (snapshots.Usage{}) {
		t.Fatalf("usage = %+v, want zero", usage)
	}
}

func TestDiskUsageHonoursCancellation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for i := range 4 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(i)), []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := diskUsage(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
}

func TestCollectParentsRejectsABadLabel(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	chain, diffID := digestOf(1), digestOf(2)
	h.adopt(t, "extract-0", "", chain, diffID, 0)

	// Reach past Update's protection the way a corrupt database would.
	h.forceLabel(t, chain.String(), "sha256:nonsense")

	if _, err := h.sn.Prepare(h.ctx, "container-1", chain.String()); err == nil {
		t.Fatal("expected a bad blob label to be rejected")
	}
}

// helpers

func optionValue(t *testing.T, options []string, prefix string) string {
	t.Helper()

	for _, o := range options {
		if v, ok := strings.CutPrefix(o, prefix); ok {
			return v
		}
	}

	t.Fatalf("option %q not found in %v", prefix, options)

	return ""
}

func hasOption(options []string, prefix string) bool {
	for _, o := range options {
		if strings.HasPrefix(o, prefix) {
			return true
		}
	}

	return false
}

// idOf returns the numeric identifier that names a snapshot's directory.
func (h *harness) idOf(t *testing.T, key string) string {
	t.Helper()

	var id string

	err := h.sn.ms.WithTransaction(h.ctx, false, func(ctx context.Context) error {
		var err error

		id, _, _, err = storage.GetInfo(ctx, key)

		return err
	})
	if err != nil {
		t.Fatalf("idOf %s: %v", key, err)
	}

	return id
}

// forceLabel writes the blob label without going through Update, standing in
// for a database corrupted by something other than this snapshotter.
func (h *harness) forceLabel(t *testing.T, name, value string) {
	t.Helper()

	err := h.sn.ms.WithTransaction(h.ctx, true, func(ctx context.Context) error {
		_, err := storage.UpdateInfo(ctx, snapshots.Info{
			Name:   name,
			Labels: map[string]string{LabelBlob: value},
		}, "labels")

		return err
	})
	if err != nil {
		t.Fatalf("forceLabel: %v", err)
	}
}

// blobProvider is a content store holding gzip compressed layer tars.
type blobProvider struct {
	blobs map[string][]byte
	err   error
}

func (p *blobProvider) ReaderAt(_ context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	if p.err != nil {
		return nil, p.err
	}

	data, ok := p.blobs[desc.Digest.String()]
	if !ok {
		return nil, fmt.Errorf("blob %s: %w", desc.Digest, errdefs.ErrNotFound)
	}

	return &bytesReaderAt{Reader: bytes.NewReader(data)}, nil
}

type bytesReaderAt struct {
	*bytes.Reader
	closed bool
}

func (r *bytesReaderAt) Size() int64 { return r.Reader.Size() }

func (r *bytesReaderAt) Close() error {
	r.closed = true

	return nil
}

func TestContentOpenerDecompresses(t *testing.T) {
	t.Parallel()

	payload := []byte("this is a layer tar, honest")

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("gzip write: %v", err)
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	layer := digestOf(7)
	p := &blobProvider{blobs: map[string][]byte{layer.String(): buf.Bytes()}}

	o, err := NewContentOpener(p, "")
	if err != nil {
		t.Fatalf("NewContentOpener: %v", err)
	}

	rc, err := o.Open(t.Context(), ingest.Request{
		DiffID:  digestOf(1),
		ChainID: digestOf(2),
		Layer:   gdigest.MustParse(layer.String()),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if string(got) != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestContentOpenerPassesUncompressedLayers(t *testing.T) {
	t.Parallel()

	payload := []byte("already a tar")
	layer := digestOf(8)

	o, err := NewContentOpener(&blobProvider{blobs: map[string][]byte{layer.String(): payload}}, "custom")
	if err != nil {
		t.Fatalf("NewContentOpener: %v", err)
	}

	rc, err := o.Open(t.Context(), ingest.Request{
		DiffID:  digestOf(1),
		ChainID: digestOf(2),
		Layer:   gdigest.MustParse(layer.String()),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(got) != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestContentOpenerMissingBlob(t *testing.T) {
	t.Parallel()

	o, err := NewContentOpener(&blobProvider{blobs: map[string][]byte{}}, "")
	if err != nil {
		t.Fatalf("NewContentOpener: %v", err)
	}

	_, err = o.Open(t.Context(), ingest.Request{
		DiffID:  digestOf(1),
		ChainID: digestOf(2),
		Layer:   gdigest.MustParse(digestOf(9).String()),
	})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("error = %v, want not found", err)
	}
}

func TestContentOpenerRequiresAProvider(t *testing.T) {
	t.Parallel()

	if _, err := NewContentOpener(nil, ""); err == nil {
		t.Fatal("expected an error")
	}

	o, err := NewContentOpener(&blobProvider{}, "")
	if err != nil {
		t.Fatalf("NewContentOpener: %v", err)
	}

	if o.Namespace != DefaultNamespace {
		t.Fatalf("namespace = %q, want %q", o.Namespace, DefaultNamespace)
	}
}
