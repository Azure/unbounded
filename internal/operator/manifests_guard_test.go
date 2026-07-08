// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"io/fs"
	"strings"
	"testing"

	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	netmanifests "github.com/Azure/unbounded/deploy/net"
	storagemanifests "github.com/Azure/unbounded/deploy/unbounded-storage-supervisor"
)

// TestEmbeddedManifestsHaveNoLatestImageTags guards that the component manifests
// the operator embeds and applies never pin an image to :latest. Operator-managed
// components must be version-matched to the operator's release; a :latest tag
// (as previously produced by the storage supervisor manifests) breaks that
// invariant. This test relies on `make test` rendering the manifests first.
func TestEmbeddedManifestsHaveNoLatestImageTags(t *testing.T) {
	sets := map[string]fs.FS{
		"machina": machinamanifests.Manifests,
		"net":     netmanifests.Manifests,
		"storage": storagemanifests.Manifests,
	}

	for name, manifests := range sets {
		files, err := yamlFiles(manifests)
		if err != nil {
			t.Fatalf("%s: list manifests: %v", name, err)
		}

		for _, file := range files {
			data, err := fs.ReadFile(manifests, file)
			if err != nil {
				t.Fatalf("%s: read %s: %v", name, file, err)
			}

			for _, line := range strings.Split(string(data), "\n") {
				trimmed := strings.TrimSpace(line)
				if !strings.HasPrefix(trimmed, "image:") {
					continue
				}

				image := strings.TrimSpace(strings.TrimPrefix(trimmed, "image:"))
				if strings.HasSuffix(image, ":latest") {
					t.Errorf("%s manifest %s pins a :latest image (%q); operator-managed components must be version-matched", name, file, image)
				}
			}
		}
	}
}
