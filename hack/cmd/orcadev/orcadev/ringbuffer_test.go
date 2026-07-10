// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"strings"
	"sync"
	"testing"
)

func TestRingBuffer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		cap    int
		writes []string
		want   string
	}{
		{
			name:   "smaller than capacity",
			cap:    8,
			writes: []string{"abc"},
			want:   "abc",
		},
		{
			name:   "exact fill no wrap",
			cap:    4,
			writes: []string{"abcd"},
			want:   "abcd",
		},
		{
			name:   "wrap once",
			cap:    4,
			writes: []string{"abcd", "ef"},
			want:   "cdef",
		},
		{
			name:   "single oversize write retains tail",
			cap:    4,
			writes: []string{"abcdefgh"},
			want:   "efgh",
		},
		{
			name:   "multiple writes wrap",
			cap:    6,
			writes: []string{"abcd", "efgh", "ij"},
			want:   "efghij",
		},
		{
			name:   "byte-by-byte fill then overflow",
			cap:    3,
			writes: []string{"a", "b", "c", "d", "e"},
			want:   "cde",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rb := newRingBuffer(tt.cap)
			for _, w := range tt.writes {
				n, err := rb.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write(%q) error = %v", w, err)
				}

				if n != len(w) {
					t.Fatalf("Write(%q) n = %d, want %d", w, n, len(w))
				}
			}

			if got := rb.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRingBufferLargeAccumulationStaysBounded(t *testing.T) {
	t.Parallel()

	const capBytes = 1024

	rb := newRingBuffer(capBytes)

	chunk := strings.Repeat("x", 4096)
	for i := 0; i < 1000; i++ {
		_, _ = rb.Write([]byte(chunk))
	}

	got := rb.String()
	if len(got) != capBytes {
		t.Fatalf("ring length = %d, want %d", len(got), capBytes)
	}
}

func TestRingBufferConcurrentWriteAndRead(t *testing.T) {
	t.Parallel()

	rb := newRingBuffer(64)
	start := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		<-start

		for i := 0; i < 1000; i++ {
			_, _ = rb.Write([]byte("abcdefghijklmnopqrstuvwxyz"))
		}
	}()

	close(start)

	for i := 0; i < 1000; i++ {
		_ = rb.String()
	}

	wg.Wait()
}
