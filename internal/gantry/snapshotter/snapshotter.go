// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

// Package snapshotter implements the gantry-snapshotter: a containerd proxy
// snapshotter whose committed image layers live once per cluster in RACER and
// whose writable container layers live on local disk.
//
// The whole point of the snapshotter is what it does NOT do. When containerd
// asks to prepare a snapshot for a layer whose chain ID is already published in
// the cluster catalog, this snapshotter maps the layer onto the node, records
// the committed snapshot, and answers ErrAlreadyExists. containerd's unpacker
// treats that plus a successful Stat as "this layer is done", so the layer blob
// is never fetched from a registry and never unpacked. On a 40 layer image
// where every layer is already in the catalog, the node downloads zero layer
// bytes and runs zero applies; the only work left is mapping each layer's EROFS
// image out of a RACER segment and mounting it.
//
// A miss falls back to the ordinary path: an overlayfs active snapshot on local
// disk, into which containerd applies the layer tar exactly as the built in
// overlay snapshotter would. Commit then queues the layer for ingest so the
// next node in the cluster hits instead of missing. Ingest is deliberately off
// the container start path.
//
// Layers are addressed in two ways and it matters which is used where:
//
//   - chain ID identifies a position in a specific stack of layers. containerd
//     names committed snapshots by chain ID, so that is the key the catalog is
//     probed with in Prepare.
//   - diff ID identifies the uncompressed content of one layer regardless of
//     what is below it. That is what an EROFS image is built from, so that is
//     the key a blob is stored under. Two images that share a layer at
//     different depths share the blob and differ only by a chain record.
package snapshotter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/containerd/errdefs"

	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/ingest"
)

const (
	// LabelSnapshotRef is the label containerd's unpacker sets on Prepare to
	// name the committed snapshot it intends to create. Its presence is what
	// distinguishes an image layer from a container's writable layer, and its
	// value is the chain ID the catalog is probed with.
	//
	// containerd keeps this constant unexported, so it is restated here.
	LabelSnapshotRef = "containerd.io/snapshot.ref"

	// LabelDiffID is the uncompressed digest of the layer being prepared. It is
	// set by containerd's unpacker and is the key a blob is stored under.
	LabelDiffID = "containerd.io/snapshot/diff-id"

	// LabelLayerDigest is the compressed digest of the layer, added by the CRI
	// and transfer service annotation handlers. It is how ingest finds the layer
	// tar in containerd's content store, and it is the key the ingest election
	// hashes, so the node gantry already picked to pull a layer is usually the
	// node that ingests it.
	//
	// It is only present when containerd is configured with
	// disable_snapshot_annotations = false.
	LabelLayerDigest = "containerd.io/snapshot/cri.layer-digest"

	// LabelBlob marks a committed snapshot as backed by a RACER blob and holds
	// the diff ID to look up. Its absence means the snapshot is an ordinary
	// local directory.
	//
	// This lives in the snapshot metadata rather than in a file on disk because
	// mounting a snapshot has to resolve every ancestor, and walking the parent
	// chain in bbolt is a handful of in-memory bucket lookups inside a
	// transaction that is already open. The alternative, a marker file per
	// snapshot directory, costs a stat per layer per container start and can
	// drift from the database.
	LabelBlob = "gantry-snapshotter.unbounded-cloud.io/blob"
)

// maxParents bounds the parent chain walk. Image layer counts are in the tens;
// this only exists so a corrupt database cannot spin forever.
const maxParents = 1024

// DefaultMissSync is the shortest interval between two catalog reads triggered
// by a Prepare miss. A miss is worth re-reading the catalog for, because the
// common case is that another node ingested the layer seconds ago and this
// node's index is merely stale. Re-reading on every miss of a genuinely new
// image would add a round trip per layer for nothing, hence the floor.
const DefaultMissSync = 2 * time.Second

// Catalog is the part of the cluster blob index the snapshotter needs. It is
// satisfied by *catalog.Store.
type Catalog interface {
	// Resolve maps a chain ID to the blob that backs it.
	Resolve(chainID catalog.Digest) (catalog.Blob, bool)
	// Blob maps a diff ID to the blob that holds it.
	Blob(diffID catalog.Digest) (catalog.Blob, bool)
	// Sync re-reads records published by other nodes.
	Sync() (bool, error)
	// Generation is the catalog generation this node's index has applied.
	Generation() uint64
	// Watermark publishes how far this node has caught up. It is what the
	// cleaner waits on before it trims a segment.
	Watermark(generation uint64) error
}

// Mapper turns a blob address into a mounted read-only filesystem. It is
// satisfied by *blockmap.Map.
type Mapper interface {
	Ensure(ctx context.Context, layer catalog.Digest, blob catalog.Blob) (string, error)
	Name(layer catalog.Digest, blob catalog.Blob) string
	Root() string
	Prune(ctx context.Context, keep map[string]struct{}) error
}

// Submitter accepts layers for background ingest. It is satisfied by
// *ingest.Queue. Submit must not block.
type Submitter interface {
	Submit(req ingest.Request) bool
}

// AdoptOutcome says what a Prepare carrying a snapshot ref did with it.
//
// This is the one number that says whether the daemon is earning its keep: a
// hit is a layer neither downloaded nor unpacked on this node, and a miss is
// containerd doing exactly what it would have done without a snapshotter. The
// two failure outcomes are separated from a miss because they mean something
// different operationally: a miss is a cold cluster, a failure is a node that
// cannot reach layers other nodes can.
type AdoptOutcome int

const (
	// AdoptMiss means the cluster catalog does not have the chain ID.
	// containerd unpacks the layer and this node may ingest it afterwards.
	AdoptMiss AdoptOutcome = iota

	// AdoptHit means the layer was mapped from the cluster's storage and
	// the fetch and the unpack were both skipped.
	AdoptHit

	// AdoptExists means the chain ID was already committed here, usually
	// because a concurrent Prepare adopted it first. containerd gets the
	// same answer as a hit.
	AdoptExists

	// AdoptFailed means the catalog had the layer and this node could not
	// map it. The layer is unpacked locally instead, so it costs bandwidth
	// rather than availability.
	AdoptFailed
)

// String renders an outcome for logs and metric labels.
func (o AdoptOutcome) String() string {
	switch o {
	case AdoptMiss:
		return "miss"
	case AdoptHit:
		return "hit"
	case AdoptExists:
		return "exists"
	case AdoptFailed:
		return "failed"
	default:
		return fmt.Sprintf("unknown(%d)", int(o))
	}
}

// Options configures a Snapshotter.
type Options struct {
	// Root is the directory holding the metadata database and the local
	// writable layers.
	Root string
	// Catalog is the cluster blob index. Required.
	Catalog Catalog
	// Mapper maps blobs onto the node. Required.
	Mapper Mapper
	// Queue receives layers that missed the catalog. Optional; a nil queue
	// turns the snapshotter into a plain overlay snapshotter that reads
	// whatever the cluster has already published but never contributes.
	Queue Submitter
	// MountOptions are appended to every overlay mount. When nil a sensible
	// default is chosen for the running kernel.
	MountOptions []string
	// MissSync overrides DefaultMissSync.
	MissSync time.Duration
	// Observe reports the outcome of every Prepare containerd issues while
	// unpacking an image. Optional. It runs on the Prepare goroutine, so an
	// implementation that blocks blocks a container start.
	Observe func(AdoptOutcome)
	// Logger receives warnings from paths that must not fail the operation.
	Logger *slog.Logger
}

// Snapshotter is a containerd snapshotter backed by the cluster blob catalog.
type Snapshotter struct {
	root      string
	lowerRoot string
	ms        *storage.MetaStore
	cat       Catalog
	maps      Mapper
	queue     Submitter
	opts      []string
	log       *slog.Logger
	missGap   time.Duration
	observe   func(AdoptOutcome)

	syncMu   sync.Mutex
	lastSync time.Time

	// noAnnotations keeps a static containerd misconfiguration from being
	// reported once per layer.
	noAnnotations sync.Once
}

// New opens or creates a snapshotter rooted at opts.Root.
func New(opts Options) (*Snapshotter, error) {
	switch {
	case opts.Root == "":
		return nil, errors.New("snapshotter: root is required")
	case opts.Catalog == nil:
		return nil, errors.New("snapshotter: catalog is required")
	case opts.Mapper == nil:
		return nil, errors.New("snapshotter: mapper is required")
	}

	if err := os.MkdirAll(filepath.Join(opts.Root, "snapshots"), 0o700); err != nil {
		return nil, fmt.Errorf("snapshotter: create root: %w", err)
	}

	ms, err := storage.NewMetaStore(filepath.Join(opts.Root, "metadata.db"))
	if err != nil {
		return nil, fmt.Errorf("snapshotter: open metadata: %w", err)
	}

	mountOpts := opts.MountOptions
	if mountOpts == nil {
		mountOpts = defaultMountOptions()
	}

	gap := opts.MissSync
	if gap <= 0 {
		gap = DefaultMissSync
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Every lowerdir of every overlay this snapshotter builds is either a
	// local snapshot directory under Root or a layer mount under the mapper's
	// root. When those two share a parent, containerd can shorten a long
	// lowerdir list by chdir-ing to it; when they do not, it cannot shorten
	// anything and deep images run into the kernel's one page limit on mount
	// options. That is a deployment mistake, not a runtime condition, so say
	// so once at startup rather than at the first container start that trips
	// over it.
	lowerRoot := commonRoot(opts.Root, opts.Mapper.Root())
	if lowerRoot == "" {
		logger.Warn("gantry-snapshotter: layer mounts and local snapshots share no parent directory, deep images may exceed the mount option limit",
			"root", opts.Root, "map-root", opts.Mapper.Root())
	}

	return &Snapshotter{
		root:      opts.Root,
		lowerRoot: lowerRoot,
		ms:        ms,
		cat:       opts.Catalog,
		maps:      opts.Mapper,
		queue:     opts.Queue,
		opts:      mountOpts,
		log:       logger,
		missGap:   gap,
		observe:   opts.Observe,
	}, nil
}

// Stat returns the info for the named snapshot.
func (s *Snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	var info snapshots.Info

	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		var err error

		_, info, _, err = storage.GetInfo(ctx, key)

		return err
	})

	return info, err
}

// Update mutates the labels of an existing snapshot. The blob label is refused:
// it is what tells every later mount where the layer's bytes are, and rewriting
// it would silently repoint a committed snapshot at someone else's data.
func (s *Snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	var updated snapshots.Info

	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		_, current, _, err := storage.GetInfo(ctx, info.Name)
		if err != nil {
			return err
		}

		if err := checkBlobLabel(current, info, fieldpaths); err != nil {
			return err
		}

		updated, err = storage.UpdateInfo(ctx, info, fieldpaths...)

		return err
	})

	return updated, err
}

// checkBlobLabel rejects an update that would add, change, or drop the blob
// label.
func checkBlobLabel(current, next snapshots.Info, fieldpaths []string) error {
	const path = "labels." + LabelBlob

	touched := len(fieldpaths) == 0
	for _, f := range fieldpaths {
		if f == "labels" || f == path {
			touched = true
			break
		}
	}

	if !touched {
		return nil
	}

	if current.Labels[LabelBlob] == next.Labels[LabelBlob] {
		return nil
	}

	return fmt.Errorf("snapshot %q: %s is immutable: %w", next.Name, LabelBlob, errdefs.ErrInvalidArgument)
}

// Usage reports the space a snapshot occupies on this node. A RACER backed
// snapshot occupies nothing locally; the size recorded at adoption is the size
// of the shared blob, which is what containerd's image size accounting wants.
func (s *Snapshotter) Usage(ctx context.Context, key string) (snapshots.Usage, error) {
	var usage snapshots.Usage

	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		id, snInfo, u, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}

		usage = u

		if snInfo.Kind == snapshots.KindActive {
			du, err := diskUsage(ctx, s.upperPath(id))
			if err != nil {
				return err
			}

			usage = du
		}

		return nil
	})

	return usage, err
}

// Walk iterates over every snapshot matching the filters.
func (s *Snapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, fs ...string) error {
	return s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		return storage.WalkInfo(ctx, fn, fs...)
	})
}

// Close releases the metadata database.
func (s *Snapshotter) Close() error {
	return s.ms.Close()
}

func (s *Snapshotter) snapshotDir() string {
	return filepath.Join(s.root, "snapshots")
}

func (s *Snapshotter) upperPath(id string) string {
	return filepath.Join(s.root, "snapshots", id, "fs")
}

func (s *Snapshotter) workPath(id string) string {
	return filepath.Join(s.root, "snapshots", id, "work")
}

// Prepare creates an active snapshot. When containerd is unpacking an image
// layer it passes LabelSnapshotRef naming the committed snapshot it wants; if
// the cluster already has that chain ID and this node can map it, the committed
// snapshot is created here and ErrAlreadyExists is returned so containerd skips
// the layer entirely.
func (s *Snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	var base snapshots.Info
	for _, opt := range opts {
		if err := opt(&base); err != nil {
			return nil, err
		}
	}

	if target := base.Labels[LabelSnapshotRef]; target != "" {
		adopted, err := s.adopt(ctx, key, parent, target, base)
		s.report(adoptOutcome(adopted, err))

		switch {
		case adopted:
			return nil, fmt.Errorf("snapshot %q: %w", target, errdefs.ErrAlreadyExists)
		case errors.Is(err, errdefs.ErrAlreadyExists):
			// Another goroutine committed the same chain ID first. That is the
			// answer containerd is looking for either way.
			return nil, fmt.Errorf("snapshot %q: %w", target, errdefs.ErrAlreadyExists)
		case err != nil:
			// Adoption is an optimisation. If it fails for any other reason,
			// fall through and let containerd unpack the layer normally.
			s.log.Warn("gantry-snapshotter: adopting cluster layer failed, falling back to unpack",
				"chain", target, "error", err)
		}
	}

	return s.createSnapshot(ctx, snapshots.KindActive, key, parent, opts)
}

// adoptOutcome classifies what adopt did, using the same ordering Prepare
// does so the two cannot disagree about what happened.
func adoptOutcome(adopted bool, err error) AdoptOutcome {
	switch {
	case adopted:
		return AdoptHit
	case errors.Is(err, errdefs.ErrAlreadyExists):
		return AdoptExists
	case err != nil:
		return AdoptFailed
	default:
		return AdoptMiss
	}
}

// report hands an outcome to the configured observer, which is optional.
func (s *Snapshotter) report(outcome AdoptOutcome) {
	if s.observe == nil {
		return
	}

	s.observe(outcome)
}

// View creates a read-only snapshot over the parent.
func (s *Snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return s.createSnapshot(ctx, snapshots.KindView, key, parent, opts)
}

// adopt records a committed snapshot for a chain ID the cluster already has.
// It reports whether the snapshot was created.
func (s *Snapshotter) adopt(ctx context.Context, key, parent, target string, base snapshots.Info) (bool, error) {
	chain, err := catalog.ParseDigest(target)
	if err != nil {
		// Not a sha256 chain ID, so it can never be in the catalog. Not an
		// error, just a miss.
		return false, nil
	}

	blob, ok := s.resolve(chain)
	if !ok {
		return false, nil
	}

	// Adoption cannot be taken back. Answering ErrAlreadyExists tells
	// containerd this layer is done, so it neither fetches the blob nor
	// applies the tar, and nothing revisits that decision afterwards: every
	// later Prepare for the same chain ID finds the committed snapshot and
	// gives the same answer. A node that adopts a layer it turns out not to be
	// able to map has no local copy to fall back on, so every pod that needs
	// the layer fails to start there until somebody deletes the snapshot by
	// hand.
	//
	// The catalog saying a blob exists is not enough to know this node can
	// read it. The segment holding it may not be exported here, device mapper
	// may refuse the target, the EROFS image may not mount. So the layer is
	// mapped and mounted before the promise is made. That is work the node
	// would have done at the first container start anyway, Ensure is
	// idempotent, and Cleanup keeps the mapping for as long as a snapshot
	// refers to it. If the adoption below then fails, the mapping is left for
	// Prune, which collects anything no snapshot names.
	if _, err := s.maps.Ensure(ctx, blob.DiffID, blob); err != nil {
		return false, fmt.Errorf("map layer %s: %w", blob.DiffID.Short(), err)
	}

	labels := maps.Clone(base.Labels)
	if labels == nil {
		labels = map[string]string{}
	}

	// The ref label is kept. It looks like an instruction to this call rather
	// than a property of the result, but containerd's metadata store is what
	// turns ErrAlreadyExists into a usable answer: it walks the backend for a
	// committed snapshot whose ref label equals the target and whose parent
	// equals the one it asked for, and uses that snapshot's key. Dropping the
	// label makes that walk find nothing, and every pull of an adopted layer
	// fails with "target snapshot in backend: not found". containerd's own
	// snapshotters keep it for the same reason.
	labels[LabelBlob] = blob.DiffID.String()

	// CommitActive replaces labels rather than inheriting them, so they have to
	// be handed over explicitly.
	usage := snapshots.Usage{Size: int64(blob.Address.ByteLength)}

	err = s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		if _, err := storage.CreateSnapshot(ctx, snapshots.KindActive, key, parent); err != nil {
			return fmt.Errorf("create: %w", err)
		}

		if _, err := storage.CommitActive(ctx, key, target, usage, snapshots.WithLabels(labels)); err != nil {
			return fmt.Errorf("commit: %w", err)
		}

		return nil
	})
	if err != nil {
		return false, err
	}

	return true, nil
}

// resolve looks a chain ID up in the catalog, re-reading the catalog at most
// once every missGap when it misses.
func (s *Snapshotter) resolve(chain catalog.Digest) (catalog.Blob, bool) {
	if blob, ok := s.cat.Resolve(chain); ok {
		return blob, true
	}

	if !s.syncOnMiss() {
		return catalog.Blob{}, false
	}

	return s.cat.Resolve(chain)
}

// blob looks a diff ID up in the catalog with the same staleness handling as
// resolve.
func (s *Snapshotter) blob(diffID catalog.Digest) (catalog.Blob, bool) {
	if b, ok := s.cat.Blob(diffID); ok {
		return b, true
	}

	if !s.syncOnMiss() {
		return catalog.Blob{}, false
	}

	return s.cat.Blob(diffID)
}

// syncOnMiss re-reads the catalog if it has not been read recently. It reports
// whether a read happened.
func (s *Snapshotter) syncOnMiss() bool {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	if time.Since(s.lastSync) < s.missGap {
		return false
	}

	s.lastSync = time.Now()

	if _, err := s.cat.Sync(); err != nil {
		// A stale index only costs an unpack, so this is never fatal.
		s.log.Warn("gantry-snapshotter: catalog sync failed", "error", err)
		return false
	}

	return true
}

// Commit turns the active snapshot at key into a committed snapshot named name
// and, if the layer came from an image, queues it for ingest so the rest of the
// cluster can skip the unpack this node just did.
func (s *Snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	var (
		req    ingest.Request
		reason ingestReason
	)

	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		id, snInfo, _, err := storage.GetInfo(ctx, key)
		if err != nil {
			return err
		}

		usage, err := diskUsage(ctx, s.upperPath(id))
		if err != nil {
			return err
		}

		if _, err := storage.CommitActive(ctx, key, name, usage, opts...); err != nil {
			return fmt.Errorf("failed to commit snapshot %s: %w", key, err)
		}

		// The labels are read from the active snapshot because CommitActive
		// replaces them with whatever the caller passed, which for the CRI is
		// usually nothing.
		req, reason = ingestRequest(name, snInfo.Labels)

		return nil
	})
	if err != nil {
		return err
	}

	switch reason {
	case reasonIngest:
		if s.queue != nil && !s.queue.Submit(req) {
			s.log.Debug("gantry-snapshotter: ingest not queued", "request", req.String())
		}
	case reasonNoAnnotations:
		s.warnNoAnnotations()
	case reasonSkip:
	}

	return nil
}

// warnNoAnnotations reports, once for the life of the process, that containerd
// is not telling the snapshotter which blob a layer came from.
//
// This is worth saying loudly and exactly once. Loudly, because the node keeps
// working while quietly contributing nothing to the cluster: every image is
// unpacked locally, no layer is ever published, and every other node pays the
// same cost forever. Once, because it is a static misconfiguration, and a
// message per layer would be forty lines per image.
func (s *Snapshotter) warnNoAnnotations() {
	s.noAnnotations.Do(func() {
		s.log.Warn("gantry-snapshotter: containerd is not passing layer annotations, so no layer this node unpacks will ever reach the cluster",
			"label", LabelLayerDigest,
			"fix", "set disable_snapshot_annotations = false under [plugins.'io.containerd.cri.v1.images'] and restart containerd")
	})
}

// Remove drops a snapshot. Directories are reclaimed by Cleanup.
func (s *Snapshotter) Remove(ctx context.Context, key string) error {
	return s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		if _, _, err := storage.Remove(ctx, key); err != nil {
			return fmt.Errorf("failed to remove snapshot %s: %w", key, err)
		}

		return nil
	})
}

// Cleanup reclaims local directories and host mappings that no snapshot refers
// to any more. It also converges the node after a restart, because the device
// mappings and mounts live on tmpfs while the snapshot metadata does not.
func (s *Snapshotter) Cleanup(ctx context.Context) error {
	var (
		dirs []string
		keep map[string]struct{}
		ok   bool
	)

	// Read before the scan, not after. The watermark published at the end
	// promises that this node's mappings have been pruned against an index at
	// least this fresh, and an index only ever moves forward, so a generation
	// taken early understates the truth. Taken late it could overstate it, and
	// overstating is what lets the cleaner trim pages out from under a mount.
	generation := s.cat.Generation()

	// A write transaction is taken so no snapshot can be created while the scan
	// is deciding what is unreferenced.
	err := s.ms.WithTransaction(ctx, true, func(ctx context.Context) error {
		var err error

		dirs, err = s.orphanDirectories(ctx)
		if err != nil {
			return err
		}

		keep, ok, err = s.liveMappings(ctx)

		return err
	})
	if err != nil {
		return err
	}

	var errs []error

	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", dir, err))
		}
	}

	// Pruning is skipped when any live snapshot's blob could not be resolved.
	// Unmapping a layer a running container is reading because the catalog was
	// briefly unreadable would be far worse than leaking a mapping until the
	// next sweep.
	if ok {
		if err := s.maps.Prune(ctx, keep); err != nil {
			errs = append(errs, fmt.Errorf("prune mappings: %w", err))
		} else if err := s.cat.Watermark(generation); err != nil {
			// Not fatal. A node that cannot publish its watermark holds the
			// cleaner up, which is the safe direction: reclamation stalls
			// rather than running ahead of a node that might still be mapping
			// the segment it wants to trim.
			s.log.Warn("gantry-snapshotter: cannot publish drain watermark", "err", err)
		}
	} else {
		s.log.Warn("gantry-snapshotter: skipping mapping prune, some layers did not resolve")
	}

	return errors.Join(errs...)
}

// orphanDirectories lists snapshot directories with no snapshot behind them.
func (s *Snapshotter) orphanDirectories(ctx context.Context) ([]string, error) {
	ids, err := storage.IDMap(ctx)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(s.snapshotDir())
	if err != nil {
		return nil, err
	}

	var orphans []string

	for _, e := range entries {
		if _, live := ids[e.Name()]; live {
			continue
		}

		orphans = append(orphans, filepath.Join(s.snapshotDir(), e.Name()))
	}

	return orphans, nil
}

// Referenced is the set of layers this node's snapshots still name, and is how
// this node answers a mark round.
//
// The second return is false when the set is known to be incomplete, which the
// caller must treat as no answer at all: a mark round retires every blob no
// node claimed, so answering with a short set retires layers that are still in
// use.
//
// It reads snapshot metadata rather than the mount table on purpose. A layer
// this node has a snapshot for but has not mounted yet is still one it will
// map the moment a container starts, and a set built from what happens to be
// mounted right now would not include it.
func (s *Snapshotter) Referenced(ctx context.Context) (map[catalog.Digest]struct{}, bool, error) {
	var (
		refs     map[catalog.Digest]struct{}
		complete bool
	)

	// A read transaction is enough: a snapshot created after the scan cannot
	// reference a blob the round is about to retire, because it would have to
	// resolve it through the catalog first, and a retired blob does not
	// resolve.
	err := s.ms.WithTransaction(ctx, false, func(ctx context.Context) error {
		var err error

		refs, complete, err = s.referenced(ctx)

		return err
	})
	if err != nil {
		return nil, false, err
	}

	return refs, complete, nil
}

// referenced walks the snapshot metadata inside a transaction the caller has
// already opened.
func (s *Snapshotter) referenced(ctx context.Context) (map[catalog.Digest]struct{}, bool, error) {
	refs := map[catalog.Digest]struct{}{}
	complete := true

	err := storage.WalkInfo(ctx, func(_ context.Context, snInfo snapshots.Info) error {
		value := snInfo.Labels[LabelBlob]
		if value == "" {
			return nil
		}

		diffID, err := catalog.ParseDigest(value)
		if err != nil {
			complete = false

			s.log.Warn("gantry-snapshotter: bad blob label", "snapshot", snInfo.Name, "value", value)

			return nil
		}

		refs[diffID] = struct{}{}

		return nil
	})
	if err != nil {
		return nil, false, err
	}

	return refs, complete, nil
}

// liveMappings returns the set of host mapping names every RACER backed
// snapshot needs. The second return is false when at least one snapshot's blob
// could not be located, in which case the set is incomplete.
func (s *Snapshotter) liveMappings(ctx context.Context) (map[string]struct{}, bool, error) {
	refs, complete, err := s.referenced(ctx)
	if err != nil {
		return nil, false, err
	}

	keep := make(map[string]struct{}, len(refs))

	for diffID := range refs {
		blob, found := s.cat.Blob(diffID)
		if !found {
			complete = false

			continue
		}

		keep[s.maps.Name(diffID, blob)] = struct{}{}
	}

	return keep, complete, nil
}
