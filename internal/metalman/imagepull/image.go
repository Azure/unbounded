// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Azure/unbounded/internal/ociutil"
)

const (
	// MaxReadFileSize is the largest file Image.ReadFile reads into memory.
	MaxReadFileSize int64 = 16 << 20

	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"

	maxLinkDepth = 16
)

type compression int

const (
	compressionNone compression = iota
	compressionGzip
	compressionZstd
)

func layerCompression(mediaType string) (compression, error) {
	switch mediaType {
	case ispec.MediaTypeImageLayer, ociutil.DockerMediaTypeLayer:
		return compressionNone, nil
	case ispec.MediaTypeImageLayerGzip, ociutil.DockerMediaTypeLayerGzip:
		return compressionGzip, nil
	case ispec.MediaTypeImageLayerZstd:
		return compressionZstd, nil
	default:
		return 0, fmt.Errorf("unsupported layer media type %q", mediaType)
	}
}

// Image is a single-platform image pinned to a manifest digest. It is safe
// for concurrent use.
type Image struct {
	puller *Puller
	src    Source
	eps    []*endpoint
	digest string
	layers []ispec.Descriptor
}

// Digest returns the platform manifest digest.
func (i *Image) Digest() string { return i.digest }

// Source returns the source the image was resolved from.
func (i *Image) Source() Source { return i.src }

// layerStream is an open, decompressed tar stream over one layer.
type layerStream struct {
	blob io.ReadCloser
	dec  io.Closer
	r    io.Reader // decompressed stream
	tr   *tar.Reader
}

func (l *layerStream) Close() error {
	if l.dec != nil {
		_ = l.dec.Close() //nolint:errcheck // decompressor close is best effort
	}

	return l.blob.Close()
}

// drain reads the rest of the decompressed stream and the raw blob to EOF,
// which verifies the blob digest.
func (l *layerStream) drain() error {
	if _, err := io.Copy(io.Discard, l.tr); err != nil {
		return err
	}

	if l.r != nil {
		if _, err := io.Copy(io.Discard, l.r); err != nil {
			return err
		}
	}

	_, err := io.Copy(io.Discard, l.blob)

	return err
}

func (i *Image) openLayer(ctx context.Context, idx int) (*layerStream, error) {
	desc := i.layers[idx]

	comp, err := layerCompression(desc.MediaType)
	if err != nil {
		return nil, err
	}

	blob, err := i.openBlob(ctx, desc)
	if err != nil {
		return nil, err
	}

	ls := &layerStream{blob: blob}

	var r io.Reader = blob

	switch comp {
	case compressionGzip:
		zr, err := gzip.NewReader(blob)
		if err != nil {
			_ = blob.Close() //nolint:errcheck // already failing
			return nil, fmt.Errorf("layer %s: %w", desc.Digest, err)
		}

		ls.dec = zr
		r = zr
	case compressionZstd:
		zr, err := zstd.NewReader(blob, zstd.WithDecoderConcurrency(1))
		if err != nil {
			_ = blob.Close() //nolint:errcheck // already failing
			return nil, fmt.Errorf("layer %s: %w", desc.Digest, err)
		}

		ls.dec = zr.IOReadCloser()
		r = zr
	case compressionNone:
	}

	ls.r = r
	ls.tr = tar.NewReader(r)

	return ls, nil
}

// cleanName normalizes an image path to a slash-separated path relative to
// the image root, without a leading "/" or "./".
func cleanName(name string) string {
	name = path.Clean("/" + name)
	return strings.TrimPrefix(name, "/")
}

// errNotRegular reports that a matched path is not a regular file.
var errNotRegular = errors.New("not a regular file")

// Open returns the first file found among names, which are paths relative to
// the image root such as "disk/vmlinuz". Layers are searched top-down and
// whiteouts in upper layers hide files in lower layers. When several names
// exist, the one found in the highest layer wins; among names found in the
// same layer, the earliest argument wins. Symbolic links (including links
// in parent directories) and hard links are followed within the image. If
// none of the names exist, the error wraps ErrNotFound.
//
// Open uses the Puller's layer indexes and does not read file content. The
// first time a layer is needed, it is read once in full to build its index;
// afterwards, missing names are answered without any registry requests.
//
// ctx bounds the lifetime of the returned File, including the registry
// requests made by later Read calls.
func (i *Image) Open(ctx context.Context, names ...string) (*File, error) {
	if len(names) == 0 {
		return nil, errors.New("imagepull: Open requires at least one name")
	}

	var (
		best      *File
		bestErr   error
		bestLayer = -1
	)

	for _, n := range names {
		name := cleanName(n)

		li, e, first, err := i.lookup(ctx, name)
		if errors.Is(err, ErrNotFound) {
			continue
		}

		if first < 0 {
			// Registry or index failure.
			return nil, err
		}

		if first <= bestLayer {
			continue
		}

		bestLayer, best, bestErr = first, nil, err
		if err == nil {
			best = &File{
				img:     i,
				ctx:     ctx,
				name:    name,
				entry:   e.entry,
				layer:   li,
				ordinal: e.ordinal,
				size:    e.size,
				modTime: e.modTime,
				sum:     e.sum,
			}
		}

		if first == len(i.layers)-1 {
			break
		}
	}

	if bestLayer < 0 {
		return nil, fmt.Errorf("%w: %s in %s", ErrNotFound, strings.Join(names, ", "), i.src.Ref)
	}

	if bestErr != nil {
		return nil, bestErr
	}

	return best, nil
}

// implicitDir is the entry stat returns for a directory that has no tar
// entry of its own.
var implicitDir = &indexEntry{kind: kindDir}

// stat returns the topmost visible entry for the exact path p, without
// following links, and the layer it is in. A nil entry means p does not
// exist.
func (i *Image) stat(ctx context.Context, p string) (int, *indexEntry, error) {
	for li := len(i.layers) - 1; li >= 0; li-- {
		x, err := i.layerIndex(ctx, li)
		if err != nil {
			return -1, nil, err
		}

		if e := x.entries[p]; e != nil {
			return li, e, nil
		}

		// A directory implied by entries below it shadows lower layers just
		// like an explicit directory entry.
		if x.dirs[p] {
			return li, implicitDir, nil
		}

		if x.hides(p) {
			return -1, nil, nil
		}
	}

	return -1, nil, nil
}

// lookup resolves name to a regular file, following symbolic links in any
// path component and hard links. It returns the layer and entry holding the
// content, and the layer where name itself was first matched (for
// precedence between names), which is -1 when name does not exist or the
// layer indexes could not be loaded.
func (i *Image) lookup(ctx context.Context, name string) (int, *indexEntry, int, error) {
	first := -1
	notFound := func() error {
		return fmt.Errorf("%w: %s in %s", ErrNotFound, name, i.src.Ref)
	}

	p := name
	depth := 0

	for {
		if p == "" {
			return -1, nil, first, notFound()
		}

		comps := strings.Split(p, "/")
		cur := ""
		restart := false

		for k, c := range comps {
			next := path.Join(cur, c)
			last := k == len(comps)-1

			li, e, err := i.stat(ctx, next)
			if err != nil {
				return -1, nil, -1, err
			}

			if e == nil || e == implicitDir {
				if last {
					return -1, nil, first, notFound()
				}

				// Directories need not have their own tar entry.
				cur = next

				continue
			}

			if first < 0 && (last || e.kind == kindSymlink) {
				first = li
			}

			switch {
			case e.kind == kindDir && !last:
				cur = next
				continue
			case e.kind == kindSymlink, e.kind == kindHardlink && last && !e.resolved:
				depth++
				if depth > maxLinkDepth {
					return -1, nil, first, fmt.Errorf("imagepull: too many levels of links resolving %s in %s", name, i.src.Ref)
				}

				target := e.link
				if e.kind == kindSymlink && !path.IsAbs(target) {
					target = path.Join("/"+cur, target)
				}

				p = cleanName(path.Join(append([]string{"/" + target}, comps[k+1:]...)...))
				restart = true
			case e.kind == kindReg && last, e.kind == kindHardlink && last:
				return li, e, first, nil
			case last:
				return -1, nil, first, fmt.Errorf("imagepull: %s in %s: %w", next, i.src.Ref, errNotRegular)
			default:
				// A non-directory used as a directory.
				return -1, nil, first, notFound()
			}

			break
		}

		if !restart {
			return -1, nil, first, notFound()
		}
	}
}

// ReadFile reads the first file found among names into memory and returns
// its content and the matched name. Files larger than MaxReadFileSize are
// rejected.
func (i *Image) ReadFile(ctx context.Context, names ...string) ([]byte, string, error) {
	f, err := i.Open(ctx, names...)
	if err != nil {
		return nil, "", err
	}
	defer f.Close() //nolint:errcheck // read-only

	if f.Size() > MaxReadFileSize {
		return nil, "", fmt.Errorf("imagepull: %s is %d bytes, larger than the %d byte limit", f.Name(), f.Size(), MaxReadFileSize)
	}

	// File.Read verifies the content digest when the file is read to EOF
	// from offset 0, so the bytes below are verified.
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, "", err
	}

	return data, f.Name(), nil
}

// File is an io.ReadSeekCloser over one file in an image. Seek does no I/O.
// The layer is opened on the first Read. Reading at a position before the
// current stream offset re-opens the layer and skips to the entry again;
// reading ahead discards bytes. This lets callers serve Range requests (for
// example with http.ServeContent) without an on-disk cache.
//
// When the file is read sequentially from offset 0 to EOF, its content is
// checked against the sha256 recorded in the layer index, and the final
// Read returns an error wrapping ErrDigestMismatch instead of io.EOF on a
// mismatch. Reads that start at a non-zero offset cannot be verified.
//
// A File is not safe for concurrent use.
type File struct {
	img     *Image
	ctx     context.Context //nolint:containedctx // bounds lazy re-opens done by Read
	name    string
	entry   string
	layer   int
	ordinal int
	size    int64
	modTime time.Time
	sum     digest.Digest

	ls        *layerStream
	hasher    hash.Hash
	hashed    int64 // bytes hashed contiguously from offset 0, or -1
	verified  bool
	err       error
	streamPos int64
	off       int64
	closed    bool
}

var _ io.ReadSeekCloser = (*File)(nil)

// Name returns the matched name, relative to the image root.
func (f *File) Name() string { return f.name }

// Size returns the file size in bytes.
func (f *File) Size() int64 { return f.size }

// ModTime returns the file modification time recorded in the layer.
func (f *File) ModTime() time.Time { return f.modTime }

// Read implements io.Reader.
func (f *File) Read(p []byte) (int, error) {
	if f.closed {
		return 0, errors.New("imagepull: read of closed file")
	}

	if f.err != nil {
		return 0, f.err
	}

	if f.off >= f.size {
		if err := f.verify(); err != nil {
			return 0, err
		}

		return 0, io.EOF
	}

	if f.ls == nil || f.off < f.streamPos {
		if err := f.reopen(); err != nil {
			return 0, err
		}
	}

	if f.off > f.streamPos {
		n, err := io.CopyN(io.Discard, f.ls.tr, f.off-f.streamPos)
		f.streamPos += n

		if err != nil {
			return 0, fmt.Errorf("imagepull: skip in %s: %w", f.name, err)
		}
	}

	if remaining := f.size - f.off; int64(len(p)) > remaining {
		p = p[:remaining]
	}

	n, err := f.ls.tr.Read(p)
	f.hash(p[:n])
	f.off += int64(n)
	f.streamPos += int64(n)

	if errors.Is(err, io.EOF) {
		if f.off < f.size {
			return n, io.ErrUnexpectedEOF
		}

		err = nil
	}

	if err == nil && f.off == f.size {
		err = f.verify()
	}

	return n, err
}

// hash feeds bytes read at f.off into the content hash while reads are
// contiguous from offset 0.
func (f *File) hash(b []byte) {
	if f.off == 0 {
		f.hasher = sha256.New()
		f.hashed = 0
		f.verified = false
	}

	if f.hasher == nil || f.hashed != f.off {
		f.hashed = -1
		return
	}

	_, _ = f.hasher.Write(b) //nolint:errcheck // hash writes never fail
	f.hashed += int64(len(b))
}

// verify checks the content digest once the whole file has been hashed.
func (f *File) verify() error {
	if f.verified || f.sum == "" {
		return nil
	}

	if f.size == 0 && f.hasher == nil {
		f.hasher = sha256.New()
		f.hashed = 0
	}

	if f.hasher == nil || f.hashed != f.size {
		return nil
	}

	if got := digest.NewDigest(digest.SHA256, f.hasher); got != f.sum {
		f.err = fmt.Errorf("imagepull: %s in %s: got %s, want %s: %w", f.name, f.img.src.Ref, got, f.sum, ErrDigestMismatch)
		return f.err
	}

	f.verified = true

	return nil
}

func (f *File) reopen() error {
	if f.ls != nil {
		_ = f.ls.Close() //nolint:errcheck // replacing stream
		f.ls = nil
	}

	ls, err := f.img.openLayer(f.ctx, f.layer)
	if err != nil {
		return err
	}

	for k := 0; k <= f.ordinal; k++ {
		hdr, err := ls.tr.Next()
		if err != nil {
			_ = ls.Close() //nolint:errcheck // already failing
			return fmt.Errorf("imagepull: reopen %s: %w", f.name, err)
		}

		if k == f.ordinal && (cleanName(hdr.Name) != f.entry || hdr.Size != f.size) {
			_ = ls.Close() //nolint:errcheck // already failing
			return fmt.Errorf("imagepull: reopen %s: layer content changed", f.name)
		}
	}

	f.ls = ls
	f.streamPos = 0

	return nil
}

// Seek implements io.Seeker. It only records the new offset.
func (f *File) Seek(offset int64, whence int) (int64, error) {
	if f.closed {
		return 0, errors.New("imagepull: seek on closed file")
	}

	var abs int64

	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.off + offset
	case io.SeekEnd:
		abs = f.size + offset
	default:
		return 0, fmt.Errorf("imagepull: invalid whence %d", whence)
	}

	if abs < 0 {
		return 0, errors.New("imagepull: negative position")
	}

	f.off = abs

	return abs, nil
}

// Close releases the layer stream and cancels in-flight requests.
func (f *File) Close() error {
	if f.closed {
		return nil
	}

	f.closed = true

	if f.ls != nil {
		err := f.ls.Close()
		f.ls = nil

		return err
	}

	return nil
}
