// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func (r *registry) pullRange(ctx context.Context, ref ifaces.OriginRef, length int64) (io.ReadCloser, int64, string, error) {
	end := ref.Offset + length - 1
	rangeValue := fmt.Sprintf("bytes=%d-%d", ref.Offset, end)

	resp, err := r.doRange(ctx, http.MethodGet, r.urlFor(ref), rangeValue)
	if err != nil {
		return nil, 0, "", &ifaces.OriginError{Ref: ref, Class: classOf(err), StatusCode: statusOf(err), Err: err}
	}
	// Unlike legacy open-ended Pull, bounded pages may discover the manifest
	// route at any offset. Keep the requested bounds and delegated identity.
	if resp.StatusCode == http.StatusNotFound && ref.Kind != ifaces.KindManifest {
		_ = resp.Body.Close() //nolint:errcheck // best-effort body close
		ref.Kind = ifaces.KindManifest

		resp, err = r.doRange(ctx, http.MethodGet, r.urlFor(ref), rangeValue)
		if err != nil {
			return nil, 0, "", &ifaces.OriginError{Ref: ref, Class: classOf(err), StatusCode: statusOf(err), Err: err}
		}
	}

	contentType := resp.Header.Get("Content-Type")
	if (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent) && resp.Header.Get("Docker-Content-Digest") != "" && resp.Header.Get("Docker-Content-Digest") != ref.Digest.String() {
		_ = resp.Body.Close() //nolint:errcheck // best-effort body close
		return nil, 0, "", rangeError(ref, "registry returned a different digest")
	}
	// A range cannot select bytes in an empty representation. Registries use
	// either 416 with bytes */0 or an explicitly empty 200 for this case.
	if ref.Offset == 0 && (resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && resp.Header.Get("Content-Range") == "bytes */0" ||
		resp.StatusCode == http.StatusOK && resp.ContentLength == 0) {
		_ = resp.Body.Close() //nolint:errcheck // best-effort body close
		return io.NopCloser(strings.NewReader("")), 0, contentType, nil
	}
	// Distribution manifest handlers return a complete 200 even with Range.
	// Accept it only when the known entire representation fits this first page.
	// Preserve HTTP framing and the SDK's exact-byte check: limiting the reader
	// here could disguise truncation or an incorrectly sized upstream response.
	if resp.StatusCode == http.StatusOK && ref.Offset == 0 && resp.ContentLength > 0 && resp.ContentLength <= length {
		return resp.Body, resp.ContentLength, contentType, nil
	}

	if resp.StatusCode != http.StatusPartialContent {
		defer resp.Body.Close() //nolint:errcheck // best-effort body close

		if resp.StatusCode != http.StatusOK {
			return nil, 0, "", r.classify(ref, resp)
		}

		return nil, 0, "", rangeError(ref, "registry ignored bounded Range")
	}

	start, gotEnd, size, ok := parseOriginContentRange(resp.Header.Get("Content-Range"))
	if !ok || start != ref.Offset || gotEnd != min(end, size-1) ||
		(resp.ContentLength >= 0 && resp.ContentLength != gotEnd-start+1) {
		_ = resp.Body.Close() //nolint:errcheck // best-effort body close
		return nil, 0, "", rangeError(ref, "invalid bounded Content-Range")
	}

	return resp.Body, size, contentType, nil
}

func rangeError(ref ifaces.OriginRef, reason string) error {
	return &ifaces.OriginError{Ref: ref, Class: ifaces.FailureTransient, Err: &ifaces.ErrRangeUnsupported{Offset: ref.Offset, Reason: reason}}
}
