// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/racerobject"
	"github.com/Azure/unbounded/internal/version"
	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

type failingOutput struct{}

func (failingOutput) Write([]byte) (int, error) {
	return 0, errors.New("private-output-error")
}

func TestHelpOutputFailure(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--version"}, {"sidecar", "--help"}, {"sidecar", "--version"}} {
		err := run(context.Background(), args, failingOutput{})
		if err == nil || strings.Contains(err.Error(), "private-output-error") {
			t.Fatal("output failure was lost or leaked")
		}
	}
}

// testCacheArg names the cache in tests that do not exercise --cache itself.
const testCacheArg = "--cache=test-cache"

func TestOptionsDefaults(t *testing.T) {
	o, err := parseOptions([]string{"sidecar", testCacheArg}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	if o.cache != "test-cache" || o.api != "s3" || o.listen != "127.0.0.1:8080" || o.requestTimeout != 5*time.Minute || o.maxConnections != 128 {
		t.Fatalf("unexpected sidecar defaults: %+v", o)
	}

	o, err = parseOptions([]string{"sidecar", testCacheArg, "--api=azure", "--max-connections=65536", "--request-timeout=2s"}, io.Discard)
	if err != nil || o.api != "azure" || o.maxConnections != 65536 || o.requestTimeout != 2*time.Second {
		t.Fatalf("sidecar flags not applied: %+v, %v", o, err)
	}
}

func TestCacheFlag(t *testing.T) {
	for _, name := range []string{"custom", "a-b.c0", strings.Repeat("a", 63) + "." + strings.Repeat("b", 18)} {
		o, err := parseOptions([]string{"sidecar", "--cache=" + name}, io.Discard)
		if err != nil || o.cache != name {
			t.Fatalf("valid cache %q: options=%+v err=%v", name, o, err)
		}
	}

	for _, name := range []string{"../bad", "Upper", strings.Repeat("a", 64), strings.Repeat("a", 63) + "." + strings.Repeat("b", 19)} {
		_, err := parseOptions([]string{"sidecar", "--cache=" + name}, io.Discard)
		if err == nil || err.Error() != "invalid --cache" {
			t.Fatalf("invalid cache %q: %v", name, err)
		}
	}

	// There is no default cache, so omitting --cache or passing an empty
	// value must fail rather than bind to an implicit cache.
	for _, args := range [][]string{{"sidecar"}, {"sidecar", "--cache="}} {
		_, err := parseOptions(args, io.Discard)
		if err == nil || err.Error() != "--cache is required" {
			t.Fatalf("missing cache %q: %v", args, err)
		}
	}

	var output bytes.Buffer

	_, err := parseOptions([]string{"sidecar", "--help"}, &output)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(output.String(), "-cache") || !strings.Contains(output.String(), "required") {
		t.Fatalf("unexpected help: %q, %v", output.String(), err)
	}
}

func TestOptionsErrors(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"unknown"},
		// The origin command was removed; Racer reads Azure Blob directly.
		{"origin", testCacheArg},
		{"sidecar", testCacheArg, "--namespace=store"},
		{"sidecar", testCacheArg, "--bucket=bucket"},
		{"sidecar", testCacheArg, "--cache=../bad"},
		{"sidecar", testCacheArg, "--request-timeout=-1s"},
		{"sidecar", testCacheArg, "--request-timeout=0"},
		{"sidecar", testCacheArg, "--region=us-east-1"},
		{"sidecar", testCacheArg, "--provider=azure"},
		{"sidecar", testCacheArg, "--container-url=https://account.blob.core.windows.net/container"},
		{"sidecar", testCacheArg, "--api=gcs"},
		{"sidecar", testCacheArg, "extra"},
		{"sidecar", testCacheArg, "--listen=0.0.0.0:8080"},
		{"sidecar", testCacheArg, "--max-connections=0"},
		{"sidecar", testCacheArg, "--max-connections=-1"},
		{"sidecar", testCacheArg, "--max-connections=65537"},
		{"sidecar", testCacheArg, "--max-concurrent-requests=512"},
		{"sidecar", testCacheArg, "--cache-uid=00000000-0000-4000-8000-00000000000a"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := parseOptions(args, io.Discard); err == nil {
				t.Fatal("accepted invalid flags")
			}
		})
	}
}

func TestHelpVersionAndRedaction(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"sidecar", "--help"}, {"--version"}, {"sidecar", "--version"}} {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output); err != nil {
			t.Fatal(err)
		}

		if output.Len() == 0 {
			t.Fatal("missing help or version output")
		}

		if args[len(args)-1] == "--version" && !strings.Contains(output.String(), version.String()) {
			t.Fatal("version metadata missing")
		}
	}

	for _, args := range [][]string{
		{"sidecar", testCacheArg, "--request-timeout=private-token"},
		{"sidecar", testCacheArg, "--private-token"},
	} {
		var output bytes.Buffer

		err := run(context.Background(), args, &output)
		if err == nil || strings.Contains(err.Error()+output.String(), "private-token") {
			t.Fatal("argument leaked or invalid argument accepted")
		}
	}

	if _, err := parseOptions([]string{"sidecar", "--help"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatal("help not recognized")
	}
}

func TestLoopbackValidation(t *testing.T) {
	for _, value := range []string{"127.0.0.1:0", "127.1.2.3:65535", "[::1]:8080", "[::ffff:127.0.0.1]:80"} {
		if err := validateListen(value); err != nil {
			t.Errorf("rejected %q: %v", value, err)
		}
	}

	for _, value := range []string{"localhost:8080", ":8080", "0.0.0.0:8080", "[::]:8080", "192.0.2.1:8080", "127.0.0.1", "127.0.0.1:", "127.0.0.1:http", "127.0.0.1:-1", "127.0.0.1:+80", "127.0.0.1:65536", "[::1%lo]:80"} {
		if _, err := listenSidecar(context.Background(), value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestSidecarLoopbackAndShutdown(t *testing.T) {
	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	handler, err := racerobject.NewSidecar(client, racerobject.SidecarConfig{})
	if err != nil {
		t.Fatal(err)
	}

	address, cancel, done := startTestSidecar(t, handler, time.Second)
	httpClient := &http.Client{Timeout: 2 * time.Second}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, address+"/bucket/key", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}

	response.Body.Close()

	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", response.StatusCode)
	}

	cancel()
	waitSidecar(t, done)

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(address, "http://"), time.Second)
	if err == nil {
		conn.Close()
		t.Fatal("listener remained open")
	}
}

func TestSidecarRequestCancellation(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "shutdown"}[shutdown], func(t *testing.T) {
			entered := make(chan struct{})
			released := make(chan struct{})
			handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(entered)
				<-r.Context().Done()
				close(released)
			})

			requestTimeout := 100 * time.Millisecond
			if shutdown {
				requestTimeout = time.Minute
			}

			address, cancel, done := startTestSidecar(t, handler, requestTimeout)
			requestDone := make(chan struct{})

			go func() {
				defer close(requestDone)

				client := &http.Client{Timeout: 3 * time.Second}

				resp, err := client.Get(address)
				if err == nil {
					resp.Body.Close()
				}
			}()

			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("request did not start")
			}

			if shutdown {
				cancel()
			}

			select {
			case <-released:
			case <-time.After(2 * time.Second):
				t.Fatal("request context not canceled")
			}

			cancel()
			waitSidecar(t, done)

			select {
			case <-requestDone:
			case <-time.After(4 * time.Second):
				t.Fatal("HTTP request did not stop")
			}
		})
	}
}

func TestSidecarListenerErrors(t *testing.T) {
	l, err := listenSidecar(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if other, err := listenSidecar(context.Background(), l.Addr().String()); err == nil {
		other.Close()
		t.Fatal("bound occupied port")
	}

	l.Close()

	if err := serveSidecar(context.Background(), l, http.NotFoundHandler(), time.Second, 128); err == nil {
		t.Fatal("lost listener failure")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := run(ctx, []string{"sidecar", testCacheArg}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

type closeSignalReader struct {
	*io.PipeReader
	closed chan struct{}
	once   sync.Once
}

func (r *closeSignalReader) Close() error {
	err := r.PipeReader.Close()
	r.once.Do(func() { close(r.closed) })

	return err
}

func TestSidecarShutdownReleasesSDKStream(t *testing.T) {
	metadata := racersdk.Metadata{Size: 8, ETag: `"version"`, ExpiresAt: time.Now().Add(time.Minute).Truncate(time.Millisecond)}
	reader, writer := io.Pipe()

	body := &closeSignalReader{PipeReader: reader, closed: make(chan struct{})}
	defer reader.Close()
	defer writer.Close()

	opened := make(chan struct{})

	client := racersdktest.NewClient(t, func(_ context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if request.Head {
			return metadata, nil, nil
		}

		close(opened)

		return metadata, body, nil
	})

	handler, err := racerobject.NewSidecar(client, racerobject.SidecarConfig{})
	if err != nil {
		t.Fatal(err)
	}

	address, cancel, done := startTestSidecar(t, handler, time.Minute)
	requestDone := make(chan struct{})

	go func() {
		defer close(requestDone)

		httpClient := &http.Client{Timeout: 5 * time.Second}

		response, err := httpClient.Get(address + "/bucket/key")
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()

	select {
	case <-opened:
	case <-time.After(3 * time.Second):
		t.Fatal("SDK stream not opened")
	}

	cancel()
	waitSidecar(t, done)

	select {
	case <-requestDone:
	case <-time.After(6 * time.Second):
		t.Fatal("stream request not released")
	}

	select {
	case <-body.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("origin stream body not closed")
	}
}

func startTestSidecar(t *testing.T, handler http.Handler, timeout time.Duration) (string, context.CancelFunc, <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	l, err := listenSidecar(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = l.Close() })

	done := make(chan error, 1)

	go func() { done <- serveSidecar(ctx, l, handler, timeout, 128) }()

	return "http://" + l.Addr().String(), cancel, done
}

func waitSidecar(t *testing.T, done <-chan error) {
	t.Helper()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("sidecar did not stop")
	}
}

func TestCreateSidecarAPISelection(t *testing.T) {
	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for _, api := range []string{"s3", "azure"} {
		o, err := parseOptions([]string{"sidecar", testCacheArg, "--api=" + api}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}

		handler, err := createSidecar(client, o)
		if err != nil {
			t.Fatal(err)
		}

		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/container/key", nil))

		if response.Code != http.StatusMethodNotAllowed || (response.Header().Get("x-ms-version") != "") != (api == "azure") {
			t.Fatalf("API %s: status=%d, headers=%v", api, response.Code, response.Header())
		}
	}
}

// modeArgs returns the minimal valid sidecar arguments followed by extra.
func modeArgs(mode string, extra ...string) []string {
	return append([]string{mode, testCacheArg}, extra...)
}
