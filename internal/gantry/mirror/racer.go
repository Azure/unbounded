// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
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
	Get(context.Context, racersdk.Request, ...racersdk.ReadOptions) (*racersdk.Value, error)
	Stat(context.Context, racersdk.Request) (racersdk.Metadata, error)
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

	offset, ranged := parseOriginRetryRange(r.Header.Get("Range"))
	ranged = ranged && kind == ifaces.KindBlob

	var (
		metadata racersdk.Metadata
		options  []racersdk.ReadOptions
	)

	if r.Method == http.MethodHead || ranged {
		metadata, err = s.racer.Stat(r.Context(), request)
		if err != nil {
			s.racerError(w, r, upstream, err)
			return
		}

		if !validRacerMetadata(metadata, d) {
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

		options = []racersdk.ReadOptions{{Offset: racersdk.ByteOffset(offset), Pin: metadata.ETag, Metadata: &metadata}}
	}

	if kind == ifaces.KindManifest {
		options = []racersdk.ReadOptions{{SmallObject: true}}
	}

	value, err := s.racer.Get(r.Context(), request, options...)
	if err != nil {
		s.racerError(w, r, upstream, err)
		return
	}

	defer func() { _ = value.Close() }() //nolint:errcheck // best-effort close

	actual := value.Metadata()
	if !validRacerMetadata(actual, d) || (ranged && (actual.Size != metadata.Size ||
		(actual.ContentType != "" && metadata.ContentType != "" && actual.ContentType != metadata.ContentType))) {
		http.Error(w, "invalid Racer metadata", http.StatusBadGateway)
		return
	}

	if kind == ifaces.KindManifest && actual.Size > racersdk.PageSize {
		http.Error(w, "Racer manifest too large", http.StatusBadGateway)
		return
	}

	if ranged {
		// Optional media type may be absent on a legacy peer. Keep the selected
		// snapshot rather than changing headers as later metadata arrives.
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
	// Read through SDK EOF, including its final framing check. A LimitedReader
	// would hide a truncated terminator after the advertised payload. OCI digest
	// verification belongs to the consumer, including resumed object assembly.
	if n, err := value.WriteToHTTP(w); err != nil || n != remaining {
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

func (s *Server) racerError(w http.ResponseWriter, r *http.Request, upstream string, err error) {
	var sdkErr *racersdk.Error
	if s.auth != nil && errors.As(err, &sdkErr) && sdkErr.Kind() == racersdk.ErrorUnauthorized {
		// A same-node origin callback may have remembered a validated repository
		// challenge even when the registry's /v2/ endpoint is public.
		challengeCtx, cancel := context.WithTimeout(r.Context(), authenticationChallengeTimeout)
		challenge, required, challengeErr := s.auth.AuthenticationChallenge(challengeCtx, upstream)

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
