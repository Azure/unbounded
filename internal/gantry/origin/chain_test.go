// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package origin_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/origin"
)

// staticPuller is an OriginPuller that always returns the same result.
type staticPuller struct {
	body   string
	size   int64
	err    error
	called int
}

func (s *staticPuller) Pull(_ context.Context, _ ifaces.OriginRef) (io.ReadCloser, int64, error) {
	s.called++
	if s.err != nil {
		return nil, 0, s.err
	}

	return io.NopCloser(bytes.NewBufferString(s.body)), s.size, nil
}

func (s *staticPuller) Head(_ context.Context, _ ifaces.OriginRef) (int64, string, error) {
	s.called++
	if s.err != nil {
		return 0, "", s.err
	}

	return s.size, "application/octet-stream", nil
}

var testRef = ifaces.OriginRef{
	Repository: "library/nginx",
	Digest:     digest.MustParse("sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"),
}

func notFound() *ifaces.OriginError {
	return &ifaces.OriginError{Ref: testRef, Class: ifaces.FailureNotFound}
}

func TestChain_EmptyChain_CallsFallback(t *testing.T) {
	fallback := &staticPuller{body: "from-fallback", size: 13}
	chain := origin.NewPriorityChain(nil, fallback, slog.Default())

	rc, size, err := chain.Pull(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	defer rc.Close()

	if size != 13 {
		t.Errorf("size = %d, want 13", size)
	}

	if fallback.called != 1 {
		t.Errorf("fallback called %d times, want 1", fallback.called)
	}
}

func TestChain_FirstEntryHit_FallbackNotCalled(t *testing.T) {
	first := &staticPuller{body: "from-cache", size: 10}
	fallback := &staticPuller{body: "from-fallback", size: 13}
	chain := origin.NewPriorityChain([]ifaces.OriginPuller{first}, fallback, slog.Default())

	rc, _, err := chain.Pull(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	_ = rc.Close()

	if first.called != 1 {
		t.Errorf("first called %d times, want 1", first.called)
	}

	if fallback.called != 0 {
		t.Errorf("fallback called %d times, want 0", fallback.called)
	}
}

func TestChain_FirstEntryMiss_SecondEntryTried(t *testing.T) {
	first := &staticPuller{err: notFound()}
	second := &staticPuller{body: "from-second", size: 11}
	fallback := &staticPuller{}
	chain := origin.NewPriorityChain([]ifaces.OriginPuller{first, second}, fallback, slog.Default())

	rc, _, err := chain.Pull(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	_ = rc.Close()

	if second.called != 1 {
		t.Errorf("second called %d times, want 1", second.called)
	}

	if fallback.called != 0 {
		t.Errorf("fallback called %d times, want 0", fallback.called)
	}
}

func TestChain_AllEntriesMiss_FallbackCalled(t *testing.T) {
	first := &staticPuller{err: notFound()}
	second := &staticPuller{err: notFound()}
	fallback := &staticPuller{body: "from-fallback", size: 13}
	chain := origin.NewPriorityChain([]ifaces.OriginPuller{first, second}, fallback, slog.Default())

	rc, _, err := chain.Pull(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	_ = rc.Close()

	if fallback.called != 1 {
		t.Errorf("fallback called %d times, want 1", fallback.called)
	}
}

func TestChain_AllFail_ReturnsFallbackError(t *testing.T) {
	fallbackErr := errors.New("registry down")
	first := &staticPuller{err: notFound()}
	fallback := &staticPuller{err: &ifaces.OriginError{Ref: testRef, Class: ifaces.FailureTransient, Err: fallbackErr}}
	chain := origin.NewPriorityChain([]ifaces.OriginPuller{first}, fallback, slog.Default())

	_, _, err := chain.Pull(context.Background(), testRef)
	if err == nil {
		t.Fatal("want error, got nil")
	}

	if !errors.Is(err, fallbackErr) {
		t.Errorf("want fallback error in chain, got %v", err)
	}
}

func TestChain_Head_EmptyChainCallsFallback(t *testing.T) {
	fallback := &staticPuller{size: 42}
	chain := origin.NewPriorityChain(nil, fallback, slog.Default())

	size, _, err := chain.Head(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}

	if size != 42 {
		t.Errorf("size = %d, want 42", size)
	}
}

func TestChain_Head_FirstEntryMiss_FallbackCalled(t *testing.T) {
	first := &staticPuller{err: notFound()}
	fallback := &staticPuller{size: 42}
	chain := origin.NewPriorityChain([]ifaces.OriginPuller{first}, fallback, slog.Default())

	size, _, err := chain.Head(context.Background(), testRef)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}

	if size != 42 {
		t.Errorf("size = %d, want 42", size)
	}

	if fallback.called != 1 {
		t.Errorf("fallback called %d times, want 1", fallback.called)
	}
}

// Compile-time check that PriorityChain implements ifaces.OriginPuller.
var _ ifaces.OriginPuller = (*origin.PriorityChain)(nil)
