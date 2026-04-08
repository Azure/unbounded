package ociutil

import (
	"io"
	"strings"
	"testing"

	"github.com/opencontainers/image-spec/specs-go"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestConvertDockerMediaTypes(t *testing.T) {
	m := ispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: DockerMediaTypeManifest,
		Config: ispec.Descriptor{
			MediaType: DockerMediaTypeConfig,
			Digest:    "sha256:aaa",
			Size:      100,
		},
		Layers: []ispec.Descriptor{
			{MediaType: DockerMediaTypeLayerGzip, Digest: "sha256:bbb", Size: 200},
			{MediaType: DockerMediaTypeLayer, Digest: "sha256:ccc", Size: 300},
			{MediaType: DockerMediaTypeForeignLayer, Digest: "sha256:ddd", Size: 400},
		},
	}

	ConvertDockerMediaTypes(&m)

	if m.MediaType != ispec.MediaTypeImageManifest {
		t.Errorf("manifest MediaType = %q, want %q", m.MediaType, ispec.MediaTypeImageManifest)
	}

	if m.Config.MediaType != ispec.MediaTypeImageConfig {
		t.Errorf("config MediaType = %q, want %q", m.Config.MediaType, ispec.MediaTypeImageConfig)
	}

	if m.Layers[0].MediaType != ispec.MediaTypeImageLayerGzip {
		t.Errorf("layer[0] MediaType = %q, want %q", m.Layers[0].MediaType, ispec.MediaTypeImageLayerGzip)
	}

	if m.Layers[1].MediaType != ispec.MediaTypeImageLayer {
		t.Errorf("layer[1] MediaType = %q, want %q", m.Layers[1].MediaType, ispec.MediaTypeImageLayer)
	}
	//nolint:staticcheck // matching deprecated OCI type
	if m.Layers[2].MediaType != ispec.MediaTypeImageLayerNonDistributableGzip {
		t.Errorf("layer[2] MediaType = %q, want %q", m.Layers[2].MediaType, ispec.MediaTypeImageLayerNonDistributableGzip) //nolint:staticcheck
	}
}

func TestConvertDockerMediaTypes_OCI_Noop(t *testing.T) {
	// An already-OCI manifest should not be modified.
	m := ispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ispec.MediaTypeImageManifest,
		Config: ispec.Descriptor{
			MediaType: ispec.MediaTypeImageConfig,
			Digest:    "sha256:aaa",
			Size:      100,
		},
		Layers: []ispec.Descriptor{
			{MediaType: ispec.MediaTypeImageLayerGzip, Digest: "sha256:bbb", Size: 200},
		},
	}

	ConvertDockerMediaTypes(&m)

	if m.MediaType != ispec.MediaTypeImageManifest {
		t.Errorf("manifest MediaType = %q, want %q", m.MediaType, ispec.MediaTypeImageManifest)
	}

	if m.Config.MediaType != ispec.MediaTypeImageConfig {
		t.Errorf("config MediaType = %q, want %q", m.Config.MediaType, ispec.MediaTypeImageConfig)
	}

	if m.Layers[0].MediaType != ispec.MediaTypeImageLayerGzip {
		t.Errorf("layer[0] MediaType = %q, want %q", m.Layers[0].MediaType, ispec.MediaTypeImageLayerGzip)
	}
}

func TestRegisterDockerParsers_Idempotent(t *testing.T) {
	// Calling multiple times must not panic.
	RegisterDockerParsers()
	RegisterDockerParsers()
}

func TestRegisterDockerParsers_ParseManifest(t *testing.T) {
	RegisterDockerParsers()

	rdr := strings.NewReader(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`)

	result, err := parseManifest(rdr)
	if err != nil {
		t.Fatalf("parseManifest: %v", err)
	}

	m, ok := result.(ispec.Manifest)
	if !ok {
		t.Fatalf("parseManifest returned %T, want ispec.Manifest", result)
	}

	if m.SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d, want 2", m.SchemaVersion)
	}
}

func TestRegisterDockerParsers_ParseManifest_NilReader(t *testing.T) {
	result, err := parseManifest(nil)
	if err != nil {
		t.Fatalf("parseManifest(nil): %v", err)
	}

	if _, ok := result.(ispec.Manifest); !ok {
		t.Fatalf("parseManifest(nil) returned %T, want ispec.Manifest", result)
	}
}

func TestRegisterDockerParsers_ParseIndex(t *testing.T) {
	rdr := strings.NewReader(`{"schemaVersion":2,"manifests":[]}`)

	result, err := parseIndex(rdr)
	if err != nil {
		t.Fatalf("parseIndex: %v", err)
	}

	if _, ok := result.(ispec.Index); !ok {
		t.Fatalf("parseIndex returned %T, want ispec.Index", result)
	}
}

func TestRegisterDockerParsers_ParseImage(t *testing.T) {
	rdr := strings.NewReader(`{"architecture":"amd64","os":"linux"}`)

	result, err := parseImage(rdr)
	if err != nil {
		t.Fatalf("parseImage: %v", err)
	}

	img, ok := result.(ispec.Image)
	if !ok {
		t.Fatalf("parseImage returned %T, want ispec.Image", result)
	}

	if img.Architecture != "amd64" {
		t.Errorf("Architecture = %q, want %q", img.Architecture, "amd64")
	}
}

func TestParsers_NilReader(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parse func(r *strings.Reader) (any, error)
	}{
		{"manifest", func(_ *strings.Reader) (any, error) { return parseManifest(nil) }},
		{"index", func(_ *strings.Reader) (any, error) { return parseIndex(nil) }},
		{"image", func(_ *strings.Reader) (any, error) { return parseImage(nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.parse(nil)
			if err != nil {
				t.Fatalf("parse(nil): %v", err)
			}

			if result == nil {
				t.Fatal("parse(nil) returned nil")
			}
		})
	}
}

func TestParsers_InvalidJSON(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parse func(io.Reader) (any, error)
	}{
		{"manifest", parseManifest},
		{"index", parseIndex},
		{"image", parseImage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.parse(strings.NewReader(`{invalid json`))
			if err == nil {
				t.Fatal("expected error for invalid JSON")
			}
		})
	}
}

func TestConvertDockerMediaTypes_MixedLayers(t *testing.T) {
	// A manifest with a mix of Docker and OCI layer types should only
	// convert the Docker ones, leaving OCI layers untouched.
	m := ispec.Manifest{
		MediaType: DockerMediaTypeManifest,
		Config:    ispec.Descriptor{MediaType: DockerMediaTypeConfig},
		Layers: []ispec.Descriptor{
			{MediaType: DockerMediaTypeLayerGzip},
			{MediaType: ispec.MediaTypeImageLayerGzip}, // already OCI
			{MediaType: ispec.MediaTypeImageLayerZstd}, // OCI zstd
			{MediaType: DockerMediaTypeLayer},
		},
	}

	ConvertDockerMediaTypes(&m)

	want := []string{
		ispec.MediaTypeImageLayerGzip,
		ispec.MediaTypeImageLayerGzip,
		ispec.MediaTypeImageLayerZstd,
		ispec.MediaTypeImageLayer,
	}
	for i, l := range m.Layers {
		if l.MediaType != want[i] {
			t.Errorf("layer[%d] MediaType = %q, want %q", i, l.MediaType, want[i])
		}
	}
}
