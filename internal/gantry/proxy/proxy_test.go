// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package proxy

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/listener"
)

func TestServeRequiresTCPToUnix(t *testing.T) {
	tests := []struct {
		name     string
		listen   string
		upstream string
	}{
		{name: "Unix listener", listen: "unix:///tmp/listen.sock", upstream: "unix:///tmp/upstream.sock"},
		{name: "TCP upstream", listen: "127.0.0.1:0", upstream: "127.0.0.1:5000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Serve(context.Background(), tt.listen, tt.upstream); err == nil {
				t.Fatal("Serve accepted an invalid proxy direction")
			}
		})
	}
}

func TestBridgeRoundTrip(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "upstream.sock")

	upstreamListener, err := listener.Listen("unix://" + socketPath)
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	defer upstreamListener.Close() //nolint:errcheck // test cleanup

	go func() {
		conn, acceptErr := upstreamListener.Accept()
		if acceptErr != nil {
			return
		}

		defer conn.Close() //nolint:errcheck // test cleanup

		_, _ = io.Copy(conn, conn) //nolint:errcheck // connection close ends the echo loop
	}()

	client, downstream := net.Pipe()
	defer client.Close() //nolint:errcheck // test cleanup

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go bridge(ctx, downstream, &net.Dialer{}, "unix", socketPath)

	want := []byte("gantry")
	if _, err := client.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make([]byte, len(want))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(got) != string(want) {
		t.Fatalf("round trip = %q, want %q", got, want)
	}
}
