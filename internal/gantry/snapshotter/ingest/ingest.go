// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package ingest writes an OCI layer into RACER once for the whole cluster.
//
// A layer arrives here as an uncompressed tar stream. It leaves as an
// uncompressed EROFS image sitting at a known offset in a RACER segment, plus
// two catalog records: one keyed by the layer's diffID that says where the
// bytes are, and one keyed by the chainID that says which blob a containerd
// snapshot resolves to. From then on every node in the cluster mounts those
// bytes directly and containerd never fetches or unpacks that layer again.
//
// Ingest is deliberately off the container start path. The node that misses in
// the catalog unpacks the layer locally the way any snapshotter would, starts
// the container, and only then hands the layer here. The first pod on the first
// node pays the normal cost; every pod after that, on any node, does not.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/catalog"
	"github.com/Azure/unbounded/internal/gantry/snapshotter/segment"
)

// Catalog is the part of *catalog.Store the ingester needs.
type Catalog interface {
	Blob(diffID catalog.Digest) (catalog.Blob, bool)
	Resolve(chainID catalog.Digest) (catalog.Blob, bool)
	Sync() (bool, error)
	Reserve(pages uint32, records int) (catalog.Reservation, error)
	ReserveRecords(records int) (catalog.Reservation, error)
	Append(res catalog.Reservation, records []catalog.Record) error
	Abandon(res catalog.Reservation) error
	Account(id uint32, liveDelta, deadDelta int64) error
}

// Locator resolves a catalog address to a device and a byte offset on this
// node. It is the same contract blockmap consumes, and in production both are
// backed by the same segment.Watcher.
type Locator interface {
	Locate(addr segment.Address) (device string, offset, length uint64, err error)
}

// Opener produces the uncompressed tar stream for a layer. The containerd
// content store implementation lives with the snapshotter, so this package has
// no containerd dependency and can be tested with a bytes.Reader.
type Opener interface {
	Open(ctx context.Context, req Request) (rc ReadCloser, err error)
}

// ReadCloser is io.ReadCloser, named here so Opener implementations do not have
// to import io to satisfy the interface's documentation.
type ReadCloser interface {
	Read(p []byte) (int, error)
	Close() error
}

// Request names one layer to ingest.
type Request struct {
	// DiffID is the layer's uncompressed digest. It is the blob key: two
	// images that share a layer share this and therefore share one blob.
	DiffID catalog.Digest

	// ChainID is the containerd snapshot key this layer commits as. It is
	// the chain key, which is what Prepare looks up.
	ChainID catalog.Digest

	// Layer is the compressed layer digest. It is what the elector hashes,
	// so the node that gantry already chose to pull the layer is usually the
	// node that ingests it, and it is what the Opener uses to find the
	// blob in the content store.
	Layer digest.Digest
}

func (r Request) validate() error {
	if r.DiffID == (catalog.Digest{}) {
		return errors.New("ingest: request has no diff id")
	}

	if r.ChainID == (catalog.Digest{}) {
		return errors.New("ingest: request has no chain id")
	}

	return nil
}

// Outcome says what an ingest attempt actually did.
type Outcome int

const (
	// OutcomeUnknown is the zero value and is never returned on success.
	OutcomeUnknown Outcome = iota

	// OutcomePresent means the chainID already resolved. Nothing was
	// written. This is the common case once an image has been ingested
	// anywhere in the cluster.
	OutcomePresent

	// OutcomeLinked means the blob already existed under a different
	// chainID and only the chain record was added. This is cross-image
	// layer sharing, and it costs one record and no bytes.
	OutcomeLinked

	// OutcomeIngested means the blob was built and written.
	OutcomeIngested
)

// String renders an outcome for logs.
func (o Outcome) String() string {
	switch o {
	case OutcomePresent:
		return "present"
	case OutcomeLinked:
		return "linked"
	case OutcomeIngested:
		return "ingested"
	case OutcomeUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("unknown(%d)", int(o))
	}
}

// Result reports what an ingest attempt did and, when a blob is now resolvable,
// where it lives.
type Result struct {
	Outcome Outcome
	Blob    catalog.Blob
}

// Options configures an Ingester.
type Options struct {
	// Catalog is the cluster index. Required.
	Catalog Catalog

	// Locator maps a reservation to this node's device. Required.
	Locator Locator

	// Opener supplies layer tar streams. Required.
	Opener Opener

	// Builder converts tar to erofs. Defaults to NewBuilder().
	Builder *Builder

	// Open opens a segment device. Defaults to OpenDirect.
	Open OpenFunc

	// WorkDir holds the tarball and the erofs image while they are being
	// built. It must be on a filesystem with room for the largest layer
	// twice. Required.
	WorkDir string

	// SkipVerify turns off the read-back check after a write. Leave it off:
	// RACER's 4 MiB pages carry no data checksum, and the cluster trusts
	// this blob on the strength of one record.
	SkipVerify bool
}

// Ingester writes layers into RACER and publishes them in the catalog.
//
// It holds no queue of its own. Serialization, retry policy and lifetime belong
// to the caller, which knows about pod churn and containerd's own concurrency
// limits; an Ingester is safe for concurrent use and each call is independent.
type Ingester struct {
	cat     Catalog
	loc     Locator
	opener  Opener
	builder *Builder
	open    OpenFunc
	workDir string
	verify  bool
}

// New builds an Ingester.
func New(opts Options) (*Ingester, error) {
	if opts.Catalog == nil {
		return nil, errors.New("ingest: no catalog")
	}

	if opts.Locator == nil {
		return nil, errors.New("ingest: no locator")
	}

	if opts.Opener == nil {
		return nil, errors.New("ingest: no opener")
	}

	if opts.WorkDir == "" {
		return nil, errors.New("ingest: no work directory")
	}

	if err := os.MkdirAll(opts.WorkDir, 0o700); err != nil {
		return nil, fmt.Errorf("ingest: work directory: %w", err)
	}

	i := &Ingester{
		cat:     opts.Catalog,
		loc:     opts.Locator,
		opener:  opts.Opener,
		builder: opts.Builder,
		open:    opts.Open,
		workDir: opts.WorkDir,
		verify:  !opts.SkipVerify,
	}

	if i.builder == nil {
		i.builder = NewBuilder()
	}

	if i.open == nil {
		i.open = OpenDirect
	}

	return i, nil
}

// Ingest publishes one layer.
//
// The order of the checks is the order of increasing cost: an already resolved
// chain costs a map lookup, a shared layer costs one record, and only a genuine
// miss on an elected node builds and writes anything.
func (i *Ingester) Ingest(ctx context.Context, req Request) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}

	// Another node may have published this layer since the last time this
	// node read the catalog. A sync failure is not fatal: the worst case is
	// a duplicate blob, and refusing to ingest because the index is stale
	// would turn a transient fabric error into a permanent cold path.
	_, _ = i.cat.Sync() //nolint:errcheck

	if blob, ok := i.cat.Resolve(req.ChainID); ok {
		return Result{Outcome: OutcomePresent, Blob: blob}, nil
	}

	if blob, ok := i.cat.Blob(req.DiffID); ok {
		if err := i.link(req, blob); err != nil {
			return Result{}, err
		}

		return Result{Outcome: OutcomeLinked, Blob: blob}, nil
	}

	blob, err := i.build(ctx, req)
	if err != nil {
		return Result{}, err
	}

	return Result{Outcome: OutcomeIngested, Blob: blob}, nil
}

// link adds only the chain record for a blob that already exists.
func (i *Ingester) link(req Request, blob catalog.Blob) (err error) {
	res, err := i.cat.ReserveRecords(1)
	if err != nil {
		return fmt.Errorf("ingest: reserve chain record: %w", err)
	}

	defer func() { err = i.abandon(res, err) }()

	rec := catalog.Record{
		Type:       catalog.RecordChain,
		Generation: res.Generation,
		Key:        req.ChainID,
		Ref:        blob.DiffID,
	}

	if err := i.cat.Append(res, []catalog.Record{rec}); err != nil {
		return fmt.Errorf("ingest: publish chain record: %w", err)
	}

	return nil
}

// abandon retires a reservation that is about to be dropped on the floor.
//
// It is deferred by every function that takes one, because a reservation left
// unfilled is not a lost layer, it is a hole that stops every reader in the
// cluster at that slot forever. Returning nil early on success keeps the happy
// path free of it.
//
// A failure to abandon is worth more than the failure that caused it, since it
// is the one that is not local to this node, so it is joined onto the original
// rather than replacing or hiding it.
func (i *Ingester) abandon(res catalog.Reservation, cause error) error {
	if cause == nil {
		return nil
	}

	if err := i.cat.Abandon(res); err != nil {
		return errors.Join(cause, fmt.Errorf(
			"ingest: catalog records %d..%d are a permanent hole, every node stops reading there: %w",
			res.FirstRecord, res.FirstRecord+uint64(res.RecordCount), err)) //nolint:gosec // record count is small and positive
	}

	return cause
}

// build converts the layer to erofs, writes it into a segment and publishes it.
func (i *Ingester) build(ctx context.Context, req Request) (blob catalog.Blob, err error) {
	dir, err := os.MkdirTemp(i.workDir, "b-")
	if err != nil {
		return catalog.Blob{}, fmt.Errorf("ingest: scratch directory: %w", err)
	}

	defer func() { _ = os.RemoveAll(dir) }() //nolint:errcheck

	imagePath, size, err := i.erofs(ctx, dir, req)
	if err != nil {
		return catalog.Blob{}, err
	}

	pages := segment.PagesFor(size)

	// Two records: the blob and the chain that names it. Taking both slots
	// in the same reservation means a reader can never see a chain record
	// pointing at a blob record that was never written.
	res, err := i.cat.Reserve(pages, 2)
	if err != nil {
		return catalog.Blob{}, fmt.Errorf("ingest: reserve %d pages: %w", pages, err)
	}

	// Everything from here to Append can fail, and the write in the middle
	// is multi-gigabyte O_DIRECT I/O that takes long enough to be worth
	// cancelling. None of it may leave the reservation unfilled.
	//
	// Once Append lands the slots hold real records and the layer is live,
	// so the guard stops there: accounting failures afterwards must not
	// void records that other nodes are already resolving through.
	published := false

	defer func() {
		if published {
			return
		}

		err = i.abandon(res, err)
	}()

	addr := res.Address(size)

	sum, err := i.write(imagePath, addr, size)
	if err != nil {
		return catalog.Blob{}, err
	}

	records := []catalog.Record{
		{
			Type:       catalog.RecordBlob,
			Segment:    addr.Segment,
			PageOffset: addr.PageOffset,
			PageCount:  addr.PageCount,
			ByteLength: addr.ByteLength,
			Generation: res.Generation,
			Key:        req.DiffID,
			Ref:        sum,
		},
		{
			Type:       catalog.RecordChain,
			Generation: res.Generation,
			Key:        req.ChainID,
			Ref:        req.DiffID,
		},
	}

	if err := i.cat.Append(res, records); err != nil {
		return catalog.Blob{}, fmt.Errorf("ingest: publish records: %w", err)
	}

	published = true

	// Accounting is separate from the reservation on purpose: it is the
	// cleaner's input, not a correctness invariant, and folding it into the
	// reservation compare-and-swap would double the contention on the one
	// block every ingest in the cluster has to write.
	if err := i.cat.Account(addr.Segment, int64(addr.Span()), 0); err != nil {
		return catalog.Blob{}, fmt.Errorf("ingest: account segment %d: %w", addr.Segment, err)
	}

	return catalog.Blob{
		DiffID:     req.DiffID,
		Address:    addr,
		Sum:        sum,
		Generation: res.Generation,
	}, nil
}

// erofs materialises the layer as an erofs image in dir and returns its path
// and size.
func (i *Ingester) erofs(ctx context.Context, dir string, req Request) (string, uint64, error) {
	rc, err := i.opener.Open(ctx, req)
	if err != nil {
		return "", 0, fmt.Errorf("ingest: open layer %s: %w", req.DiffID.Short(), err)
	}

	tarPath, _, err := Spill(dir, "layer-*.tar", rc)

	if cerr := rc.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("ingest: close layer: %w", cerr)
	}

	if err != nil {
		return "", 0, err
	}

	imagePath := filepath.Join(dir, "layer.erofs")

	size, err := i.builder.Build(ctx, BuildOptions{
		TarPath: tarPath,
		OutPath: imagePath,
		UUID:    UUIDFor(req.DiffID.String()),
	})
	if err != nil {
		return "", 0, err
	}

	// The tarball is the larger of the two and is no longer needed. Freeing
	// it here rather than at the end of build keeps the peak footprint at
	// one layer rather than two.
	_ = os.Remove(tarPath) //nolint:errcheck

	return imagePath, size, nil
}

// write copies the built image into the reserved page range and returns its
// sha256.
func (i *Ingester) write(imagePath string, addr segment.Address, size uint64) (catalog.Digest, error) {
	var sum catalog.Digest

	device, offset, _, err := i.loc.Locate(addr)
	if err != nil {
		return sum, fmt.Errorf("ingest: locate reservation: %w", err)
	}

	src, err := os.Open(imagePath)
	if err != nil {
		return sum, fmt.Errorf("ingest: open erofs image: %w", err)
	}

	defer func() { _ = src.Close() }() //nolint:errcheck

	dev, err := i.open(device)
	if err != nil {
		return sum, err
	}

	defer func() { _ = dev.Close() }() //nolint:errcheck

	sum, err = WriteBlob(dev, offset, src, size)
	if err != nil {
		return sum, err
	}

	if i.verify {
		if err := VerifyBlob(dev, offset, size, sum); err != nil {
			return sum, err
		}
	}

	return sum, nil
}
