// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package manifest_test

import (
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/manifest"
)

const ociImageManifest = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.oci.image.config.v1+json",
    "digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
    "size": 1024
  },
  "layers": [
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "digest": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
      "size": 10485760
    },
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "digest": "sha256:3333333333333333333333333333333333333333333333333333333333333333",
      "size": 20971520
    }
  ]
}`

const dockerImageManifest = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
  "config": {
    "mediaType": "application/vnd.docker.container.image.v1+json",
    "digest": "sha256:4444444444444444444444444444444444444444444444444444444444444444",
    "size": 2048
  },
  "layers": [
    {
      "mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
      "digest": "sha256:5555555555555555555555555555555555555555555555555555555555555555",
      "size": 5242880
    }
  ]
}`

const ociImageIndex = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:6666666666666666666666666666666666666666666666666666666666666666",
      "size": 1357,
      "platform": {"architecture": "amd64", "os": "linux"}
    },
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:7777777777777777777777777777777777777777777777777777777777777777",
      "size": 1357,
      "platform": {"architecture": "arm64", "os": "linux"}
    }
  ]
}`

const manifestWithForeignLayer = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
  "config": {
    "mediaType": "application/vnd.docker.container.image.v1+json",
    "digest": "sha256:8888888888888888888888888888888888888888888888888888888888888888",
    "size": 2048
  },
  "layers": [
    {
      "mediaType": "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip",
      "digest": "sha256:9999999999999999999999999999999999999999999999999999999999999999",
      "size": 100000000,
      "urls": ["https://mcr.microsoft.com/v2/.../blobs/sha256:..."]
    },
    {
      "mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
      "digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "size": 5000000
    }
  ]
}`

func TestChildDigests_OCIManifest(t *testing.T) {
	got, err := manifest.ChildDigests([]byte(ociImageManifest))
	if err != nil {
		t.Fatalf("ChildDigests: %v", err)
	}

	want := []digest.Digest{
		digest.MustParse("sha256:1111111111111111111111111111111111111111111111111111111111111111"),
		digest.MustParse("sha256:2222222222222222222222222222222222222222222222222222222222222222"),
		digest.MustParse("sha256:3333333333333333333333333333333333333333333333333333333333333333"),
	}
	if len(got) != len(want) {
		t.Fatalf("count: got %d want %d (%v)", len(got), len(want), got)
	}

	for i, d := range want {
		if got[i].String() != d.String() {
			t.Fatalf("digest[%d]: got %s want %s", i, got[i], d)
		}
	}
}

func TestChildDigests_DockerManifest(t *testing.T) {
	got, err := manifest.ChildDigests([]byte(dockerImageManifest))
	if err != nil {
		t.Fatalf("ChildDigests: %v", err)
	}

	want := []string{
		"sha256:4444444444444444444444444444444444444444444444444444444444444444",
		"sha256:5555555555555555555555555555555555555555555555555555555555555555",
	}
	if len(got) != len(want) {
		t.Fatalf("count: got %d want %d", len(got), len(want))
	}

	for i, d := range want {
		if got[i].String() != d {
			t.Fatalf("digest[%d]: got %s want %s", i, got[i], d)
		}
	}
}

func TestChildDigests_ImageIndexReturnsEmpty(t *testing.T) {
	got, err := manifest.ChildDigests([]byte(ociImageIndex))
	if err != nil {
		t.Fatalf("ChildDigests: %v", err)
	}

	if len(got) != 0 {
		t.Fatalf("image index should produce 0 digests, got %v", got)
	}

	if !manifest.IsImageIndex([]byte(ociImageIndex)) {
		t.Fatalf("IsImageIndex returned false on image index")
	}
}

func TestChildDigests_SkipsForeignLayers(t *testing.T) {
	got, err := manifest.ChildDigests([]byte(manifestWithForeignLayer))
	if err != nil {
		t.Fatalf("ChildDigests: %v", err)
	}

	want := []string{
		"sha256:8888888888888888888888888888888888888888888888888888888888888888",
		// Foreign layer (sha256:9999...) skipped because of urls[].
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	if len(got) != len(want) {
		t.Fatalf("count: got %d want %d (%v)", len(got), len(want), got)
	}

	for i, d := range want {
		if got[i].String() != d {
			t.Fatalf("digest[%d]: got %s want %s", i, got[i], d)
		}
	}
}

func TestChildDigests_MalformedJSON(t *testing.T) {
	_, err := manifest.ChildDigests([]byte("{this is not JSON"))
	if err == nil {
		t.Fatalf("ChildDigests on garbage: expected error, got nil")
	}
}

func TestChildDigests_EmptyBody(t *testing.T) {
	_, err := manifest.ChildDigests([]byte(""))
	if err == nil {
		t.Fatalf("ChildDigests on empty body: expected error, got nil")
	}
}

func TestChildDigests_MalformedInnerDigestSkipped(t *testing.T) {
	body := `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {"mediaType": "x", "digest": "not-a-digest", "size": 1},
  "layers": [
    {"mediaType": "x", "digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "size": 1},
    {"mediaType": "x", "digest": "also-bad", "size": 1}
  ]
}`

	got, err := manifest.ChildDigests([]byte(body))
	if err != nil {
		t.Fatalf("ChildDigests: %v", err)
	}
	// Bad config digest skipped; one good layer survives.
	if len(got) != 1 {
		t.Fatalf("expected 1 digest, got %d (%v)", len(got), got)
	}

	if got[0].String() != "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("got %s", got[0])
	}
}

// TestTypedChildren_TagsConfigAsKindConfigAndLayersAsKindBlob is the
// load-bearing invariant for the observability fix: the
// image-config blob MUST be tagged ifaces.KindConfig so the
// p2p_origin_pull_total{kind="config"} bucket actually counts when
// the prefetch path drives PleasePull/StartLocalPull through
// PrefetchChildren. Layers stay as ifaces.KindBlob because the OCI
// URL family is /blobs/ for both, and downstream pullers don't need
// to distinguish - only the metric label does.
func TestTypedChildren_TagsConfigAsKindConfigAndLayersAsKindBlob(t *testing.T) {
	t.Parallel()

	got, err := manifest.TypedChildren([]byte(ociImageManifest))
	if err != nil {
		t.Fatalf("TypedChildren: %v", err)
	}
	// Config first, then two layers, source order.
	if len(got) != 3 {
		t.Fatalf("got %d typed children, want 3 (%v)", len(got), got)
	}

	if got[0].Digest.String() != "sha256:1111111111111111111111111111111111111111111111111111111111111111" {
		t.Fatalf("config digest mismatch: %s", got[0].Digest)
	}

	if got[0].Kind != ifaces.KindConfig {
		t.Fatalf("config kind = %v, want KindConfig", got[0].Kind)
	}

	for i, c := range got[1:] {
		if c.Kind != ifaces.KindBlob {
			t.Errorf("layer[%d] kind = %v, want KindBlob", i, c.Kind)
		}
	}
}

// TestTypedChildren_DockerManifestSameKindingAsOCI confirms the
// kinding contract is media-type agnostic: schema-2 docker manifests
// and OCI manifests both place the config in the .config slot and
// layers in the .layers slot, and TypedChildren MUST tag them the
// same way for both formats.
func TestTypedChildren_DockerManifestSameKindingAsOCI(t *testing.T) {
	t.Parallel()

	got, err := manifest.TypedChildren([]byte(dockerImageManifest))
	if err != nil {
		t.Fatalf("TypedChildren: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}

	if got[0].Kind != ifaces.KindConfig {
		t.Fatalf("docker config kind = %v, want KindConfig", got[0].Kind)
	}

	if got[1].Kind != ifaces.KindBlob {
		t.Fatalf("docker layer kind = %v, want KindBlob", got[1].Kind)
	}
}

// TestTypedChildren_ImageIndexReturnsNil mirrors ChildDigests'
// image-index handling: there's no config-or-layer blob to fetch
// until containerd asks for an architecture-specific manifest.
func TestTypedChildren_ImageIndexReturnsNil(t *testing.T) {
	t.Parallel()

	got, err := manifest.TypedChildren([]byte(ociImageIndex))
	if err != nil {
		t.Fatalf("TypedChildren: %v", err)
	}

	if got != nil {
		t.Fatalf("image index: got %v, want nil", got)
	}
}

// TestChildDigests_MatchesTypedChildren proves the back-compat path
// is byte-equivalent in ordering and content with the new typed API.
// If either drifts, both callers (pre-Kind PrefetchLayers and the
// new PrefetchChildren) would see inconsistent prefetch sets - this
// guards against that regression.
func TestChildDigests_MatchesTypedChildren(t *testing.T) {
	t.Parallel()

	flat, err := manifest.ChildDigests([]byte(ociImageManifest))
	if err != nil {
		t.Fatalf("ChildDigests: %v", err)
	}

	typed, err := manifest.TypedChildren([]byte(ociImageManifest))
	if err != nil {
		t.Fatalf("TypedChildren: %v", err)
	}

	if len(flat) != len(typed) {
		t.Fatalf("length mismatch: flat=%d typed=%d", len(flat), len(typed))
	}

	for i := range flat {
		if flat[i] != typed[i].Digest {
			t.Errorf("index %d: flat=%s typed=%s", i, flat[i], typed[i].Digest)
		}
	}
}
