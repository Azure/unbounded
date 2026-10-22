// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ociartifact

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nopCloser adapts a reader so it can stand in for a fetched blob body.
type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

func descriptorFor(content string) ocispec.Descriptor {
	return ocispec.Descriptor{
		Digest: digest.FromString(content),
		Size:   int64(len(content)),
	}
}

// TestVerifyingReadCloserAcceptsMatchingContent is the baseline: a blob whose
// bytes match its descriptor reads through cleanly and terminates with io.EOF.
func TestVerifyingReadCloserAcceptsMatchingContent(t *testing.T) {
	t.Parallel()

	body := "sysext contents"
	rc := newVerifyingReadCloser(nopCloser{strings.NewReader(body)}, descriptorFor(body), "blob.raw")

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))
	require.NoError(t, rc.Close())
}

// TestVerifyingReadCloserRejectsTamperedContent is the case oras-go does not
// catch on its own: the content is the declared size but the wrong bytes, so
// only hashing the stream detects it. Registries may omit the
// Docker-Content-Digest header entirely, in which case oras-go performs no
// check at all.
func TestVerifyingReadCloserRejectsTamperedContent(t *testing.T) {
	t.Parallel()

	// Same length as the descriptor describes, different bytes.
	desc := descriptorFor("sysext contents")
	tampered := "SYSEXT CONTENTS"
	require.Equal(t, desc.Size, int64(len(tampered)), "test requires an equal-length substitution")

	rc := newVerifyingReadCloser(nopCloser{strings.NewReader(tampered)}, desc, "blob.raw")

	_, err := io.ReadAll(rc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verify OCI blob")
	assert.Contains(t, err.Error(), "blob.raw")
}

// TestVerifyingReadCloserRejectsTruncatedContent covers a short read, which
// must not be mistaken for a clean end of stream.
func TestVerifyingReadCloserRejectsTruncatedContent(t *testing.T) {
	t.Parallel()

	desc := descriptorFor("sysext contents")
	rc := newVerifyingReadCloser(nopCloser{strings.NewReader("sysext")}, desc, "blob.raw")

	_, err := io.ReadAll(rc)
	require.Error(t, err)
	assert.True(t,
		errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "verify OCI blob"),
		"expected a truncation or verification failure, got %v", err)
}

// TestVerifyingReadCloserRejectsOverlongContent covers extra trailing bytes.
func TestVerifyingReadCloserRejectsOverlongContent(t *testing.T) {
	t.Parallel()

	desc := descriptorFor("sysext")
	rc := newVerifyingReadCloser(nopCloser{strings.NewReader("sysext and then some")}, desc, "blob.raw")

	_, err := io.ReadAll(rc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verify OCI blob")
}

// TestVerifyingReadCloserSurfacesFailureThroughRead pins the design choice:
// verification failures must reach callers through Read, because Close errors
// are widely ignored and a silent Close failure would let corrupt content
// through.
func TestVerifyingReadCloserSurfacesFailureThroughRead(t *testing.T) {
	t.Parallel()

	desc := descriptorFor("sysext contents")
	rc := newVerifyingReadCloser(nopCloser{strings.NewReader("SYSEXT CONTENTS")}, desc, "blob.raw")

	buf := make([]byte, 64)

	var readErr error

	for {
		_, err := rc.Read(buf)
		if err != nil {
			readErr = err

			break
		}
	}

	require.Error(t, readErr)
	assert.NotErrorIs(t, readErr, io.EOF, "a digest mismatch must not present as a clean EOF")
	assert.NoError(t, rc.Close(), "Close should not be where the failure is reported")
}
