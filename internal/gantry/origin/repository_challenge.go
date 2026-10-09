// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
)

// RepositoryAuthenticationChallenge recovers a challenge after a remote origin
// rejects a request. The registry root can be public, and another node's cached
// challenge is not available here. Probe the caller's resource anonymously, with
// one manifest HEAD fallback on a blob 404 for the same repository and digest.
// Never follow redirects, exchange tokens, fetch content, or cache the challenge.
func (c *Client) RepositoryAuthenticationChallenge(ctx context.Context, ref ifaces.OriginRef) (string, bool, error) {
	if err := oci.ValidateRepositoryName(ref.Repository); err != nil {
		return "", false, err
	}

	if ref.Digest.IsZero() {
		return "", false, errors.New("authentication probe requires a digest")
	}

	resource := "blobs"

	switch ref.Kind {
	case ifaces.KindManifest:
		resource = "manifests"
	case ifaces.KindBlob, ifaces.KindConfig:
	default:
		return "", false, errors.New("authentication probe has invalid resource kind")
	}

	r, ok := c.registries[ref.Registry]
	if !ok {
		return "", false, fmt.Errorf("origin: unknown registry %q", ref.Registry)
	}

	if !strings.EqualFold(r.base.Scheme, "https") {
		return "", false, errors.New("authentication challenge requires an HTTPS registry endpoint")
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// Reuse configured transport/TLS trust, not the auth-bearing request path.
	// A shallow client copy keeps redirect policy and cookies local to this probe.
	hc := *r.hc
	hc.Jar = nil
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	for {
		u := r.urlWithPath("/v2/" + ref.Repository + "/" + resource + "/" + ref.Digest.String())
		u.User = nil

		req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil)
		if err != nil {
			return "", false, err
		}

		if resource == "manifests" {
			req.Header.Set("Accept", manifestAccept)
		}

		resp, err := hc.Do(req)
		if err != nil {
			return "", false, fmt.Errorf("probe repository authentication: %w", err)
		}

		_ = resp.Body.Close() //nolint:errcheck // HEAD has no content to consume.

		if resp.StatusCode == http.StatusNotFound && resource == "blobs" {
			resource = "manifests"
			continue
		}

		switch resp.StatusCode {
		case http.StatusOK:
			return "", false, nil
		case http.StatusUnauthorized:
			challenge, err := validatedAuthenticationChallenge(resp)
			return challenge, err == nil, err
		default:
			return "", false, fmt.Errorf("repository authentication probe returned HTTP %d", resp.StatusCode)
		}
	}
}
