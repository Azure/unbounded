// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/Azure/unbounded/internal/gantry/digest"
)

const (
	HandlerPrefix = "/blobs/"
	ReadinessPath = "/artifact-streaming/readyz"
)

var digestHexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// URLPolicy limits signed-origin requests to operator-approved HTTPS hosts.
type URLPolicy struct {
	AllowedHostSuffixes []string
	AllowHTTP           bool
}

// OriginURL is a validated origin capability extracted from an OverlayBD
// request. Raw is retained for the outbound request and must never be logged.
type OriginURL struct {
	Raw    string
	Digest digest.Digest
}

// OriginURLFromRequest reconstructs and validates the URL embedded by
// OverlayBD after /blobs/. RequestURI is used so signed query bytes are not
// decoded and re-encoded.
func OriginURLFromRequest(r *http.Request, policy URLPolicy) (OriginURL, error) {
	requestTarget := r.RequestURI
	if requestTarget == "" {
		requestTarget = r.URL.RequestURI()
	}

	if !strings.HasPrefix(requestTarget, HandlerPrefix) {
		return OriginURL{}, fmt.Errorf("streaming origin: request target does not start with %q", HandlerPrefix)
	}

	return ParseOriginURL(strings.TrimPrefix(requestTarget, HandlerPrefix), policy)
}

// ParseOriginURL validates a raw absolute origin URL and extracts its blob
// digest without rewriting the signed query.
func ParseOriginURL(raw string, policy URLPolicy) (OriginURL, error) {
	if raw == "" {
		return OriginURL{}, fmt.Errorf("streaming origin: empty URL")
	}

	u, err := url.Parse(raw)
	if err != nil {
		return OriginURL{}, fmt.Errorf("streaming origin: invalid URL")
	}

	if err := policy.ValidateURL(u); err != nil {
		return OriginURL{}, err
	}

	d, err := digestFromURL(u)
	if err != nil {
		return OriginURL{}, err
	}

	return OriginURL{
		Raw:    raw,
		Digest: d,
	}, nil
}

// ValidateURL validates scheme and host without requiring the URL to carry a
// digest. Redirect targets use this narrower check.
func (p URLPolicy) ValidateURL(u *url.URL) error {
	if u == nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" {
		return fmt.Errorf("streaming origin: absolute hierarchical URL required")
	}

	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("streaming origin: userinfo and fragments are forbidden")
	}

	if u.Scheme != "https" && (!p.AllowHTTP || u.Scheme != "http") {
		return fmt.Errorf("streaming origin: scheme %q is not allowed", u.Scheme)
	}

	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || net.ParseIP(host) != nil {
		return fmt.Errorf("streaming origin: DNS host required")
	}

	for _, allowed := range p.AllowedHostSuffixes {
		allowed = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(allowed, ".")))
		if allowed == "" {
			continue
		}

		if strings.HasPrefix(allowed, ".") {
			base := strings.TrimPrefix(allowed, ".")
			if host == base || strings.HasSuffix(host, allowed) {
				return nil
			}

			continue
		}

		if host == allowed {
			return nil
		}
	}

	return fmt.Errorf("streaming origin: host %q is not allowed", host)
}

func digestFromURL(u *url.URL) (digest.Digest, error) {
	var zero digest.Digest

	var found []string

	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return zero, fmt.Errorf("streaming origin: invalid query")
	}

	if values := query["d"]; len(values) > 1 {
		return zero, fmt.Errorf("streaming origin: duplicate d digest")
	} else if len(values) == 1 && values[0] != "" {
		value := values[0]

		hex, ok := strings.CutPrefix(value, "sha256:")
		if !ok || !digestHexPattern.MatchString(hex) {
			return zero, fmt.Errorf("streaming origin: invalid d digest")
		}

		found = append(found, hex)
	}

	segments := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	for index, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return zero, fmt.Errorf("streaming origin: invalid path escape")
		}

		if decoded == "sha256" && index >= 4 && index+3 < len(segments) {
			pathPrefix := make([]string, 4)
			validPrefix := true

			for prefixIndex := range pathPrefix {
				value, decodeErr := url.PathUnescape(segments[index-4+prefixIndex])
				if decodeErr != nil {
					return zero, fmt.Errorf("streaming origin: invalid path escape")
				}

				pathPrefix[prefixIndex] = value
			}

			for prefixIndex, want := range []string{"docker", "registry", "v2", "blobs"} {
				if pathPrefix[prefixIndex] != want {
					validPrefix = false
					break
				}
			}

			if !validPrefix {
				continue
			}

			prefix, prefixErr := url.PathUnescape(segments[index+1])

			hex, hexErr := url.PathUnescape(segments[index+2])

			data, dataErr := url.PathUnescape(segments[index+3])
			if prefixErr != nil || hexErr != nil || dataErr != nil || data != "data" ||
				len(prefix) != 2 || !digestHexPattern.MatchString(hex) || prefix != hex[:2] {
				return zero, fmt.Errorf("streaming origin: invalid registry data path digest")
			}

			found = append(found, hex)
		}
	}

	if len(found) == 0 {
		return zero, fmt.Errorf("streaming origin: SHA-256 digest not found")
	}

	for _, value := range found[1:] {
		if value != found[0] {
			return zero, fmt.Errorf("streaming origin: conflicting digests")
		}
	}

	return digest.Parse("sha256:" + found[0])
}
