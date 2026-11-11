// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/pkg/racersdk"
)

const (
	ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	ociIndexMediaType    = "application/vnd.oci.image.index.v1+json"
)

// errUnknownManifestType reports a manifest body whose media type cannot be
// determined.
var errUnknownManifestType = errors.New("unknown manifest media type")

// serveManifest answers GET and HEAD on the manifest route. Racer stores every
// object as application/octet-stream (see objectContentType), so the media
// type comes from the manifest itself. Manifests fit in one page, so the body
// is buffered and digest-checked before any header is written; a corrupt or
// untypeable manifest therefore gets a clean error status instead of an
// aborted response. HEAD reads the body too, because containerd resolves
// references with HEAD and needs the media type.
func (s *Handler) serveManifest(w http.ResponseWriter, r *http.Request, id string, ref ifaces.OriginRef, request racersdk.Request, retry *retryState) {
	ctx := r.Context()

	var (
		metadata racersdk.Metadata
		body     bytes.Buffer
	)

	for {
		value, err := s.getWithRetry(ctx, retry, request, racersdk.ReadOptions{SmallObject: true})
		if err != nil {
			s.logRacerFailure(id, "get", err, -1, 0)
			s.racerError(w, r, ref, err)

			return
		}

		metadata = value.Metadata()
		if !validRacerMetadata(metadata, ref.Digest) {
			_ = value.Close() //nolint:errcheck // best-effort close on invalid metadata

			s.logRacerFailure(id, "get_metadata", errors.New("invalid metadata"), -1, 0)
			http.Error(w, "invalid Racer metadata", http.StatusBadGateway)

			return
		}

		if metadata.Size > racersdk.PageSize {
			_ = value.Close() //nolint:errcheck // best-effort close on an oversized manifest

			s.logRacerFailure(id, "manifest_size", errors.New("manifest too large"), int64(metadata.Size), 0)
			http.Error(w, "Racer manifest too large", http.StatusBadGateway)

			return
		}

		body.Reset()
		n, err := value.WriteTo(&body)
		_ = value.Close() //nolint:errcheck // the body is fully buffered or abandoned

		if err == nil && n == int64(metadata.Size) {
			break
		}

		if err == nil {
			err = errors.New("short manifest read")
		}

		if !retryable(ctx, err) || !retry.wait(ctx, "write_body") {
			s.logRacerFailure(id, "read_manifest", err, int64(metadata.Size), n)
			writeRacerError(w, err)

			return
		}
	}

	sum := sha256.Sum256(body.Bytes())
	if hex.EncodeToString(sum[:]) != ref.Digest.Hex() {
		s.digestMismatch(ctx, id, request, metadata, int64(metadata.Size), 0)
		http.Error(w, "Racer manifest failed digest verification", http.StatusBadGateway)

		return
	}

	mediaType, err := manifestBodyMediaType(body.Bytes())
	if err != nil {
		// The registry's own Content-Type is the authority for a manifest Gantry
		// cannot type; 503 makes containerd fall through to it.
		s.logRacerFailure(id, "manifest_media_type", err, int64(metadata.Size), 0)
		http.Error(w, "unknown manifest media type", http.StatusServiceUnavailable)

		return
	}

	metadata.ContentType = mediaType
	writeRacerHeaders(w, ref.Digest, metadata)
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodHead {
		return
	}

	if _, err := w.Write(body.Bytes()); err != nil {
		s.logRacerFailure(id, "write_body", err, int64(body.Len()), 0)
	}
}

// manifestBodyMediaType reports the media type of a manifest from its body:
// the mediaType field when present, otherwise an OCI index or manifest
// inferred from its structure. Bodies that are not a JSON object, or whose
// type cannot be determined (for example a Docker schema 1 manifest), are an
// error.
func manifestBodyMediaType(body []byte) (string, error) {
	var manifest struct {
		MediaType string          `json:"mediaType"`
		Manifests json.RawMessage `json:"manifests"`
		Config    json.RawMessage `json:"config"`
		Layers    json.RawMessage `json:"layers"`
	}

	if err := json.Unmarshal(body, &manifest); err != nil {
		return "", errors.Join(errUnknownManifestType, err)
	}

	if mediaType := manifestMediaType(manifest.MediaType); mediaType != "" {
		return mediaType, nil
	}

	switch {
	case present(manifest.Manifests):
		return ociIndexMediaType, nil
	case present(manifest.Config) || present(manifest.Layers):
		return ociManifestMediaType, nil
	}

	return "", errUnknownManifestType
}

// present reports whether a JSON field was set to a non-null value.
func present(field json.RawMessage) bool {
	return len(field) != 0 && !bytes.Equal(field, []byte("null"))
}
