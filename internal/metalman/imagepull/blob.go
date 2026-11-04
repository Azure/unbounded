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

// openBlob opens a verified reader over the blob described by desc. The
// endpoints are tried in order; if one fails mid-stream, the read resumes
// from the next endpoint at the current offset.
//
// When a mirror served any bytes of a blob that later fails verification,
// the blob is marked so that later reads through this Image, and through
// other Images of the same Puller until Puller.MirrorQuarantine elapses,
// skip the mirror and go to the upstream registry only.
func (i *Image) openBlob(ctx context.Context, desc ispec.Descriptor) (*blobStream, error) {
	eps := i.eps
	if i.mirrorAvoided(desc.Digest) {
		eps = eps[len(eps)-1:]
	}

	r := &resumeReader{ctx: ctx, eps: eps, desc: desc, p: i.puller}
	if err := r.openNext(); err != nil {
		return nil, err
	}

	v := newVerifyReader(r, desc)
	v.onMismatch = func() { i.noteMismatch(desc.Digest, r) }

	return &blobStream{verifyReader: v, rr: r}, nil
}

// blobStream is a verified blob reader that also reports whether a mirror
// served any of its bytes.
type blobStream struct {
	*verifyReader
	rr *resumeReader
}

// fromMirror reports whether a mirror endpoint delivered any bytes.
func (b *blobStream) fromMirror() bool { return b.rr.mirrorBytes }

// noteMismatch marks blob d as served corrupted by the mirror when the
// mirror delivered any of the bytes read by r.
func (i *Image) noteMismatch(d digest.Digest, r *resumeReader) {
	if r.mirrorBytes {
		i.badMirror.Store(d, true)

		if len(i.eps) > 1 {
			i.puller.state().quarantine.add(i.eps[0].name, d.String())
		}
	}
}

// mirrorAvoided reports whether blob d must be read from upstream only.
func (i *Image) mirrorAvoided(d digest.Digest) bool {
	if len(i.eps) < 2 {
		return false
	}

	if _, ok := i.badMirror.Load(d); ok {
		return true
	}

	return i.puller.state().quarantine.has(i.eps[0].name, d.String())
}

// resumeReader reads a blob from the first working endpoint and, when that
// endpoint fails after some bytes were delivered, resumes from the next
// endpoint at the current offset.
type resumeReader struct {
	ctx  context.Context //nolint:containedctx // bounds resumed requests
	eps  []*endpoint
	desc ispec.Descriptor
	p    *Puller

	next int // index of the next endpoint to try
	cur  *blobReader
	// mirrorBytes is set once any endpoint other than the last (the
	// upstream registry) has delivered bytes.
	mirrorBytes bool
	off         int64
	errs        []error
	err         error
}

func (r *resumeReader) openNext() error {
	for r.next < len(r.eps) {
		ep := r.eps[r.next]
		r.next++

		br, err := newBlobReader(r.ctx, ep, r.desc, r.off, r.p)
		if err == nil {
			r.cur = br
			return nil
		}

		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}

		r.errs = append(r.errs, fmt.Errorf("%s: %w", ep.name, err))
	}

	return fmt.Errorf("fetch blob %s: %w", r.desc.Digest, errors.Join(r.errs...))
}

func (r *resumeReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}

	for {
		n, err := r.cur.Read(p)
		r.off += int64(n)

		if n > 0 && r.next < len(r.eps) {
			r.mirrorBytes = true
		}

		// Only a bare io.EOF marks the end of the blob: transport errors
		// such as a dropped connection may wrap io.EOF.
		if err == nil || err == io.EOF { //nolint:errorlint // see above
			return n, err
		}

		_ = r.cur.Close() //nolint:errcheck // replacing a failed reader
		r.cur = nil

		if r.ctx.Err() != nil {
			r.err = r.ctx.Err()
			return n, r.err
		}

		r.errs = append(r.errs, fmt.Errorf("%s at offset %d: %w", r.eps[r.next-1].name, r.off, err))

		if oerr := r.openNext(); oerr != nil {
			r.err = oerr
			return n, r.err
		}

		if n > 0 {
			return n, nil
		}
	}
}

func (r *resumeReader) Close() error {
	if r.cur != nil {
		err := r.cur.Close()
		r.cur = nil

		return err
	}

	return nil
}

type chunkResult struct {
	data []byte
	err  error
}

// pendingChunk is a prefetched chunk request and the budget it holds.
type pendingChunk struct {
	ch     chan chunkResult
	charge int64
}

var errBlobReaderClosed = errors.New("blob reader closed")

// blobReader delivers a blob in order from one endpoint, starting at a given
// offset. In ranged mode it fetches chunks concurrently with HTTP Range
// requests, holding prefetched chunks within the Puller memory budget;
// otherwise it streams one response body.
//
// A blobReader does not support concurrent Read and Close calls.
type blobReader struct {
	ctx    context.Context //nolint:containedctx // bounds chunk requests
	cancel context.CancelFunc
	ep     *endpoint
	url    string
	size   int64
	chunk  int64
	conc   int
	budget *budget

	body      io.Closer // streamed response body backing cur, if any
	cur       io.Reader // nil when the next chunk must be loaded
	curCharge int64
	ranged    bool
	next      int64 // offset of the next chunk to schedule
	pending   []pendingChunk
	wg        sync.WaitGroup
	err       error
	closed    bool
}

func newBlobReader(parent context.Context, ep *endpoint, desc ispec.Descriptor, off int64, p *Puller) (*blobReader, error) {
	ctx, cancel := context.WithCancel(auth.AppendRepositoryScope(parent, ep.ref, auth.ActionPull))
	b := &blobReader{
		ctx:    ctx,
		cancel: cancel,
		ep:     ep,
		url:    fmt.Sprintf("%s/v2/%s/blobs/%s", ep.baseURL, ep.ref.Repository, desc.Digest),
		size:   desc.Size,
		chunk:  p.chunkSize(),
		conc:   p.concurrency(),
		budget: p.state().budget,
	}

	if err := b.start(off); err != nil {
		cancel()
		return nil, err
	}

	return b, nil
}

func (b *blobReader) start(off int64) error {
	if off >= b.size {
		b.cur = &exactReader{}
		return nil
	}

	if b.conc > 1 && b.size-off > b.chunk {
		end := off + b.chunk - 1

		resp, err := b.getRetry(off, end)
		if err != nil {
			return err
		}

		switch {
		case resp.StatusCode == http.StatusPartialContent && contentRangeMatches(resp, off, end, b.size):
			b.ranged = true
			b.body = resp.Body
			b.cur = &exactReader{r: resp.Body, remaining: b.chunk}
			b.next = end + 1
			// Start prefetching while the first chunk is streamed.
			b.schedule()

			return nil
		case resp.StatusCode == http.StatusOK:
			// Range is not supported (or ignored, as Gantry does for
			// bounded ranges): stream the full body sequentially.
			return b.streamFull(resp, off)
		case resp.StatusCode == http.StatusPartialContent:
			// Unusable Content-Range: retry with an open-ended range below.
			_ = resp.Body.Close() //nolint:errcheck // discarding response
		default:
			return statusError(resp)
		}
	}

	start := int64(-1)
	if off > 0 {
		start = off
	}

	resp, err := b.getRetry(start, -1)
	if err != nil {
		return err
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		return b.streamFull(resp, off)
	case off > 0 && resp.StatusCode == http.StatusPartialContent && contentRangeMatches(resp, off, b.size-1, b.size):
		b.body = resp.Body
		b.cur = &exactReader{r: resp.Body, remaining: b.size - off}

		return nil
	default:
		return statusError(resp)
	}
}

// streamFull streams a 200 response carrying the whole blob, discarding the
// first off bytes.
func (b *blobReader) streamFull(resp *http.Response, off int64) error {
	if off > 0 {
		if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
			_ = resp.Body.Close() //nolint:errcheck // already failing
			return fmt.Errorf("skip to offset %d: %w", off, err)
		}
	}

	b.body = resp.Body
	b.cur = &exactReader{r: resp.Body, remaining: b.size - off}

	return nil
}

// getRetry issues a blob GET, retrying transport errors, 5xx and 429
// responses up to the endpoint's attempt count.
func (b *blobReader) getRetry(start, end int64) (*http.Response, error) {
	var lastErr error

	for attempt := range max(b.ep.attempts, 1) {
		if attempt > 0 {
			if err := b.backoff(attempt); err != nil {
				return nil, err
			}
		}

		resp, err := b.get(start, end)
		if err != nil {
			if b.ctx.Err() != nil {
				return nil, b.ctx.Err()
			}

			lastErr = err

			continue
		}

		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			lastErr = statusError(resp)
			continue
		}

		return resp, nil
	}

	return nil, lastErr
}

func (b *blobReader) backoff(attempt int) error {
	select {
	case <-b.ctx.Done():
		return b.ctx.Err()
	case <-time.After(chunkRetryBackoff * time.Duration(attempt)):
		return nil
	}
}

// get issues a blob GET. A negative start omits the Range header; a
// negative end requests an open-ended range.
func (b *blobReader) get(start, end int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(b.ctx, http.MethodGet, b.url, nil)
	if err != nil {
		return nil, err
	}

	switch {
	case start >= 0 && end >= 0:
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	case start >= 0:
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
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
		if b.cur == nil {
			if err := b.advance(); err != nil {
				b.err = err
				return 0, err
			}
		}

		n, err := b.cur.Read(p)
		if errors.Is(err, io.EOF) {
			// Current chunk or stream is exhausted; release it now.
			b.releaseCur()

			if !b.ranged {
				b.err = io.EOF
				return n, io.EOF
			}

			err = nil
		}

		if err != nil {
			b.err = err
			return n, err
		}

		if n > 0 {
			return n, nil
		}
	}
}

// releaseCur closes the response body backing the current chunk and
// returns its budget.
func (b *blobReader) releaseCur() {
	if b.body != nil {
		_ = b.body.Close() //nolint:errcheck // exhausted body
		b.body = nil
	}

	b.budget.release(b.curCharge)
	b.curCharge = 0
	b.cur = nil
}

// advance loads the next chunk in ranged mode.
func (b *blobReader) advance() error {
	if !b.ranged {
		return io.EOF
	}

	b.schedule()

	if len(b.pending) == 0 {
		if b.next >= b.size {
			return io.EOF
		}

		// No budget for prefetching: stream the next chunk directly so
		// that the reader always makes progress.
		start := b.next
		end := min(start+b.chunk, b.size) - 1

		body, err := b.streamChunk(start, end)
		if err != nil {
			return err
		}

		b.next = end + 1
		b.body = body
		b.cur = &exactReader{r: body, remaining: end - start + 1}

		return nil
	}

	pc := b.pending[0]
	b.pending = b.pending[1:]

	var res chunkResult
	select {
	case res = <-pc.ch:
	case <-b.ctx.Done():
		b.budget.release(pc.charge)
		return b.ctx.Err()
	}

	if res.err != nil {
		b.budget.release(pc.charge)
		return res.err
	}

	b.cur = bytes.NewReader(res.data)
	b.curCharge = pc.charge
	b.schedule()

	return nil
}

// schedule starts chunk prefetches until conc requests are in flight (the
// streamed current chunk counts as one), the blob is fully scheduled, or
// the Puller memory budget is exhausted.
func (b *blobReader) schedule() {
	inflight := len(b.pending)
	if b.body != nil {
		inflight++
	}

	for inflight < b.conc && b.next < b.size {
		start := b.next
		end := min(start+b.chunk, b.size) - 1
		n := end - start + 1

		if !b.budget.tryAcquire(n) {
			return
		}

		b.next = end + 1
		inflight++

		ch := make(chan chunkResult, 1)
		b.pending = append(b.pending, pendingChunk{ch: ch, charge: n})

		b.wg.Add(1)

		go func() {
			defer b.wg.Done()

			data, err := b.fetchChunk(start, end)
			ch <- chunkResult{data: data, err: err}
		}()
	}
}

// streamChunk opens a ranged response for one chunk without buffering it.
func (b *blobReader) streamChunk(start, end int64) (io.ReadCloser, error) {
	resp, err := b.getRetry(start, end)
	if err != nil {
		return nil, fmt.Errorf("fetch blob range %d-%d: %w", start, end, err)
	}

	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("fetch blob range %d-%d: %w", start, end, statusError(resp))
	}

	if !contentRangeMatches(resp, start, end, b.size) {
		_ = resp.Body.Close() //nolint:errcheck // discarding response

		return nil, fmt.Errorf("unexpected Content-Range %q for bytes %d-%d/%d",
			resp.Header.Get("Content-Range"), start, end, b.size)
	}

	return resp.Body, nil
}

func (b *blobReader) fetchChunk(start, end int64) ([]byte, error) {
	var lastErr error

	for attempt := range max(b.ep.attempts, 1) {
		if attempt > 0 {
			if err := b.backoff(attempt); err != nil {
				return nil, err
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

// Close cancels in-flight requests, waits for them to finish, and returns
// the budget held by buffered chunks.
func (b *blobReader) Close() error {
	if b.closed {
		return nil
	}

	b.closed = true
	b.err = errBlobReaderClosed
	b.cancel()
	b.wg.Wait()
	b.releaseCur()

	for _, pc := range b.pending {
		b.budget.release(pc.charge)
	}

	b.pending = nil

	return nil
}

// exactReader reads exactly remaining bytes, reporting a short body as
// io.ErrUnexpectedEOF.
type exactReader struct {
	r         io.Reader
	remaining int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.remaining <= 0 || e.r == nil {
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
	// onMismatch, when set, is called once on a digest mismatch.
	onMismatch func()
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

	if err == io.EOF { //nolint:errorlint // wrapped EOFs are transport errors
		switch {
		case v.n != v.desc.Size:
			v.err = fmt.Errorf("blob %s: got %d bytes, want %d: %w", v.desc.Digest, v.n, v.desc.Size, io.ErrUnexpectedEOF)
		case !v.verifier.Verified():
			v.err = fmt.Errorf("blob %s: %w", v.desc.Digest, ErrDigestMismatch)

			if v.onMismatch != nil {
				v.onMismatch()
			}
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

// ErrDigestMismatch reports that content does not match its digest: a blob
// read to EOF, or a file read sequentially from offset 0 to EOF.
var ErrDigestMismatch = errors.New("blob digest mismatch")

// budget is a Puller-wide limit on bytes held in prefetched chunks.
type budget struct {
	mu   sync.Mutex
	max  int64
	used int64
	peak int64
}

func newBudget(maxBytes int64) *budget { return &budget{max: maxBytes} }

func (b *budget) tryAcquire(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.used+n > b.max {
		return false
	}

	b.used += n
	b.peak = max(b.peak, b.used)

	return true
}

func (b *budget) release(n int64) {
	if n == 0 {
		return
	}

	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

func (b *budget) stats() (used, peak int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.used, b.peak
}
