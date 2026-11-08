// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
)

// MaxTagManifestSize bounds a manifest fetched by tag. OCI registries
// commonly cap manifests at 4MiB.
const MaxTagManifestSize = 4 << 20

// ManifestByTag fetches the manifest a tag currently points to. It returns
// the manifest bytes, the upstream Content-Type, and the digest of the bytes.
// The digest is computed locally; when the registry sends
// Docker-Content-Digest, a different value is an error. The caller's context
// carries delegated credentials like Pull.
func (c *Client) ManifestByTag(ctx context.Context, registryName, repo, tag string) ([]byte, string, digest.Digest, error) {
	ref := ifaces.OriginRef{Registry: registryName, Repository: repo, Kind: ifaces.KindManifest}

	if err := oci.ValidateRepositoryName(repo); err != nil {
		return nil, "", digest.Digest{}, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: err}
	}

	if tag == "" || strings.ContainsAny(tag, "/:@") {
		return nil, "", digest.Digest{}, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: fmt.Errorf("invalid tag %q", tag)}
	}

	r, ok := c.registries[registryName]
	if !ok {
		return nil, "", digest.Digest{}, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureNotFound, Err: fmt.Errorf("unknown registry %q", registryName)}
	}

	u := r.urlWithPath("/v2/" + repo + "/manifests/" + url.PathEscape(tag))

	resp, err := r.doRange(ctx, http.MethodGet, u.String(), "")
	if err != nil {
		return nil, "", digest.Digest{}, &ifaces.OriginError{Ref: ref, Class: classOf(err), StatusCode: statusOf(err), Err: err}
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort body close

	if resp.StatusCode != http.StatusOK {
		return nil, "", digest.Digest{}, r.classify(ref, resp)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxTagManifestSize+1))
	if err != nil {
		return nil, "", digest.Digest{}, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureTransient, Err: err}
	}

	if len(body) > MaxTagManifestSize {
		return nil, "", digest.Digest{}, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureTransient, Err: errors.New("manifest exceeds 4MiB")}
	}

	sum := sha256.Sum256(body)

	d, err := digest.Parse("sha256:" + hex.EncodeToString(sum[:]))
	if err != nil {
		return nil, "", digest.Digest{}, err
	}

	if got := resp.Header.Get("Docker-Content-Digest"); got != "" && got != d.String() {
		return nil, "", digest.Digest{}, &ifaces.OriginError{Ref: ref, Class: ifaces.FailureTransient, Err: fmt.Errorf("registry digest %s does not match manifest bytes %s", got, d)}
	}

	return body, resp.Header.Get("Content-Type"), d, nil
}
