// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

func TestClientPageSizeConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		in   int
		want uint64
		ok   bool
	}{
		"default":        {0, PageSize, true},
		"4 KiB":          {4 << 10, 4 << 10, true},
		"64 KiB":         {64 << 10, 64 << 10, true},
		"16 MiB":         {16 << 20, 16 << 20, true},
		"below minimum":  {4<<10 - 1, 0, false},
		"not power of 2": {6 << 10, 0, false},
		"above maximum":  {32 << 20, 0, false},
		"negative":       {-1, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := newClient(ClientConfig{Cache: "blobs", PageSize: tc.in}, "/nonexistent/socket")
			if !tc.ok {
				assertIs(t, err, ErrInvalidRequest)
				return
			}

			if err != nil {
				t.Fatal(err)
			}

			defer c.Close()

			if c.pageSize != tc.want {
				t.Fatalf("page size = %d, want %d", c.pageSize, tc.want)
			}
		})
	}
}

func TestGetUsesConfiguredPageSize(t *testing.T) {
	for _, pageSize := range []int{4 << 10, 64 << 10} {
		t.Run(fmt.Sprintf("%d bytes", pageSize), func(t *testing.T) {
			first := uint64(pageSize - 1)
			end := uint64(pageSize + 2)

			c := rawServerSize(t, pageSize, func(conn net.Conn, r *bufio.Reader, head []byte) {
				if !strings.Contains(string(head), "Racer-Page-Credits: 2\r\n") ||
					!strings.Contains(string(head), fmt.Sprintf("Racer-Byte-Credits: %d\r\n", 2*pageSize)) {
					t.Errorf("subscription must grant two pages and %d bytes: %q", 2*pageSize, head)
					return
				}

				if _, err := io.WriteString(conn, subscriptionHeadSize(end, first, end, uint64(pageSize))); err != nil {
					return
				}

				for number, payload := range []string{"a", "bc"} {
					offset := first
					if number == 1 {
						offset = uint64(pageSize)
					}

					if err := writeFrame(conn, wire.PageFrame, uint64(number), offset, uint32(len(payload))); err != nil {
						return
					}

					if _, err := io.WriteString(conn, payload); err != nil {
						return
					}
				}

				for number, length := range []uint32{1, 2} {
					if err := readCredit(r, uint64(number), length); err != nil {
						t.Error(err)
						return
					}
				}

				_ = writeFrame(conn, wire.CompleteFrame, 2, end-first, 0)
			})

			o, err := c.Get(t.Context(), Request{}, ReadOptions{Offset: int64(first), Length: int64(end - first)})
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(o)

			var b strings.Builder
			if _, err := io.Copy(writerOnly{&b}, readerOnly{o}); err != nil || b.String() != "abc" {
				t.Fatalf("received %q, %v; want abc", b.String(), err)
			}
		})
	}
}

func TestGetRangesWithConfiguredPageSize(t *testing.T) {
	for _, pageSize := range []int{4 << 10, 64 << 10} {
		t.Run(fmt.Sprintf("%d bytes", pageSize), func(t *testing.T) {
			size := int64(2*pageSize + 1000)
			c := fakeClientSize(t, pageSize, offsetOrigin(size))

			for _, tc := range []struct {
				name           string
				offset, length int64
			}{
				{"whole", 0, 0},
				{"within page", 7, 100},
				{"across pages", int64(pageSize - 3), 10},
				{"second page", int64(pageSize), 0},
				{"last byte", size - 1, 1},
				{"exact length", 0, size},
			} {
				t.Run(tc.name, func(t *testing.T) {
					want := tc.length
					if want == 0 {
						want = size - tc.offset
					}

					o, err := c.Get(t.Context(), Request{}, ReadOptions{Offset: tc.offset, Length: tc.length})
					if err != nil {
						t.Fatal(err)
					}
					defer closeQuietly(o)

					sink := &offsetSink{offset: tc.offset}

					n, err := o.WriteTo(writerOnly{sink})
					if err != nil || n != want {
						t.Fatalf("n=%d err=%v; want %d bytes", n, err, want)
					}

					if got := o.Progress(); got != want {
						t.Fatalf("Progress() = %d, want %d", got, want)
					}
				})
			}
		})
	}
}

func TestOriginPageSizeConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		in   int
		want uint64
		ok   bool
	}{
		"default":        {0, PageSize, true},
		"4 KiB":          {4 << 10, 4 << 10, true},
		"64 KiB":         {64 << 10, 64 << 10, true},
		"below minimum":  {4<<10 - 1, 0, false},
		"not power of 2": {6 << 10, 0, false},
		"above maximum":  {32 << 20, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			limits, err := (OriginConfig{Cache: "test", PageSize: tc.in}).limits()
			if !tc.ok {
				assertIs(t, err, ErrInvalidRequest)
				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if limits.pageSize != tc.want {
				t.Fatalf("page size = %d, want %d", limits.pageSize, tc.want)
			}
		})
	}

	request := newOriginRequest(wire.Request{Operation: wire.OperationBootstrap, Range: wire.BootstrapRange(64 << 10)}, 64<<10)
	if request.Length != 64<<10 {
		t.Fatalf("bootstrap length = %d, want %d", request.Length, 64<<10)
	}
}

func TestSmallObjectUsesConfiguredPageSize(t *testing.T) {
	for _, pageSize := range []int{4 << 10, 64 << 10} {
		t.Run(fmt.Sprintf("%d bytes", pageSize), func(t *testing.T) {
			atLimit := fakeClientSize(t, pageSize, offsetOrigin(int64(pageSize)))

			o, err := atLimit.Get(t.Context(), Request{}, ReadOptions{SmallObject: true})
			if err != nil {
				t.Fatal(err)
			}

			if _, err := io.Copy(io.Discard, o); err != nil {
				t.Fatal(err)
			}

			closeQuietly(o)

			overLimit := fakeClientSize(t, pageSize, offsetOrigin(int64(pageSize+1)))
			if _, err := overLimit.Get(t.Context(), Request{}, ReadOptions{SmallObject: true}); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("over-limit SmallObject error = %v; want ErrInvalidRequest", err)
			}
		})
	}
}
