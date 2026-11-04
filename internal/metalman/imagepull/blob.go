// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package imagepull

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote/auth"
)

const (
	chunkAttempts     = 3
	chunkRetryBackoff = 100 * time.Millisecond
)

// openBlob opens a verified reader over the blob described by desc, trying
// each endpoint in order until one returns a usable first response.
func (i *Image) openBlob(ctx context.Context, desc ispec.Descriptor) (io.ReadCloser, error) {
	var errs []error

	for _, ep := range i.eps {
		r, err := newBlobReader(ctx, ep, desc, i.puller.chunkSize(), i.puller.concurrency())
		if err == nil {
			return newVerifyReader(r, desc), nil
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		errs = append(errs, fmt.Errorf("%s: %w", ep.name, err))
	}

	return nil, fmt.Errorf("fetch blob %s: %w", desc.Digest, errors.Join(errs...))
}

type chunkResult struct {
	data []byte
	err  error
}

// blobReader delivers a blob in order. In ranged mode it fetches chunks
// concurrently with HTTP Range requests; otherwise it streams one response
// body.
type blobReader struct {
	ctx    context.Context
	cancel context.CancelFunc
	ep     *endpoint
	url    string
	size   int64
	chunk  int64
	conc   int

	body    io.Closer // first response body, closed on Close
	cur     io.Reader
	ranged  bool
	next    int64 // offset of the next chunk to schedule
	pending []chan chunkResult
	wg      sync.WaitGroup
	err     error
	closed  bool
}

func newBlobReader(parent context.Context, ep *endpoint, desc ispec.Descriptor, chunk int64, conc int) (*blobReader, error) {
	ctx, cancel := context.WithCancel(auth.AppendRepositoryScope(parent, ep.ref, auth.ActionPull))
	b := &blobReader{
		ctx:    ctx,
		cancel: cancel,
		ep:     ep,
		url:    fmt.Sprintf("%s/v2/%s/blobs/%s", ep.baseURL, ep.ref.Repository, desc.Digest),
		size:   desc.Size,
		chunk:  chunk,
		conc:   conc,
	}

	if err := b.start(); err != nil {
		cancel()
		return nil, err
	}

	return b, nil
}

func (b *blobReader) start() error {
	if b.conc > 1 && b.size > b.chunk {
		resp, err := b.get(0, b.chunk-1)
		if err != nil {
			return err
		}

		switch {
		case resp.StatusCode == http.StatusPartialContent && contentRangeMatches(resp, 0, b.chunk-1, b.size):
			b.ranged = true
			b.body = resp.Body
			b.cur = &exactReader{r: resp.Body, remaining: b.chunk}
			b.next = b.chunk

			return nil
		case resp.StatusCode == http.StatusOK:
			// Range is not supported (or ignored, as Gantry does for
			// bounded ranges): stream the full body sequentially.
			b.body = resp.Body
			b.cur = resp.Body

			return nil
		case resp.StatusCode == http.StatusPartialContent:
			// Unusable Content-Range: retry without Range below.
			_ = resp.Body.Close() //nolint:errcheck // discarding response
		default:
			return statusError(resp)
		}
	}

	resp, err := b.get(-1, -1)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return statusError(resp)
	}

	b.body = resp.Body
	b.cur = resp.Body

	return nil
}

// get issues a blob GET. A negative start omits the Range header.
func (b *blobReader) get(start, end int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(b.ctx, http.MethodGet, b.url, nil)
	if err != nil {
		return nil, err
	}

	if start >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	}

	return b.ep.client.Do(req)
}

func statusError(resp *http.Response) error {
	defer resp.Body.Close() //nolint:errcheck // discarding response

	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512)) //nolint:errcheck // best effort

	return &StatusError{StatusCode: resp.StatusCode, URL: resp.Request.URL.Redacted(), Body: strings.TrimSpace(string(msg))}
}

// StatusError reports an unexpected HTTP status from a blob request.
type StatusError struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("GET %s: unexpected status %d", e.URL, e.StatusCode)
	}

	return fmt.Sprintf("GET %s: unexpected status %d: %s", e.URL, e.StatusCode, e.Body)
}

func contentRangeMatches(resp *http.Response, start, end, size int64) bool {
	s, e, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
	return ok && s == start && e == end && total == size
}

// parseContentRange parses "bytes <start>-<end>/<size>".
func parseContentRange(v string) (start, end, size int64, ok bool) {
	rest, found := strings.CutPrefix(v, "bytes ")
	if !found {
		return 0, 0, 0, false
	}

	rng, total, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, 0, false
	}

	a, z, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, 0, false
	}

	var err error
	if start, err = strconv.ParseInt(a, 10, 64); err != nil {
		return 0, 0, 0, false
	}

	if end, err = strconv.ParseInt(z, 10, 64); err != nil {
		return 0, 0, 0, false
	}

	if size, err = strconv.ParseInt(total, 10, 64); err != nil {
		return 0, 0, 0, false
	}

	return start, end, size, true
}

func (b *blobReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}

	for {
		n, err := b.cur.Read(p)
		if n > 0 || (err != nil && !errors.Is(err, io.EOF)) {
			if err != nil && !errors.Is(err, io.EOF) {
				b.err = err
			}

			return n, b.errOrNil(err)
		}

		if err == nil {
			continue
		}

		// Current chunk or stream is exhausted.
		if !b.ranged {
			b.err = io.EOF
			return 0, io.EOF
		}

		if err := b.advance(); err != nil {
			b.err = err
			return 0, err
		}
	}
}

func (b *blobReader) errOrNil(err error) error {
	if errors.Is(err, io.EOF) && b.ranged {
		// EOF of a chunk is not EOF of the blob.
		return nil
	}

	return err
}

// advance moves to the next chunk in ranged mode, keeping up to conc chunk
// requests in flight.
func (b *blobReader) advance() error {
	b.schedule()

	if len(b.pending) == 0 {
		return io.EOF
	}

	ch := b.pending[0]
	b.pending = b.pending[1:]

	var res chunkResult
	select {
	case res = <-ch:
	case <-b.ctx.Done():
		return b.ctx.Err()
	}

	if res.err != nil {
		return res.err
	}

	b.cur = bytes.NewReader(res.data)
	b.schedule()

	return nil
}

func (b *blobReader) schedule() {
	for len(b.pending) < b.conc && b.next < b.size {
		start := b.next
		end := min(start+b.chunk, b.size) - 1
		b.next = end + 1

		ch := make(chan chunkResult, 1)
		b.pending = append(b.pending, ch)

		b.wg.Add(1)

		go func() {
			defer b.wg.Done()

			data, err := b.fetchChunk(start, end)
			ch <- chunkResult{data: data, err: err}
		}()
	}
}

func (b *blobReader) fetchChunk(start, end int64) ([]byte, error) {
	var lastErr error

	for attempt := range chunkAttempts {
		if attempt > 0 {
			select {
			case <-b.ctx.Done():
				return nil, b.ctx.Err()
			case <-time.After(chunkRetryBackoff * time.Duration(attempt)):
			}
		}

		data, retryable, err := b.fetchChunkOnce(start, end)
		if err == nil {
			return data, nil
		}

		if b.ctx.Err() != nil {
			return nil, b.ctx.Err()
		}

		lastErr = err

		if !retryable {
			break
		}
	}

	return nil, fmt.Errorf("fetch blob range %d-%d: %w", start, end, lastErr)
}

func (b *blobReader) fetchChunkOnce(start, end int64) ([]byte, bool, error) {
	resp, err := b.get(start, end)
	if err != nil {
		return nil, true, err
	}

	if resp.StatusCode != http.StatusPartialContent {
		retryable := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests

		return nil, retryable, statusError(resp)
	}

	defer resp.Body.Close() //nolint:errcheck // read-only body

	if !contentRangeMatches(resp, start, end, b.size) {
		return nil, false, fmt.Errorf("unexpected Content-Range %q for bytes %d-%d/%d",
			resp.Header.Get("Content-Range"), start, end, b.size)
	}

	data := make([]byte, end-start+1)
	if _, err := io.ReadFull(resp.Body, data); err != nil {
		return nil, true, err
	}

	return data, false, nil
}

// Close cancels in-flight requests and waits for them to finish.
func (b *blobReader) Close() error {
	if b.closed {
		return nil
	}

	b.closed = true
	b.cancel()

	if b.body != nil {
		_ = b.body.Close() //nolint:errcheck // canceling
	}

	b.wg.Wait()

	if b.err == nil {
		b.err = errors.New("blob reader closed")
	}

	return nil
}

// exactReader reads exactly remaining bytes, reporting a short body as
// io.ErrUnexpectedEOF.
type exactReader struct {
	r         io.Reader
	remaining int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.remaining <= 0 {
		return 0, io.EOF
	}

	if int64(len(p)) > e.remaining {
		p = p[:e.remaining]
	}

	n, err := e.r.Read(p)
	e.remaining -= int64(n)

	if errors.Is(err, io.EOF) {
		if e.remaining > 0 {
			return n, io.ErrUnexpectedEOF
		}

		err = nil
	}

	return n, err
}

// verifyReader checks size and digest when the blob is read to EOF.
type verifyReader struct {
	rc       io.ReadCloser
	desc     ispec.Descriptor
	verifier digest.Verifier
	n        int64
	err      error
}

func newVerifyReader(rc io.ReadCloser, desc ispec.Descriptor) *verifyReader {
	return &verifyReader{rc: rc, desc: desc, verifier: desc.Digest.Verifier()}
}

func (v *verifyReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}

	n, err := v.rc.Read(p)
	if n > 0 {
		v.n += int64(n)
		_, _ = v.verifier.Write(p[:n]) //nolint:errcheck // hash writes never fail

		if v.n > v.desc.Size {
			v.err = fmt.Errorf("blob %s: more than %d bytes", v.desc.Digest, v.desc.Size)
			return n, v.err
		}
	}

	if errors.Is(err, io.EOF) {
		switch {
		case v.n != v.desc.Size:
			v.err = fmt.Errorf("blob %s: got %d bytes, want %d: %w", v.desc.Digest, v.n, v.desc.Size, io.ErrUnexpectedEOF)
		case !v.verifier.Verified():
			v.err = fmt.Errorf("blob %s: %w", v.desc.Digest, ErrDigestMismatch)
		default:
			v.err = io.EOF
		}

		return n, v.err
	}

	if err != nil {
		v.err = err
	}

	return n, err
}

func (v *verifyReader) Close() error { return v.rc.Close() }

// ErrDigestMismatch reports that a blob read to EOF does not match its
// digest.
var ErrDigestMismatch = errors.New("blob digest mismatch")
