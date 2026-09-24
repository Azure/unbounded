// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streaming_test

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/streaming"
)

func TestRedactedAzureOriginURLFixtures(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/azure-origin-urls.json")
	if err != nil {
		t.Fatal(err)
	}

	var fixtures []struct {
		Name              string `json:"name"`
		URL               string `json:"url"`
		Digest            string `json:"digest"`
		AllowedHostSuffix string `json:"allowed_host_suffix"`
	}
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}

	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()

			got, err := streaming.ParseOriginURL(fixture.URL, streaming.URLPolicy{
				AllowedHostSuffixes: []string{fixture.AllowedHostSuffix},
			})
			if err != nil {
				t.Fatal(err)
			}

			if got.Digest.String() != fixture.Digest {
				t.Fatalf("digest = %s, want %s", got.Digest, fixture.Digest)
			}
		})
	}
}

const testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestOriginURLFromRequestPreservesRawSignedURL(t *testing.T) {
	t.Parallel()

	raw := "https://westus.data.azurecr.io/account//docker/registry/v2/blobs/sha256/01/" + testDigest + "/data?sig=a%2Bb%2Fc%3D&se=2030-01-01T00%3A00%3A00Z"
	req := httptest.NewRequest("GET", streaming.HandlerPrefix+raw, nil)

	got, err := streaming.OriginURLFromRequest(req, streaming.URLPolicy{
		AllowedHostSuffixes: []string{".data.azurecr.io"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got.Raw != raw {
		t.Fatalf("raw URL changed:\n got: %s\nwant: %s", got.Raw, raw)
	}

	if got.Digest.String() != "sha256:"+testDigest {
		t.Fatalf("digest = %s, want sha256:%s", got.Digest, testDigest)
	}
}

func TestParseOriginURLForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		raw    string
		policy streaming.URLPolicy
	}{
		{
			name:   "ACR query digest",
			raw:    "https://app.eastus.data.azurecr.io/?sv=1&d=sha256:" + testDigest + "&sig=redacted",
			policy: streaming.URLPolicy{AllowedHostSuffixes: []string{".data.azurecr.io"}},
		},
		{
			name:   "MCR data path",
			raw:    "https://westus.data.mcr.microsoft.com/account//docker/registry/v2/blobs/sha256/01/" + testDigest + "/data?sig=redacted",
			policy: streaming.URLPolicy{AllowedHostSuffixes: []string{".data.mcr.microsoft.com"}},
		},
		{
			name:   "Azure Blob path",
			raw:    "https://account.blob.core.windows.net/container//docker/registry/v2/blobs/sha256/01/" + testDigest + "/data?sig=redacted",
			policy: streaming.URLPolicy{AllowedHostSuffixes: []string{".blob.core.windows.net"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got, err := streaming.ParseOriginURL(test.raw, test.policy)
			if err != nil {
				t.Fatal(err)
			}

			if got.Digest.String() != "sha256:"+testDigest {
				t.Fatalf("digest = %s, want sha256:%s", got.Digest, testDigest)
			}
		})
	}
}

func TestParseOriginURLRejectsUnsafeOrConflictingURL(t *testing.T) {
	t.Parallel()

	policy := streaming.URLPolicy{AllowedHostSuffixes: []string{".data.azurecr.io"}}
	differentDigest := strings.Repeat("a", 64)

	for _, raw := range []string{
		"http://westus.data.azurecr.io/?d=sha256:" + testDigest,
		"https://127.0.0.1/?d=sha256:" + testDigest,
		"https://user@westus.data.azurecr.io/?d=sha256:" + testDigest,
		"https://attacker.example/?d=sha256:" + testDigest,
		"https://westus.data.azurecr.io/no-digest?sig=redacted",
		"https://app.azurecr.io/v2/team/app/blobs/sha256:" + testDigest,
		"https://westus.data.azurecr.io/account//docker/registry/v2/blobs/sha256/aa/" + differentDigest + "/data?d=sha256:" + testDigest,
		"https://westus.data.azurecr.io/docker/registry/v2/blobs/sha256/ff/" + testDigest + "/data",
		"https://westus.data.azurecr.io/docker/not-registry/v2/blobs/sha256/01/" + testDigest + "/data",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			if _, err := streaming.ParseOriginURL(raw, policy); err == nil {
				t.Fatal("expected URL validation error")
			}
		})
	}
}

func TestURLPolicyAllowsHostOnLabelBoundaryOnly(t *testing.T) {
	t.Parallel()

	policy := streaming.URLPolicy{AllowedHostSuffixes: []string{".blob.core.windows.net"}}
	if _, err := streaming.ParseOriginURL(
		"https://evilblob.core.windows.net/?d=sha256:"+testDigest,
		policy,
	); err == nil {
		t.Fatal("expected host boundary validation error")
	}
}

func TestParseOriginURLRejectsDuplicateDigestQuery(t *testing.T) {
	t.Parallel()

	raw := "https://westus.data.azurecr.io/?d=sha256:" + testDigest +
		"&d=sha256:" + strings.Repeat("a", 64) + "&sig=redacted"
	if _, err := streaming.ParseOriginURL(raw, streaming.URLPolicy{
		AllowedHostSuffixes: []string{".data.azurecr.io"},
	}); err == nil {
		t.Fatal("expected duplicate digest validation error")
	}
}

func TestParseOriginURLErrorDoesNotExposeSignedQuery(t *testing.T) {
	t.Parallel()

	const secret = "secret-that-must-not-appear"

	raw := "https://westus.data.azurecr.io/?d=sha256:" + testDigest + "&sig=" + secret + "%zz"

	_, err := streaming.ParseOriginURL(raw, streaming.URLPolicy{
		AllowedHostSuffixes: []string{".data.azurecr.io"},
	})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, want redacted parse failure", err)
	}
}
