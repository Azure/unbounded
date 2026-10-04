// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// RacerClient is satisfied by the SDK client, including its protocol fake.
type RacerClient interface {
	GetStreaming(context.Context, racersdk.Request, ...racersdk.ReadOptions) (*racersdk.Value, error)
	Stat(context.Context, racersdk.Request) (racersdk.Metadata, error)
}

// RepositoryAuthenticationChallenger recovers an origin challenge for the exact
// requested resource without fetching content or forwarding caller credentials.
type RepositoryAuthenticationChallenger interface {
	RepositoryAuthenticationChallenge(context.Context, ifaces.OriginRef) (string, bool, error)
}

// WithRacer selects the exclusive Racer content path. The origin passed to New
// remains available only for authentication challenges, never content fallback.
func WithRacer(client RacerClient) Option {
	return func(s *Server) { s.racer = client }
}

func (s *Server) serveRacer(w http.ResponseWriter, r *http.Request, upstream, repo string, d digest.Digest, kind ifaces.OriginRefKind) {
	// Fresh opaque correlation, including for aborts after headers are committed.
	id := rand.Text()
	w.Header().Set("Gantry-Racer-Request-ID", id)

	if s.racer == nil {
		s.logRacerFailure(id, "client", errors.New("unavailable"), -1, 0)
		http.Error(w, "Racer unavailable", http.StatusServiceUnavailable)

		return
	}

	ref := ifaces.OriginRef{Registry: upstream, Repository: repo, Digest: d, Kind: kind}

	request, err := gantryracer.Request(ref, registryauth.Authorization(r.Context()))
	if err != nil {
		s.logRacerFailure(id, "request", err, -1, 0)
		http.Error(w, "invalid Racer request", http.StatusBadRequest)

		return
	}

	offset, ranged := parseOriginRetryRange(r.Header.Get("Range"))
	ranged = ranged && kind == ifaces.KindBlob

	var (
		metadata racersdk.Metadata
		options  []racersdk.ReadOptions
	)

	if r.Method == http.MethodHead || ranged {
		metadata, err = s.racer.Stat(r.Context(), request)
		if err != nil {
			s.logRacerFailure(id, "stat", err, -1, 0)
			s.racerError(w, r, ref, err)

			return
		}

		if !validRacerMetadata(metadata, d) {
			s.logRacerFailure(id, "stat_metadata", errors.New("invalid metadata"), -1, 0)
			http.Error(w, "invalid Racer metadata", http.StatusBadGateway)

			return
		}

		if r.Method == http.MethodHead {
			writeRacerHeaders(w, d, metadata, kind)
			w.WriteHeader(http.StatusOK)

			return
		}

		if offset >= int64(metadata.Size) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", metadata.Size))
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)

			return
		}

		// Pass the version pin, not Metadata: the SDK preserves a supplied
		// snapshot even when the peer omits or adds an optional MIME type.
		// Gantry must compare the returned metadata exactly before serving it.
		options = []racersdk.ReadOptions{{Offset: racersdk.ByteOffset(offset), Pin: metadata.ETag, SmallObject: metadata.Size <= racersdk.PageSize}}
	}

	if kind == ifaces.KindManifest {
		options = []racersdk.ReadOptions{{SmallObject: true}}
	}

	value, err := s.racer.GetStreaming(r.Context(), request, options...)
	if err != nil {
		expected := int64(-1)
		if ranged {
			expected = int64(metadata.Size) - offset
		}

		s.logRacerFailure(id, "get_streaming", err, expected, 0)
		s.racerError(w, r, ref, err)

		return
	}

	defer func() { _ = value.Close() }() //nolint:errcheck // best-effort close

	actual := value.Metadata()
	if !validRacerMetadata(actual, d) || (ranged && (actual.Size != metadata.Size ||
		actual.ContentType != metadata.ContentType)) {
		s.logRacerFailure(id, "get_metadata", errors.New("invalid metadata"), -1, 0)
		http.Error(w, "invalid Racer metadata", http.StatusBadGateway)

		return
	}

	if kind == ifaces.KindManifest && actual.Size > racersdk.PageSize {
		s.logRacerFailure(id, "manifest_size", errors.New("manifest too large"), int64(actual.Size), 0)
		http.Error(w, "Racer manifest too large", http.StatusBadGateway)

		return
	}

	if ranged {
		// The returned size, digest, and MIME type match the selected snapshot.
		actual = metadata
	}

	size := int64(actual.Size)
	writeRacerHeaders(w, d, actual, kind)

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
	// Publish nonempty response headers without waiting for payload. Empty
	// responses cannot withhold a final byte, so validate Complete before flushing.
	if remaining > 0 {
		if err := http.NewResponseController(w).Flush(); err != nil {
			s.logRacerFailure(id, "flush_headers", err, remaining, 0)
			panic(http.ErrAbortHandler)
		}
	}
	// The SDK gates the final byte on Complete, including for resumed ranges.
	// Earlier incomplete page prefixes may already be visible on failure: abort
	// rather than append an error body or let net/http complete the response.
	// OCI digest verification belongs to the consumer's assembled object.
	if n, err := value.WriteToHTTP(w); err != nil || n != remaining {
		s.logRacerFailure(id, "write_body", err, remaining, n)
		panic(http.ErrAbortHandler)
	}
}

func validRacerMetadata(metadata racersdk.Metadata, d digest.Digest) bool {
	return metadata.Validate() == nil && metadata.ETag.String() == `"`+d.String()+`"`
}

func writeRacerHeaders(w http.ResponseWriter, d digest.Digest, metadata racersdk.Metadata, kind ifaces.OriginRefKind) {
	w.Header().Set("Content-Type", metadata.ContentType)
	writeBlobHeaders(w, d, int64(metadata.Size), kind)
	w.Header().Set("Gantry-Mirrored", "1")
}

func (s *Server) racerError(w http.ResponseWriter, r *http.Request, ref ifaces.OriginRef, err error) {
	var sdkErr *racersdk.Error
	if s.auth != nil && errors.As(err, &sdkErr) && sdkErr.Kind() == racersdk.ErrorUnauthorized {
		// The rejecting origin callback may run on another node. Its challenge
		// cache is unavailable, and a public /v2/ root says nothing about this repo.
		challengeCtx, cancel := context.WithTimeout(r.Context(), authenticationChallengeTimeout)

		var (
			challenge    string
			required     bool
			challengeErr error
		)
		if auth, ok := s.auth.(RepositoryAuthenticationChallenger); ok {
			challenge, required, challengeErr = auth.RepositoryAuthenticationChallenge(challengeCtx, ref)
		} else {
			challenge, required, challengeErr = s.auth.AuthenticationChallenge(challengeCtx, ref.Registry)
		}

		cancel()

		if challengeErr == nil && required && challenge != "" {
			w.Header().Set("WWW-Authenticate", challenge)
		}
	}

	writeRacerError(w, err)
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
