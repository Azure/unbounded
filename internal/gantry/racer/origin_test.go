// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	sdk "github.com/Azure/unbounded/pkg/racer"
)

func testRef() ifaces.OriginRef {
	return ifaces.OriginRef{Registry: "registry.example:5000", Repository: "library/image", Kind: ifaces.KindManifest, Digest: digest.MustParse(fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("payload"))))}
}

func TestTargetCanonicalAndCredentialFree(t *testing.T) {
	ref := testRef()

	target, err := Target(ref)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ParseTarget(target)
	if err != nil || got != ref {
		t.Fatal(got, err)
	}

	for _, bad := range []string{target + "?auth=secret", target + "/", strings.Replace(target, "/v1/", "/v2/", 1), strings.Replace(target, "sha256:", "SHA256:", 1)} {
		if _, err := ParseTarget(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}

	ref.Registry = "user:password@registry.example"
	if _, err := Target(ref); err == nil {
		t.Fatal("accepted credentials")
	}

	ref = testRef()
	ref.Kind = ifaces.KindBlob
	blob, _ := Target(ref)
	ref.Kind = ifaces.KindConfig

	config, _ := Target(ref)
	if blob != config || blob == target {
		t.Fatal("wrong URL kind canonicalization")
	}
}

type localFixture struct {
	body        []byte
	unavailable bool
	opens       int
}

func (f *localFixture) Descriptor(context.Context, digest.Digest) (ocispec.Descriptor, error) {
	if f.unavailable {
		return ocispec.Descriptor{}, &ifaces.ErrUnavailable{Cause: errors.New("offline")}
	}

	return ocispec.Descriptor{Size: int64(len(f.body)), MediaType: "application/vnd.oci.image.index.v1+json"}, nil
}

func (f *localFixture) Open(context.Context, digest.Digest) (io.ReadCloser, int64, error) {
	f.opens++
	return &seekCloser{bytes.NewReader(f.body)}, int64(len(f.body)), nil
}

type seekCloser struct{ *bytes.Reader }

func (*seekCloser) Close() error { return nil }

func TestOriginLocalFirstAndMetadata(t *testing.T) {
	ref := testRef()
	target, _ := Target(ref)
	local := &localFixture{body: []byte("payload")}
	o := &Origin{Local: local, Registries: map[string]bool{ref.Registry: true}}

	meta, err := o.Stat(t.Context(), target)
	if err != nil || meta.Size != 7 || meta.ETag != `"`+ref.Digest.Hex()+`"` || meta.ContentType != "application/vnd.oci.image.index.v1+json" || meta.TTL == nil || *meta.TTL != MetadataTTL || local.opens != 0 {
		t.Fatal(meta, err)
	}

	body, err := o.OpenRange(t.Context(), target, meta.ETag, 2, 3)
	if err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(body)
	_ = body.Close()

	if err != nil || string(got) != "ylo" {
		t.Fatal(string(got), err)
	}

	if _, err := o.OpenRange(t.Context(), target, `"wrong"`, 0, 7); !errors.Is(err, sdk.ErrVersionChanged) {
		t.Fatal(err)
	}

	if _, err := o.OpenRange(t.Context(), target, meta.ETag, 6, 3); !errors.Is(err, sdk.ErrVersionChanged) {
		t.Fatal(err)
	}

	if _, err := o.Stat(t.Context(), "/not-a-target"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}

	local.unavailable = true

	if _, err := o.Stat(t.Context(), target); err == nil {
		t.Fatal("unavailable local must not become registry miss")
	}
}

func TestQuarantineBoundAndIsolation(t *testing.T) {
	b := &Backend{}

	ref := testRef()
	for i := range 1050 {
		ref.Repository = fmt.Sprintf("repo%d", i)
		b.Quarantine(ref)
	}

	if len(b.quarantine) != 1024 {
		t.Fatal(len(b.quarantine))
	}

	if _, err := b.Open(t.Context(), ref); !errors.Is(err, ErrQuarantined) {
		t.Fatal(err)
	}

	target, _ := Target(ref)
	ref.Registry = "other.example"

	other, _ := Target(ref)
	if _, ok := b.quarantine[other]; ok || target == other {
		t.Fatal("cross-registry quarantine")
	}
}
