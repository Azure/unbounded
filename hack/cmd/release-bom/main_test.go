// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestBuildBOM(t *testing.T) {
	opts := options{
		tag:           "v1.2.3",
		commit:        "abcdef123456",
		registry:      "registry.example.com/project",
		netCNIVersion: "v9.8.7",
		output:        "ignored.json",
	}

	resolver := func(_ context.Context, name, ref string) (resolvedImage, error) {
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(ref)))

		return resolvedImage{
			Name:      name,
			Reference: ref,
			Digest:    digest,
			MediaType: "application/vnd.oci.image.index.v1+json",
			Platforms: []string{"linux/amd64", "linux/arm64"},
		}, nil
	}

	bom, err := buildBOM(t.Context(), opts, resolver)
	if err != nil {
		t.Fatalf("buildBOM: %v", err)
	}

	if bom.SchemaVersion != bomSchemaVersion || bom.Release.Tag != opts.tag || bom.Release.GitCommit != opts.commit {
		t.Fatalf("release metadata = %#v", bom.Release)
	}

	if len(bom.Images) != len(releaseImageNames) {
		t.Fatalf("release image count = %d, want %d", len(bom.Images), len(releaseImageNames))
	}

	if got, want := bom.Images[0].Reference, "registry.example.com/project/gantry:v1.2.3"; got != want {
		t.Fatalf("first image reference = %q, want %q", got, want)
	}

	if bom.NodeBootstrap.ContainerdVersion != goalstates.ContainerdVersion ||
		bom.NodeBootstrap.RuncVersion != goalstates.RunCVersion ||
		bom.NodeBootstrap.CNIPluginVersion != goalstates.CNIPluginVersion ||
		bom.NodeBootstrap.NetCNIPluginVersion != opts.netCNIVersion {
		t.Fatalf("node bootstrap versions = %#v", bom.NodeBootstrap)
	}

	if len(bom.NodeBootstrap.RootFSImages) != 6 {
		t.Fatalf("rootfs image count = %d, want 6", len(bom.NodeBootstrap.RootFSImages))
	}

	if bom.NodeBootstrap.SandboxImage.Reference != goalstates.SandboxImage {
		t.Fatalf("sandbox image = %q, want %q", bom.NodeBootstrap.SandboxImage.Reference, goalstates.SandboxImage)
	}

	foundOperatorManifest := false

	for _, artifact := range bom.Artifacts {
		if artifact.Name == "unbounded-operator-v1.2.3.yaml" && artifact.SignatureBundle == "unbounded-operator-v1.2.3.yaml.bundle.json" {
			foundOperatorManifest = true
		}
	}

	if !foundOperatorManifest {
		t.Fatal("signed operator manifest is missing from BOM artifacts")
	}
}

func TestOptionsValidate(t *testing.T) {
	valid := options{tag: "v1", commit: "abc", registry: "registry.example", netCNIVersion: "v1", output: "bom.json"}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid options: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*options)
	}{
		{name: "tag", mutate: func(o *options) { o.tag = "" }},
		{name: "commit", mutate: func(o *options) { o.commit = "" }},
		{name: "registry", mutate: func(o *options) { o.registry = "" }},
		{name: "net CNI version", mutate: func(o *options) { o.netCNIVersion = "" }},
		{name: "output", mutate: func(o *options) { o.output = "" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := valid
			test.mutate(&opts)

			if err := opts.validate(); err == nil {
				t.Fatal("validate returned nil")
			}
		})
	}
}

func TestResolvedImagePlatformsAreStable(t *testing.T) {
	platforms := uniqueSortedStrings([]string{"linux/arm64", "linux/amd64", "linux/arm64"})

	if got, want := fmt.Sprint(platforms), "[linux/amd64 linux/arm64]"; got != want {
		t.Fatalf("platforms = %q, want %q", got, want)
	}
}

func TestWriteBOMCreatesOutputDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "release-bom.json")
	want := &releaseBOM{SchemaVersion: bomSchemaVersion, Release: releaseInfo{Tag: "v1.2.3", GitCommit: "abc"}}

	if err := writeBOM(path, want); err != nil {
		t.Fatalf("writeBOM: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read BOM: %v", err)
	}

	var got releaseBOM
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal BOM: %v", err)
	}

	if got.SchemaVersion != want.SchemaVersion || got.Release != want.Release {
		t.Fatalf("BOM = %#v, want %#v", got, want)
	}
}
