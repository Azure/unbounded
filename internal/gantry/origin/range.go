// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
)

var _ ifaces.OriginRangePuller = (*Client)(nil)

// HeadMetadata fetches the exact identity representation size and media type.
// It resolves the legacy blob-to-manifest fallback using HEAD only. The returned
// Ref must be passed to OpenRange so each range needs only one data GET.
func (c *Client) HeadMetadata(ctx context.Context, ref ifaces.OriginRef) (ifaces.OriginMetadata, error) {
	r, err := c.rangeRegistry(ref)
	if err != nil {
		return ifaces.OriginMetadata{}, err
	}

	ref.Offset = 0
	headers := http.Header{"Accept-Encoding": {"identity"}}

	resp, err := r.doWithHeaders(ctx, http.MethodHead, r.urlFor(ref), headers)
	if err != nil {
		return ifaces.OriginMetadata{}, rangeOriginError(ref, err)
	}

	if resp.StatusCode == http.StatusNotFound && ref.Kind != ifaces.KindManifest {
		_ = resp.Body.Close() //nolint:errcheck // Release the blob response before manifest fallback.
		ref.Kind = ifaces.KindManifest

		resp, err = r.doWithHeaders(ctx, http.MethodHead, r.urlFor(ref), headers)
		if err != nil {
			return ifaces.OriginMetadata{}, rangeOriginError(ref, err)
		}
	}
	defer resp.Body.Close() //nolint:errcheck // HEAD response cleanup.

	if resp.StatusCode != http.StatusOK {
		failure := r.classify(ref, resp)
		if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
			failure.Err = &ifaces.OriginMetadataUnavailableError{Reason: "HEAD is unsupported"}
		}

		return ifaces.OriginMetadata{}, failure
	}

	if err := identityResponse(resp); err != nil {
		return ifaces.OriginMetadata{}, rangeOriginError(ref, err)
	}

	size, err := decimal(resp.Header.Get("Content-Length"))
	if err != nil {
		return ifaces.OriginMetadata{}, rangeOriginError(ref, &ifaces.OriginMetadataUnavailableError{Reason: "missing or invalid Content-Length"})
	}

	return ifaces.OriginMetadata{Ref: ref, Size: size, ContentType: resp.Header.Get("Content-Type")}, nil
}

// OpenRange opens exactly [offset, offset+length) of the expected fullSize.
// ref.Offset is ignored; the explicit bounds are authoritative. Empty ranges
// and ranges outside fullSize are rejected before any network request. It never
// probes with HEAD, falls back to another URL kind, buffers the object, or skips
// a prefix. OCI digests are not upstream HTTP ETags: no If-Match is sent.
//
// The caller must read to EOF and close the body. The stream validates its final
// boundary before returning the last bytes, reports truncated/oversized bodies,
// and never exposes bytes beyond length. Close does not drain or validate an
// abandoned stream. A 200 response is accepted only for the whole object.
func (c *Client) OpenRange(ctx context.Context, ref ifaces.OriginRef, offset, length, fullSize int64) (io.ReadCloser, error) {
	kind := ref.Kind.MetricLabel()
	if c.metrics.onPullStart != nil {
		c.metrics.onPullStart(kind)
	}

	body, err := c.openRange(ctx, ref, offset, length, fullSize)
	if err != nil {
		c.recordFailure(kind, err)
	}

	return body, err
}

func (c *Client) openRange(ctx context.Context, ref ifaces.OriginRef, offset, length, fullSize int64) (io.ReadCloser, error) {
	if offset < 0 || length <= 0 || fullSize < 0 || offset > fullSize || length > fullSize-offset {
		return nil, rangeOriginError(ref, errors.New("invalid bounded range"))
	}

	r, err := c.rangeRegistry(ref)
	if err != nil {
		return nil, err
	}

	headers := http.Header{
		"Accept-Encoding": {"identity"},
		"Range":           {fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)},
	}

	resp, err := r.doWithHeaders(ctx, http.MethodGet, r.urlFor(ref), headers)
	if err != nil {
		return nil, rangeOriginError(ref, err)
	}

	if err := validateRangeResponse(resp, offset, length, fullSize); err != nil {
		_ = resp.Body.Close() //nolint:errcheck // Release rejected range response.
		failure := r.classify(ref, resp)
		// Preserve HTTP failures (especially auth challenges and rate limits).
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent || resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			failure.Err = err
		}

		return nil, failure
	}

	body := resp.Body
	if c.metrics.onBytesRead != nil {
		body = &countingReadCloser{ReadCloser: body, onFinish: func(n int64) {
			c.metrics.onBytesRead(ref.Kind.MetricLabel(), n)
		}}
	}

	return &rangeReadCloser{ReadCloser: body, ref: ref, remaining: length}, nil
}

func (c *Client) rangeRegistry(ref ifaces.OriginRef) (*registry, error) {
	if err := oci.ValidateRepositoryName(ref.Repository); err != nil {
		return nil, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: err}
	}

	if ref.Digest.IsZero() {
		return nil, rangeOriginError(ref, errors.New("missing OCI digest"))
	}

	r, ok := c.registries[ref.Registry]
	if !ok {
		return nil, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: fmt.Errorf("unknown registry %q", ref.Registry)}
	}

	return r, nil
}

func validateRangeResponse(resp *http.Response, offset, length, fullSize int64) error {
	switch resp.StatusCode {
	case http.StatusOK:
		if offset != 0 || length != fullSize {
			return &ifaces.OriginRangeUnsupportedError{Reason: "upstream ignored Range"}
		}

		if resp.Header.Get("Content-Range") != "" {
			return errors.New("unexpected Content-Range on 200 response")
		}
	case http.StatusPartialContent:
		value := resp.Header.Get("Content-Range")
		if len(resp.Header.Values("Content-Range")) != 1 {
			return errors.New("expected exactly one Content-Range")
		}

		bounds, total, ok := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
		start, end, bounded := strings.Cut(bounds, "-")
		a, aErr := decimal(start)
		b, bErr := decimal(end)

		n, nErr := decimal(total)
		if !strings.HasPrefix(value, "bytes ") || !ok || !bounded || aErr != nil || bErr != nil || nErr != nil || a != offset || b != offset+length-1 || n != fullSize {
			return fmt.Errorf("invalid Content-Range %q", value)
		}
	case http.StatusRequestedRangeNotSatisfiable:
		return &ifaces.OriginRangeUnsupportedError{Reason: "upstream rejected Range"}
	default:
		return fmt.Errorf("unexpected range status %d", resp.StatusCode)
	}

	if err := identityResponse(resp); err != nil {
		return err
	}

	if cl := resp.Header.Get("Content-Length"); cl != "" {
		n, err := decimal(cl)
		if err != nil || n != length {
			return errors.New("range Content-Length does not match requested length")
		}
	}

	return nil
}

func identityResponse(resp *http.Response) error {
	encodings := resp.Header.Values("Content-Encoding")
	if resp.Uncompressed || len(encodings) > 1 || (len(encodings) == 1 && encodings[0] != "" && !strings.EqualFold(encodings[0], "identity")) {
		return errors.New("upstream returned an encoded representation")
	}

	return nil
}

func decimal(s string) (int64, error) {
	if s == "" || strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, errors.New("expected nonnegative decimal")
	}

	return strconv.ParseInt(s, 10, 64)
}

type rangeReadCloser struct {
	io.ReadCloser
	ref       ifaces.OriginRef
	remaining int64
	terminal  error
}

func (r *rangeReadCloser) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if r.terminal != nil {
		return 0, r.terminal
	}

	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}

	n, err := r.ReadCloser.Read(p)

	r.remaining -= int64(n)
	if err == io.EOF && r.remaining != 0 {
		err = io.ErrUnexpectedEOF
	}

	if r.remaining == 0 && err == nil {
		// A one-byte probe detects surplus data even with chunked framing. Do
		// this before returning the final bytes, including to ReadFull callers.
		var (
			probe [1]byte
			extra int
		)

		extra, err = io.ReadFull(r.ReadCloser, probe[:])
		if extra != 0 {
			err = errors.New("upstream range body exceeds requested length")
		}
	}

	if err != nil {
		if err != io.EOF {
			err = rangeOriginError(r.ref, err)
			// Do not let io.ReadFull hide a boundary failure after receiving
			// its requested count. Bytes from this failed read are withheld.
			if r.remaining == 0 {
				n = 0
			}
		}

		r.terminal = err
	}

	return n, err
}

func rangeOriginError(ref ifaces.OriginRef, err error) *ifaces.OriginError {
	var upstream *ifaces.OriginError
	if errors.As(err, &upstream) {
		copy := *upstream
		copy.Ref = ref

		return &copy
	}

	return &ifaces.OriginError{Ref: ref, Class: classOf(err), Err: err}
}

func setHTTPErrorDetails(oe *ifaces.OriginError, resp *http.Response) {
	oe.StatusCode = resp.StatusCode

	oe.RetryAfterHeader = resp.Header.Get("Retry-After")
	if seconds, err := decimal(oe.RetryAfterHeader); err == nil {
		const maxSeconds = int64((1<<63 - 1) / time.Second)

		oe.RetryAfter = time.Duration(min(seconds, maxSeconds)) * time.Second
	} else if date, err := http.ParseTime(oe.RetryAfterHeader); err == nil {
		oe.RetryAfter = max(time.Until(date), 0)
	}
}
