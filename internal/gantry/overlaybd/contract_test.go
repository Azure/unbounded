// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"errors"
	"strings"
	"testing"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestParseManifestPreservesACRLayerContract(t *testing.T) {
	body := `{
		"schemaVersion":2,
		"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + digestA + `","size":123},
		"layers":[
			{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"` + digestB + `","size":4096,"annotations":{"` + AnnotationBlobDigest + `":"` + digestB + `","` + AnnotationBlobSize + `":"4096","` + AnnotationVersion + `":"` + VersionV1 + `","` + AnnotationBlobFSType + `":"ext4"}},
			{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"` + digestC + `","size":8192,"annotations":{"` + AnnotationBlobDigest + `":"` + digestC + `","` + AnnotationBlobSize + `":"8192"}}
		]
	}`

	manifest, err := ParseManifest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}

	if manifest.ConfigDigest.String() != digestA || len(manifest.Layers) != 2 {
		t.Fatalf("manifest = %+v", manifest)
	}

	if manifest.Layers[0].DataDigest.String() != digestB || manifest.Layers[0].Size != 4096 || manifest.Layers[0].FSType != "ext4" || manifest.Layers[0].Version != VersionV1 {
		t.Fatalf("first layer = %+v", manifest.Layers[0])
	}

	if manifest.Layers[1].DataDigest.String() != digestC || manifest.Layers[1].Size != 8192 || manifest.Layers[1].Version != "" {
		t.Fatalf("second layer = %+v", manifest.Layers[1])
	}
}

func TestParseManifestRejectsOrdinaryAndMalformedLayers(t *testing.T) {
	base := `{"schemaVersion":2,"config":{"digest":"` + digestA + `","size":1},"layers":[{"digest":"` + digestB + `","size":4096,"annotations":{%s}}]}`

	for _, test := range []struct {
		name        string
		annotations string
		wantNotAS   bool
		want        string
	}{
		{name: "ordinary", annotations: ``, wantNotAS: true},
		{name: "missing size", annotations: `"` + AnnotationBlobDigest + `":"` + digestB + `"`, wantNotAS: true},
		{name: "bad size", annotations: `"` + AnnotationBlobDigest + `":"` + digestB + `","` + AnnotationBlobSize + `":"invalid"`, want: "invalid blob size"},
		{name: "size mismatch", annotations: `"` + AnnotationBlobDigest + `":"` + digestB + `","` + AnnotationBlobSize + `":"8192"`, want: "does not match"},
		{name: "unsupported version", annotations: `"` + AnnotationBlobDigest + `":"` + digestB + `","` + AnnotationBlobSize + `":"4096","` + AnnotationVersion + `":"2.0.0"`, want: "unsupported OverlayBD version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(strings.Replace(base, "%s", test.annotations, 1)))
			if test.wantNotAS {
				if !errors.Is(err, ErrNotArtifactStreaming) {
					t.Fatalf("error = %v, want ErrNotArtifactStreaming", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
