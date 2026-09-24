//go:build e2e && linux

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	cdimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	ocidigest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	gantryracer "github.com/Azure/unbounded/internal/gantry/racer"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

// Exercise the production source, recorder wiring, and nil-DHT/nil-notifier
// subscriber in a running Racer agent. Direct origin HEADs bypass both the
// mirror and Racer's metadata cache, so neither can hide a missing descriptor.
func gantryLocalMetadataWithoutLibp2p(t *testing.T, f *gantryFixture) {
	t.Helper()
	f.agents[0].stop()
	f.offline.Store(true)

	ctx, release, err := f.containerd.WithLease(namespaces.WithNamespace(t.Context(), "node0"))
	if err != nil {
		t.Fatal(err)
	}
	defer release(ctx)

	write := func(value any, mediaType string) ocispec.Descriptor {
		t.Helper()

		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}

		desc := ocispec.Descriptor{Digest: ocidigest.FromBytes(data), Size: int64(len(data)), MediaType: mediaType}
		if err := content.WriteBlob(ctx, f.containerd.ContentStore(), desc.Digest.String(), bytes.NewReader(data), desc); err != nil {
			t.Fatal(err)
		}

		return desc
	}
	seed := func(phase string) []ocispec.Descriptor {
		t.Helper()

		var descriptors []ocispec.Descriptor

		for _, types := range [][3]string{
			{ocispec.MediaTypeImageConfig, ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageIndex},
			{cdimages.MediaTypeDockerSchema2Config, cdimages.MediaTypeDockerSchema2Manifest, cdimages.MediaTypeDockerSchema2ManifestList},
		} {
			cfg := write(map[string]string{"phase": phase, "architecture": "amd64", "os": "linux"}, types[0])
			manifest := write(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: types[1], Config: cfg, Layers: []ocispec.Descriptor{}}, types[1])
			index := write(ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: types[2], Manifests: []ocispec.Descriptor{manifest}}, types[2])
			descriptors = append(descriptors, manifest, index)
		}

		return descriptors
	}
	publish := func(descriptors []ocispec.Descriptor, name string, update bool) {
		t.Helper()

		for i := 1; i < len(descriptors); i += 2 {
			image := cdimages.Image{Name: fmt.Sprintf("fixture.test/metadata/%s:%d", name, i), Target: descriptors[i]}

			var err error
			if update {
				_, err = f.containerd.ImageService().Update(ctx, image, "target")
			} else {
				_, err = f.containerd.ImageService().Create(ctx, image)
			}

			if err != nil {
				t.Fatal(err)
			}
		}
	}
	initial := seed("initial")
	publish(initial, "initial", false)

	// A real host initialization anywhere in startup must either touch this
	// identity or fail to bind the exclusively occupied TCP address.
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	address := occupied.Addr().String()
	identity := filepath.Join(f.dir, "metadata-libp2p-identity")
	assertNoIdentity := func() {
		t.Helper()

		if _, err := os.Lstat(identity); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Racer agent touched configured libp2p identity: %v", err)
		}
	}
	assertNoIdentity()

	data, err := os.ReadFile(f.agentConfigs[0])
	if err != nil {
		t.Fatal(err)
	}

	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}

	cfg.Libp2pIdentityPath = identity
	cfg.Libp2pListen = []string{fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", occupied.Addr().(*net.TCPAddr).Port)}

	data, err = yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(f.dir, "gantry-metadata.yaml")
	gantryWrite(t, path, data)
	f.agents[0] = gantryStart(t, f.dir, "gantry-metadata", []string{"SSL_CERT_FILE=" + filepath.Join(f.dir, "registry-0.pem")}, os.Getenv("GANTRY_BINARY"), "agent", "--config", path)
	gantryAwait(t, "Racer readiness with occupied libp2p port", func() bool {
		resp, err := f.client.Get("http://" + f.metrics[0] + "/readyz")
		if err != nil {
			return false
		}

		resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	})
	assertNoIdentity()

	origin, err := sdk.NewClient("/run/racer/node0/origin", sdk.ClientOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer origin.CloseIdleConnections()

	target := func(desc ocispec.Descriptor) string {
		t.Helper()

		value, err := gantryracer.Target(ifaces.OriginRef{Registry: "fixture.test", Repository: "metadata", Kind: ifaces.KindManifest, Digest: digest.MustParse(desc.Digest.String())})
		if err != nil {
			t.Fatal(err)
		}

		return value
	}
	assertMetadata := func(t *testing.T, descriptors []ocispec.Descriptor) {
		t.Helper()

		for _, desc := range descriptors {
			gantryAwait(t, "local metadata "+desc.MediaType, func() bool {
				_, err := origin.Stat(ctx, target(desc))
				return err == nil
			})
		}

		before := f.originRequestCount("", "")

		for _, desc := range descriptors {
			meta, err := origin.Stat(ctx, target(desc))
			if err != nil || meta.ContentType != desc.MediaType || meta.Size != desc.Size || meta.ETag != `"`+desc.Digest.Encoded()+`"` {
				t.Fatalf("local %s metadata: %+v, err=%v, want %+v", desc.MediaType, meta, err, desc)
			}
		}

		if after := f.originRequestCount("", ""); after != before {
			t.Fatalf("local metadata contacted offline registry: before=%d after=%d", before, after)
		}
	}
	t.Run("InitialList", func(t *testing.T) { assertMetadata(t, initial) })

	for _, phase := range []string{"Create", "Update"} {
		t.Run(phase, func(t *testing.T) {
			descriptors := seed(phase)
			// Bare content is present but has no recorded media type. Require a
			// failed registry HEAD before publishing the image event; this rules
			// out JSON sniffing or an unrelated path populating the index.
			before := f.originRequestCount("HEAD", "")

			for _, desc := range descriptors {
				_, err := origin.Stat(ctx, target(desc))

				var unavailable *sdk.HTTPError
				if !errors.As(err, &unavailable) || unavailable.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("unrecorded %s must require unavailable registry metadata: %v", desc.MediaType, err)
				}
			}

			if after := f.originRequestCount("HEAD", ""); after != before+len(descriptors) {
				t.Fatalf("unrecorded descriptors did not each require registry HEAD: before=%d after=%d", before, after)
			}

			publish(descriptors, "events", phase == "Update")
			assertMetadata(t, descriptors)
		})
	}

	assertNoIdentity()

	if err := occupied.Close(); err != nil {
		t.Fatal(err)
	}

	probe, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatalf("running Racer agent bound configured libp2p address: %v", err)
	}

	probe.Close()
	t.Log("ready Racer agent ignored occupied libp2p address and identity; initial List and create/update events supplied offline OCI and Docker manifest/index metadata")
}
