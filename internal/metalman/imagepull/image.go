// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
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

// whiteouts tracks paths hidden by upper layers.
type whiteouts struct {
	deleted map[string]bool
	opaque  map[string]bool
}

func newWhiteouts() whiteouts {
	return whiteouts{deleted: map[string]bool{}, opaque: map[string]bool{}}
}

func (w whiteouts) merge(o whiteouts) {
	for k := range o.deleted {
		w.deleted[k] = true
	}

	for k := range o.opaque {
		w.opaque[k] = true
	}
}

// hides reports whether name, as found in a lower layer, is hidden.
func (w whiteouts) hides(name string) bool {
	if w.deleted[name] {
		return true
	}

	for dir := name; ; {
		dir = path.Dir(dir)
		if dir == "/" {
			dir = "."
		}

		if w.deleted[dir] || w.opaque[dir] {
			return true
		}

		if dir == "." {
			return false
		}
	}
}

// Open returns the first file found among names, which are paths relative to
// the image root such as "disk/vmlinuz". Layers are searched top-down and
// whiteouts in upper layers hide files in lower layers. Within a layer, the
// first tar entry matching any of the names wins. Symbolic and hard links are
// followed within the image. If none of the names exist, the error wraps
// ErrNotFound.
//
// ctx bounds the lifetime of the returned File, including the registry
// requests made by later Read calls.
func (i *Image) Open(ctx context.Context, names ...string) (*File, error) {
	if len(names) == 0 {
		return nil, errors.New("imagepull: Open requires at least one name")
	}

	cleaned := make([]string, len(names))
	for k, n := range names {
		cleaned[k] = cleanName(n)
	}

	return i.open(ctx, cleaned, "", 0)
}

func (i *Image) open(ctx context.Context, names []string, alias string, depth int) (*File, error) {
	if depth > maxLinkDepth {
		return nil, fmt.Errorf("imagepull: too many levels of links resolving %v", names)
	}

	hidden := newWhiteouts()

	for li := len(i.layers) - 1; li >= 0; li-- {
		f, layerHidden, err := i.scanLayer(ctx, li, names, alias, hidden, depth)
		if err != nil || f != nil {
			return f, err
		}

		hidden.merge(layerHidden)
	}

	return nil, fmt.Errorf("%w: %s in %s", ErrNotFound, strings.Join(names, ", "), i.src.Ref)
}

// scanLayer looks for names in layer li. It returns the opened file when
// found, or the whiteouts the layer contributes for lower layers.
func (i *Image) scanLayer(ctx context.Context, li int, names []string, alias string, hidden whiteouts, depth int) (*File, whiteouts, error) {
	ls, err := i.openLayer(ctx, li)
	if err != nil {
		return nil, whiteouts{}, err
	}

	layerHidden := newWhiteouts()

	for ordinal := 0; ; ordinal++ {
		hdr, err := ls.tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			_ = ls.Close() //nolint:errcheck // already failing
			return nil, whiteouts{}, fmt.Errorf("read layer %s: %w", i.layers[li].Digest, err)
		}

		name := cleanName(hdr.Name)
		dir, base := path.Split(name)
		dir = cleanName(dir)

		if dir == "" {
			dir = "."
		}

		if base == whiteoutOpaque {
			layerHidden.opaque[dir] = true
			continue
		}

		if strings.HasPrefix(base, whiteoutPrefix) {
			layerHidden.deleted[cleanName(path.Join(dir, strings.TrimPrefix(base, whiteoutPrefix)))] = true
			continue
		}

		if !matches(name, names) || hidden.hides(name) {
			continue
		}

		matched := name
		if alias != "" {
			matched = alias
		}

		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA appears in old archives
			return &File{
				img:     i,
				ctx:     ctx,
				name:    matched,
				entry:   name,
				layer:   li,
				ordinal: ordinal,
				size:    hdr.Size,
				modTime: hdr.ModTime,
				ls:      ls,
			}, whiteouts{}, nil
		case tar.TypeSymlink:
			_ = ls.Close() //nolint:errcheck // done with this layer

			target := hdr.Linkname
			if !path.IsAbs(target) {
				target = path.Join(path.Dir("/"+name), target)
			}

			f, err := i.open(ctx, []string{cleanName(target)}, matched, depth+1)

			return f, whiteouts{}, err
		case tar.TypeLink:
			_ = ls.Close() //nolint:errcheck // done with this layer

			f, err := i.open(ctx, []string{cleanName(hdr.Linkname)}, matched, depth+1)

			return f, whiteouts{}, err
		default:
			_ = ls.Close() //nolint:errcheck // done with this layer
			return nil, whiteouts{}, fmt.Errorf("imagepull: %s in %s is not a regular file (type %q)", name, i.src.Ref, hdr.Typeflag)
		}
	}

	// The whole layer was scanned: drain what remains so that the blob is
	// read to EOF and its digest is verified.
	err = ls.drain()
	_ = ls.Close() //nolint:errcheck // read-only stream

	if err != nil {
		return nil, whiteouts{}, fmt.Errorf("read layer %s: %w", i.layers[li].Digest, err)
	}

	return nil, layerHidden, nil
}

func matches(name string, names []string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}

	return false
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

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, "", err
	}

	return data, f.Name(), nil
}

// File is an io.ReadSeekCloser over one file in an image. Seek does no I/O.
// Reading at a position before the current stream offset re-opens the layer
// and skips to the entry again; reading ahead discards bytes. This lets
// callers serve Range requests (for example with http.ServeContent) without
// an on-disk cache. A File is not safe for concurrent use.
type File struct {
	img     *Image
	ctx     context.Context //nolint:containedctx // bounds lazy re-opens done by Read
	name    string
	entry   string
	layer   int
	ordinal int
	size    int64
	modTime time.Time

	ls        *layerStream
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

	if f.off >= f.size {
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
	f.off += int64(n)
	f.streamPos += int64(n)

	if errors.Is(err, io.EOF) {
		if f.off < f.size {
			return n, io.ErrUnexpectedEOF
		}

		err = nil
	}

	return n, err
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
