// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/digestpipe"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// RacerClient is satisfied by the SDK client, including its protocol fake.
type RacerClient interface {
	Get(context.Context, racersdk.Request) (*racersdk.Value, error)
}

// WithRacer selects the exclusive Racer content path. The origin passed to New
// remains available only for authentication challenges, never content fallback.
func WithRacer(client RacerClient) Option {
	return func(s *Server) { s.racer = client }
}

func (s *Server) serveRacer(w http.ResponseWriter, r *http.Request, upstream, repo string, d digest.Digest, kind ifaces.OriginRefKind) {
	if s.racer == nil {
		http.Error(w, "Racer unavailable", http.StatusServiceUnavailable)
		return
	}

	request, err := gantryracer.Request(ifaces.OriginRef{Registry: upstream, Repository: repo, Digest: d, Kind: kind}, registryauth.Authorization(r.Context()))
	if err != nil {
		http.Error(w, "invalid Racer request", http.StatusBadRequest)
		return
	}

	value, err := s.racer.Get(r.Context(), request)
	if err != nil {
		var sdkErr *racersdk.Error
		if s.auth != nil && errors.As(err, &sdkErr) && sdkErr.Kind() == racersdk.ErrorUnauthorized {
			// A same-node origin callback may have remembered a validated
			// repository challenge. The registry-level API cannot discover a
			// remote repository challenge when its /v2/ endpoint is public.
			challengeCtx, cancel := context.WithTimeout(r.Context(), authenticationChallengeTimeout)
			challenge, required, challengeErr := s.auth.AuthenticationChallenge(challengeCtx, upstream)

			cancel()

			if challengeErr == nil && required && challenge != "" {
				w.Header().Set("WWW-Authenticate", challenge)
			}
		}

		writeRacerError(w, err)

		return
	}

	defer func() { _ = value.Close() }() //nolint:errcheck // best-effort close

	metadata := value.Metadata()
	if metadata.ETag.String() != `"`+d.String()+`"` {
		http.Error(w, "invalid Racer version", http.StatusBadGateway)
		return
	}

	size := int64(metadata.Size)
	reader := bufio.NewReader(value)

	prefix, err := reader.Peek(int(min(size, 512)))
	if err != nil {
		writeRacerError(w, err)
		return
	}

	// The SDK has no HEAD operation and carries no media type. Inspecting the
	// bootstrap prefix preserves the OCI descriptor type, including indexes.
	if r.Method == http.MethodHead {
		writeBlobHeadersWithPrefix(w, d, size, kind, prefix)
		w.Header().Set("Gantry-Mirrored", "1")
		w.WriteHeader(http.StatusOK)

		return
	}

	prefix = bytes.Clone(prefix)

	// Hash skipped bytes too, so a resumed response still verifies the entire
	// Racer value. No direct-origin request can be reached from this handler.
	verifier := digestpipe.New(io.Discard)
	source := io.TeeReader(reader, verifier)
	offset, ranged := parseOriginRetryRange(r.Header.Get("Range"))

	ranged = ranged && kind == ifaces.KindBlob
	if ranged {
		if offset >= size {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)

			return
		}

		if _, err := io.CopyN(io.Discard, source, offset); err != nil {
			writeRacerError(w, err)
			return
		}
	}

	writeBlobHeadersWithPrefix(w, d, size, kind, prefix)
	w.Header().Set("Gantry-Mirrored", "1")

	if ranged {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, size-1, size))
		w.Header().Set("Content-Length", strconv.FormatInt(size-offset, 10))
		w.WriteHeader(http.StatusPartialContent)
	}

	remaining := size
	if ranged {
		remaining -= offset
	}
	// Hold the final bounded chunk until both framing and digest verification
	// succeed. A bad digest must not look like a complete Content-Length body.
	if _, err := io.CopyN(w, source, max(0, remaining-32*1024)); err != nil {
		panic(http.ErrAbortHandler)
	}

	last := make([]byte, min(remaining, 32*1024)+1)

	n, err := io.ReadFull(source, last[:len(last)-1])
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	// Probe separately so an upstream UnexpectedEOF cannot pass as clean EOF.
	if _, err := io.ReadFull(source, last[n:]); err != io.EOF {
		panic(http.ErrAbortHandler)
	}

	if err := verifier.Verify(d); err != nil {
		panic(http.ErrAbortHandler)
	}

	if _, err := w.Write(last[:n]); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func writeRacerError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway

	var sdkErr *racersdk.Error
	if errors.As(err, &sdkErr) {
		switch sdkErr.Kind() {
		case racersdk.ErrorNotFound:
			status = http.StatusNotFound
		case racersdk.ErrorUnauthorized:
			status = http.StatusUnauthorized
		case racersdk.ErrorForbidden:
			status = http.StatusForbidden
		case racersdk.ErrorUnavailable, racersdk.ErrorClosed, racersdk.ErrorCanceled, racersdk.ErrorDeadline, racersdk.ErrorIO:
			status = http.StatusServiceUnavailable
		}
	}

	http.Error(w, "Racer request failed", status)
}
