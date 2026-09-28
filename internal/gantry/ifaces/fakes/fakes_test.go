// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fakes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
)

func TestCache_RoundTrip(t *testing.T) {
	body := []byte("hello gantry")
	d := mustDigest(body)
	c := NewCache()

	w, err := c.Writer(context.Background(), d)
	if err != nil {
		t.Fatalf("Writer: %v", err)
	}

	if _, err := w.Write(body); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := w.Commit(context.Background()); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	ok, err := c.Has(context.Background(), d)
	if err != nil || !ok {
		t.Fatalf("Has: ok=%v err=%v", ok, err)
	}

	r, n, err := c.Open(context.Background(), d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()

	if n != int64(len(body)) {
		t.Errorf("len = %d, want %d", n, len(body))
	}

	got, _ := io.ReadAll(r)
	if string(got) != string(body) {
		t.Errorf("body = %q, want %q", got, body)
	}
}

func TestCache_DigestMismatchRejectsCommit(t *testing.T) {
	c := NewCache()
	wrong := digest.MustParse("sha256:" + zeros(64))

	w, _ := c.Writer(context.Background(), wrong)
	if _, err := w.Write([]byte("not the right bytes")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := w.Commit(context.Background()); err == nil {
		t.Fatal("Commit succeeded with wrong digest")
	}

	ok, _ := c.Has(context.Background(), wrong)
	if ok {
		t.Error("entry was committed despite digest mismatch")
	}
}

func TestOriginPuller_NotFound(t *testing.T) {
	o := NewOriginPuller()

	_, _, err := o.Pull(context.Background(), ifaces.OriginRef{Digest: mustDigest([]byte("missing"))})
	if err == nil {
		t.Fatal("Pull on empty origin returned no error")
	}

	var oe *ifaces.OriginError
	if !errors.As(err, &oe) {
		t.Fatalf("error is %T, want *OriginError", err)
	}

	if oe.Class != ifaces.FailureNotFound {
		t.Errorf("class = %q, want not_found", oe.Class)
	}
}

func TestOriginPullerBoundedPages(t *testing.T) {
	o := NewOriginPuller()
	d := mustDigest([]byte("abcdef"))
	o.Put(d, []byte("abcdef"))

	for _, offset := range []int64{0, 4} {
		body, size, _, err := o.PullRange(context.Background(), ifaces.OriginRef{Digest: d, Offset: offset}, 4)
		if err != nil {
			t.Fatal(err)
		}

		got, err := io.ReadAll(body)
		_ = body.Close()

		want := "abcd"
		if offset == 4 {
			want = "ef"
		}

		if err != nil || string(got) != want || size != 6 {
			t.Fatalf("page=%q size=%d err=%v", got, size, err)
		}
	}

	if o.HeadCount(d) != 0 || o.PullCount(d) != 2 {
		t.Fatal("bounded reads performed HEAD or lost accounting")
	}

	_, _, _, err := o.PullRange(context.Background(), ifaces.OriginRef{Digest: d, Offset: 6}, 4)
	if err == nil {
		t.Fatal("accepted offset past EOF")
	}

	empty := mustDigest(nil)
	o.Put(empty, nil)

	body, size, _, err := o.PullRange(context.Background(), ifaces.OriginRef{Digest: empty}, 4)
	if err != nil || size != 0 {
		t.Fatalf("empty=%d %v", size, err)
	}

	_ = body.Close()
}

func mustDigest(b []byte) digest.Digest {
	sum := sha256.Sum256(b)
	return digest.MustParse("sha256:" + hex.EncodeToString(sum[:]))
}

func zeros(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = '0'
	}

	return string(out)
}
