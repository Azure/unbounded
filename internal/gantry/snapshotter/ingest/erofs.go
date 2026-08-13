// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// DefaultBinary is the erofs image builder this package shells out to.
const DefaultBinary = "mkfs.erofs"

// DefaultBlockBits is the log2 of the EROFS block size the builder pins.
//
// mkfs.erofs otherwise defaults to the build host's page size. An image built
// with 64 KiB blocks on an arm64 host with 64 KiB pages cannot be mounted on a
// node with 4 KiB pages, and the whole point of this snapshotter is that any
// node can mount any blob. 4 KiB is the smallest page size in the fleet and is
// mountable everywhere.
const DefaultBlockBits = 12

// DefaultBuildTimeout bounds a single mkfs.erofs invocation.
const DefaultBuildTimeout = 10 * time.Minute

// ErrBuildFailed reports that the erofs builder exited non zero.
var ErrBuildFailed = errors.New("ingest: erofs build failed")

var (
	errNoSelf    = errors.New("ingest: no node id")
	errNoMembers = errors.New("ingest: no membership source")
)

// BuildOptions describes one erofs image build.
type BuildOptions struct {
	// TarPath is the uncompressed layer tarball to convert. Required.
	TarPath string

	// OutPath is where the erofs image is written. Required.
	OutPath string

	// UUID pins the image's superblock UUID. Two nodes that race to ingest
	// the same layer then produce byte identical images, which makes a
	// mismatch between two copies a real corruption signal rather than
	// expected noise. Optional.
	UUID string
}

func (o BuildOptions) validate() error {
	if o.TarPath == "" {
		return errors.New("ingest: no tar path")
	}

	if o.OutPath == "" {
		return errors.New("ingest: no output path")
	}

	return nil
}

// Builder converts an OCI layer tarball into an uncompressed EROFS image.
//
// Uncompressed is deliberate. EROFS can compress, but every compressed block a
// container touches costs a decompression on the read path, and this
// snapshotter's whole reason to exist is the read path. The bytes are already
// stored once for the cluster instead of once per node, so the space that
// compression would save is the cheapest thing in the system.
//
// The builder shells out rather than linking a library because erofs-utils has
// no stable Go binding and the format's writer side is not something worth
// reimplementing. It runs once per layer per cluster, so the fork cost is
// irrelevant next to the read path it feeds.
type Builder struct {
	// Binary is the builder executable. Defaults to DefaultBinary.
	Binary string

	// BlockBits is log2 of the EROFS block size. Defaults to
	// DefaultBlockBits.
	BlockBits int

	// Timeout bounds one build. Defaults to DefaultBuildTimeout.
	Timeout time.Duration

	// ExtraArgs are appended before the positional arguments. They exist so
	// an operator can turn on a newer erofs-utils feature without a rebuild.
	ExtraArgs []string

	// run is indirected so tests do not need erofs-utils installed.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewBuilder returns a builder with the defaults applied.
func NewBuilder() *Builder { return &Builder{} }

func (b *Builder) binary() string {
	if b.Binary != "" {
		return b.Binary
	}

	return DefaultBinary
}

func (b *Builder) blockBits() int {
	if b.BlockBits > 0 {
		return b.BlockBits
	}

	return DefaultBlockBits
}

func (b *Builder) timeout() time.Duration {
	if b.Timeout > 0 {
		return b.Timeout
	}

	return DefaultBuildTimeout
}

// Args returns the argument vector Build would run. It is exported so the
// arguments can be asserted in a test without executing anything.
func (b *Builder) Args(opts BuildOptions) []string {
	args := []string{
		// Full tar mode: the layer's data is written into the image
		// rather than left in an external blob. An index only image
		// would need the tarball to stay reachable at read time, which
		// would put a second object in RACER per layer for no gain.
		"--tar=f",
		// Convert AUFS whiteouts, which is what OCI layers carry, into
		// the character devices and opaque xattrs overlayfs expects.
		// Without this a layer that deletes a file from its parent
		// silently fails to delete it.
		"--aufs",
		"-b" + strconv.Itoa(1<<b.blockBits()),
	}

	if opts.UUID != "" {
		args = append(args, "-U"+opts.UUID)
	}

	args = append(args, b.ExtraArgs...)

	return append(args, opts.OutPath, opts.TarPath)
}

// Build converts opts.TarPath into an uncompressed erofs image at opts.OutPath
// and returns the image's size in bytes.
//
// A failed build removes the partial output. Leaving it would make a retry
// look like a build that had already succeeded.
func (b *Builder) Build(ctx context.Context, opts BuildOptions) (uint64, error) {
	if err := opts.validate(); err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(ctx, b.timeout())
	defer cancel()

	run := b.run
	if run == nil {
		run = runCommand
	}

	out, err := run(ctx, b.binary(), b.Args(opts)...)
	if err != nil {
		_ = os.Remove(opts.OutPath) //nolint:errcheck

		if len(out) > 0 {
			return 0, fmt.Errorf("%w: %w: %s", ErrBuildFailed, err, trim(out))
		}

		return 0, fmt.Errorf("%w: %w", ErrBuildFailed, err)
	}

	info, err := os.Stat(opts.OutPath)
	if err != nil {
		return 0, fmt.Errorf("ingest: stat erofs image: %w", err)
	}

	if info.Size() <= 0 {
		_ = os.Remove(opts.OutPath) //nolint:errcheck

		return 0, fmt.Errorf("%w: empty image", ErrBuildFailed)
	}

	return uint64(info.Size()), nil
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// trim shortens builder output so a failure does not paste a whole build log
// into an error string.
func trim(out []byte) string {
	const limit = 512

	if len(out) > limit {
		out = out[len(out)-limit:]
	}

	return string(out)
}

// Spill copies r into a new file under dir and returns its path.
//
// mkfs.erofs takes a path, not a stream. Feeding it a pipe would work but would
// make a retry impossible without re-fetching the layer, and the layer is
// already on local disk in containerd's content store, so a second copy on the
// same disk is the cheaper trade.
func Spill(dir, pattern string, r io.Reader) (path string, size uint64, err error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", 0, fmt.Errorf("ingest: create temp: %w", err)
	}

	defer func() {
		cerr := f.Close()
		if err == nil && cerr != nil {
			err = fmt.Errorf("ingest: close temp: %w", cerr)
		}

		if err != nil {
			_ = os.Remove(f.Name()) //nolint:errcheck
		}
	}()

	n, err := io.Copy(f, r)
	if err != nil {
		return "", 0, fmt.Errorf("ingest: buffer layer: %w", err)
	}

	return f.Name(), uint64(n), nil
}

// UUIDFor derives a stable RFC 4122 version 5 style UUID from a name.
//
// Two nodes that race on the same layer build the same image only if every
// input to mkfs.erofs is the same, and the superblock UUID is otherwise random.
func UUIDFor(name string) string {
	sum := sha256.Sum256([]byte("gantry-snapshotter/" + name))

	var u [16]byte

	copy(u[:], sum[:16])

	u[6] = (u[6] & 0x0f) | 0x50
	u[8] = (u[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
