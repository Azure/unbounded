// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package origin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestManifestByTag(t *testing.T) {
	body := []byte(`{"schemaVersion":2}`)
	sum := sha256.Sum256(body)
	want := "sha256:" + hex.EncodeToString(sum[:])
	badDigest := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/library/app/manifests/v1":
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")

			if badDigest {
				w.Header().Set("Docker-Content-Digest", "sha256:"+hex.EncodeToString(make([]byte, 32)))
			} else {
				w.Header().Set("Docker-Content-Digest", want)
			}

			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL})

	got, ct, d, err := c.ManifestByTag(context.Background(), "reg", "library/app", "v1")
	if err != nil {
		t.Fatalf("ManifestByTag: %v", err)
	}

	if string(got) != string(body) || ct != "application/vnd.oci.image.manifest.v1+json" || d.String() != want {
		t.Fatalf("got %q %q %s", got, ct, d)
	}

	if _, _, _, err := c.ManifestByTag(context.Background(), "reg", "library/app", "missing"); err == nil {
		t.Fatal("want not-found error")
	} else {
		var oe *ifaces.OriginError
		if !errors.As(err, &oe) || oe.Class != ifaces.FailureNotFound {
			t.Fatalf("want not_found OriginError, got %v", err)
		}
	}

	if _, _, _, err := c.ManifestByTag(context.Background(), "reg", "library/app", "bad/tag"); err == nil {
		t.Fatal("want invalid tag error")
	}

	if _, _, _, err := c.ManifestByTag(context.Background(), "other", "library/app", "v1"); err == nil {
		t.Fatal("want unknown registry error")
	}

	badDigest = true

	if _, _, _, err := c.ManifestByTag(context.Background(), "reg", "library/app", "v1"); err == nil {
		t.Fatal("want digest mismatch error")
	}
}

func TestManifestByTagPreservesEndpointQuery(t *testing.T) {
	body := []byte(`{"schemaVersion":2}`)
	sum := sha256.Sum256(body)
	want := "sha256:" + hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/library/app/manifests/v1" || r.URL.RawQuery != "token=abc&ns=x" {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", want)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := newClient(t, config.UpstreamRegistry{Name: "reg", Endpoint: srv.URL + "?token=abc&ns=x"})

	_, _, d, err := c.ManifestByTag(context.Background(), "reg", "library/app", "v1")
	if err != nil {
		t.Fatalf("ManifestByTag: %v", err)
	}

	if d.String() != want {
		t.Fatalf("digest = %s, want %s", d, want)
	}
}
