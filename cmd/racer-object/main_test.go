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
	"os"
	"path/filepath"
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
	for _, args := range [][]string{{"--help"}, {"--version"}, {"origin", "--help"}, {"sidecar", "--help"}, {"origin", "--version"}, {"sidecar", "--version"}} {
		err := run(context.Background(), args, failingOutput{})
		if err == nil || strings.Contains(err.Error(), "private-output-error") {
			t.Fatal("output failure was lost or leaked")
		}
	}
}

// testCacheArg names the cache in tests that do not exercise --cache itself.
const testCacheArg = "--cache=test-cache"

// modeArgs returns the minimal valid arguments for mode followed by extra.
func modeArgs(mode string, extra ...string) []string {
	args := []string{mode, testCacheArg}
	if mode == "origin" {
		args = append(args, "--bucket=bucket")
	}

	return append(args, extra...)
}

func TestOptionsDefaults(t *testing.T) {
	for _, mode := range []string{"origin", "sidecar"} {
		t.Run(mode, func(t *testing.T) {
			o, err := parseOptions(modeArgs(mode), io.Discard)
			if err != nil {
				t.Fatal(err)
			}

			if o.cache != "test-cache" || (mode == "origin") != (o.bucket == "bucket") {
				t.Fatalf("unexpected shared defaults: %+v", o)
			}

			if mode == "origin" {
				if o.requestTimeout != time.Minute || o.metadataTTL != 30*time.Second || !o.pathStyle || o.endpoint != "" || o.region != "" || o.maxRequests != 512 {
					t.Fatalf("unexpected origin defaults: %+v", o)
				}
			} else if o.listen != "127.0.0.1:8080" || o.requestTimeout != 5*time.Minute || o.maxConnections != 128 {
				t.Fatalf("unexpected sidecar defaults: %+v", o)
			}
		})
	}

	o, err := parseOptions([]string{"origin", "--bucket=one", "--cache=custom", "--metadata-ttl=0", "--path-style=false", "--region=us-west-2", "--endpoint=https://s3.example/", "--request-timeout=2s"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	if o.bucket != "one" || o.cache != "custom" || o.metadataTTL != 0 || o.pathStyle || o.region != "us-west-2" || o.requestTimeout != 2*time.Second {
		t.Fatalf("flags not applied: %+v", o)
	}

	o, err = parseOptions([]string{"origin", testCacheArg, "--bucket=bucket", "--max-concurrent-requests=1"}, io.Discard)
	if err != nil || o.maxRequests != 1 {
		t.Fatalf("origin concurrency not applied: %+v, %v", o, err)
	}

	o, err = parseOptions([]string{"sidecar", testCacheArg, "--max-connections=65536"}, io.Discard)
	if err != nil || o.maxConnections != 65536 {
		t.Fatalf("sidecar connections not applied: %+v, %v", o, err)
	}
}

func TestCacheFlag(t *testing.T) {
	for _, mode := range []string{"origin", "sidecar"} {
		t.Run(mode, func(t *testing.T) {
			for _, name := range []string{"custom", "a-b.c0", strings.Repeat("a", 63) + "." + strings.Repeat("b", 18)} {
				o, err := parseOptions(modeArgs(mode, "--cache="+name), io.Discard)
				if err != nil || o.cache != name {
					t.Fatalf("valid cache %q: options=%+v err=%v", name, o, err)
				}
			}

			for _, name := range []string{"../bad", "Upper", strings.Repeat("a", 64), strings.Repeat("a", 63) + "." + strings.Repeat("b", 19)} {
				_, err := parseOptions(modeArgs(mode, "--cache="+name), io.Discard)
				if err == nil || err.Error() != "invalid --cache" {
					t.Fatalf("invalid cache %q: %v", name, err)
				}
			}

			// There is no default cache, so omitting --cache or passing an
			// empty value must fail rather than bind to an implicit cache.
			for _, cacheArgs := range [][]string{nil, {"--cache="}} {
				args := append([]string{mode}, cacheArgs...)
				if mode == "origin" {
					args = append(args, "--bucket=bucket")
				}

				_, err := parseOptions(args, io.Discard)
				if err == nil || err.Error() != "--cache is required" {
					t.Fatalf("missing cache %q: %v", args, err)
				}
			}

			var output bytes.Buffer

			_, err := parseOptions([]string{mode, "--help"}, &output)
			if !errors.Is(err, flag.ErrHelp) || !strings.Contains(output.String(), "-cache") || !strings.Contains(output.String(), "required") {
				t.Fatalf("unexpected help: %q, %v", output.String(), err)
			}
		})
	}
}

func TestCacheUIDFlag(t *testing.T) {
	o, err := parseOptions([]string{"origin", testCacheArg, "--bucket=bucket"}, io.Discard)
	if err != nil || o.cacheUID != "" {
		t.Fatalf("default cache UID: options=%+v err=%v", o, err)
	}

	const uid = "00000000-0000-4000-8000-00000000000a"

	o, err = parseOptions([]string{"origin", testCacheArg, "--bucket=bucket", "--cache-uid=" + uid}, io.Discard)
	if err != nil || o.cacheUID != uid {
		t.Fatalf("cache UID: options=%+v err=%v", o, err)
	}

	_, err = parseOptions([]string{"origin", testCacheArg, "--bucket=bucket", "--cache-uid=bad"}, io.Discard)
	if err == nil || err.Error() != "invalid --cache-uid" {
		t.Fatalf("invalid cache UID: %v", err)
	}
}

func TestOptionsErrors(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"unknown"},
		{"origin", testCacheArg},
		{"origin", testCacheArg, "--bucket=has space"},
		{"origin", testCacheArg, "--bucket=a/b"},
		{"origin", testCacheArg, "--bucket=one", "--bucket=two"},
		{"origin", testCacheArg, "--bucket=bucket", "--namespace=store"},
		{"sidecar", testCacheArg, "--namespace=store"},
		{"sidecar", testCacheArg, "--bucket=bucket"},
		{"sidecar", testCacheArg, "--cache=../bad"},
		{"origin", testCacheArg, "--bucket="},
		{"sidecar", testCacheArg, "--bucket=../bad"},
		{"origin", testCacheArg, "--bucket=bucket", "--request-timeout=0"},
		{"sidecar", testCacheArg, "--request-timeout=-1s"},
		{"origin", testCacheArg, "--bucket=bucket", "--metadata-ttl=-1s"},
		{"origin", testCacheArg, "--bucket=bucket", "--metadata-ttl=bad"},
		{"origin", testCacheArg, "--bucket=bucket", "--listen=127.0.0.1:0"},
		{"sidecar", testCacheArg, "--region=us-east-1"},
		{"sidecar", testCacheArg, "extra"},
		{"sidecar", testCacheArg, "--listen=0.0.0.0:8080"},
		{"origin", testCacheArg, "--bucket=bucket", "--max-concurrent-requests=0"},
		{"origin", testCacheArg, "--bucket=bucket", "--max-concurrent-requests=65537"},
		{"origin", testCacheArg, "--bucket=bucket", "--max-connections=128"},
		{"sidecar", testCacheArg, "--max-connections=0"},
		{"sidecar", testCacheArg, "--max-connections=-1"},
		{"sidecar", testCacheArg, "--max-concurrent-requests=512"},
		{"origin", testCacheArg, "--bucket=bucket", "--cache-uid=not-a-uid"},
		{"origin", testCacheArg, "--bucket=bucket", "--cache-uid=00000000-0000-4000-8000-00000000000A"},
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
	for _, args := range [][]string{{"--help"}, {"-h"}, {"origin", "--help"}, {"sidecar", "--help"}, {"--version"}, {"origin", "--version"}, {"sidecar", "--version"}} {
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
		{"origin", testCacheArg, "--bucket=bucket", "--endpoint=https://private-token@upstream/"},
		{"origin", testCacheArg, "--bucket=bucket", "--request-timeout=private-token"},
		{"sidecar", testCacheArg, "--private-token"},
	} {
		var output bytes.Buffer

		err := run(context.Background(), args, &output)
		if err == nil || strings.Contains(err.Error()+output.String(), "private-token") {
			t.Fatal("argument leaked or invalid argument accepted")
		}
	}

	if _, err := parseOptions([]string{"origin", "--help"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatal("help not recognized")
	}
}

func TestEndpointValidation(t *testing.T) {
	for _, value := range []string{"", "https://s3.example", "http://localhost:9000/", "http://127.0.0.1:80", "https://[::1]:443"} {
		if err := validateEndpoint(value); err != nil {
			t.Errorf("rejected %q: %v", value, err)
		}
	}

	for _, value := range []string{
		"s3.example", "ftp://s3.example", "https://", "https://user:password@s3.example", "https://s3.example/prefix", "https://s3.example/%2f",
		"https://s3.example?token=value", "https://s3.example?", "https://s3.example#fragment", "https://s3.example#", "https://s3.example:0", "https://s3.example:65536", "https://s3.example:abc", "https://s3.example:",
	} {
		if err := validateEndpoint(value); err == nil {
			t.Errorf("accepted %q", value)
		}
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

func TestAWSRegionAndCredentialChain(t *testing.T) {
	dir := t.TempDir()
	for key, value := range map[string]string{
		"AWS_CONFIG_FILE": filepath.Join(dir, "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"),
		"AWS_PROFILE": "", "AWS_DEFAULT_PROFILE": "", "AWS_REGION": "", "AWS_DEFAULT_REGION": "", "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_ACCESS_KEY_ID": "test-access", "AWS_SECRET_ACCESS_KEY": "test-secret", "AWS_SESSION_TOKEN": "test-session",
	} {
		t.Setenv(key, value)
	}

	o := options{requestTimeout: time.Second}
	if _, err := originAWSConfig(context.Background(), o); err == nil {
		t.Fatal("accepted missing region")
	}

	t.Setenv("AWS_REGION", "us-east-1")

	cfg, err := originAWSConfig(context.Background(), o)
	if err != nil || cfg.Region != "us-east-1" {
		t.Fatalf("environment region: %v", err)
	}

	creds, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil || creds.AccessKeyID != "test-access" || creds.SessionToken != "test-session" {
		t.Fatal("default credential chain not used")
	}

	o.region = "us-west-2"

	cfg, err = originAWSConfig(context.Background(), o)
	if err != nil || cfg.Region != "us-west-2" {
		t.Fatalf("explicit region: %v", err)
	}

	if cfg.HTTPClient.(*http.Client).Timeout != time.Second {
		t.Fatal("upstream HTTP timeout missing")
	}

	o.region = ""

	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "eu-west-1")

	cfg, err = originAWSConfig(context.Background(), o)
	if err != nil || cfg.Region != "eu-west-1" {
		t.Fatalf("default environment region: %v", err)
	}

	badConfig := filepath.Join(dir, "bad-config")
	if err := os.WriteFile(badConfig, []byte("[default]\nmax_attempts = not-a-number\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("AWS_CONFIG_FILE", badConfig)

	if _, err := originAWSConfig(context.Background(), o); err == nil {
		t.Fatal("accepted invalid AWS configuration")
	}
}

func TestCheckOriginDirectory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, cache string) []string
		missing bool
		ok      bool
	}{
		{name: "existing", ok: true, setup: func(t *testing.T, cache string) []string {
			mkdir(t, cache, 0o755)
			mkdir(t, filepath.Join(cache, "origin"), 0o700)

			return []string{"cache", "origin"}
		}},
		{name: "missing leaf", missing: true, setup: func(t *testing.T, cache string) []string {
			mkdir(t, cache, 0o755)

			return []string{"cache", "origin"}
		}},
		{name: "missing parent", missing: true, setup: func(t *testing.T, cache string) []string {
			return []string{"cache", "origin"}
		}},
		{name: "writable leaf", setup: func(t *testing.T, cache string) []string {
			mkdir(t, cache, 0o755)
			mkdir(t, filepath.Join(cache, "origin"), 0o777)

			return []string{"cache", "origin"}
		}},
		{name: "writable parent", setup: func(t *testing.T, cache string) []string {
			mkdir(t, cache, 0o777)
			mkdir(t, filepath.Join(cache, "origin"), 0o700)

			return []string{"cache", "origin"}
		}},
		{name: "symlink leaf", setup: func(t *testing.T, cache string) []string {
			mkdir(t, cache, 0o755)

			target := t.TempDir()
			if err := os.Chmod(target, 0o700); err != nil {
				t.Fatal(err)
			}

			if err := os.Symlink(target, filepath.Join(cache, "origin")); err != nil {
				t.Fatal(err)
			}

			return []string{"cache", "origin"}
		}},
		{name: "symlink parent", setup: func(t *testing.T, cache string) []string {
			target := t.TempDir()
			mkdir(t, filepath.Join(target, "origin"), 0o700)

			if err := os.Symlink(target, cache); err != nil {
				t.Fatal(err)
			}

			return []string{"cache", "origin"}
		}},
		{name: "file leaf", setup: func(t *testing.T, cache string) []string {
			mkdir(t, cache, 0o755)

			if err := os.WriteFile(filepath.Join(cache, "origin"), nil, 0o600); err != nil {
				t.Fatal(err)
			}

			return []string{"cache", "origin"}
		}},
		{name: "traversal", setup: func(t *testing.T, cache string) []string {
			return []string{"..", "escape"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}

			parent, err := os.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()

			cache := filepath.Join(dir, "cache")
			parts := tc.setup(t, cache)

			before, _ := os.Lstat(cache)

			err = checkOriginDirectory(parent, parts)
			if tc.ok != (err == nil) {
				t.Fatalf("ok=%v, err=%v", tc.ok, err)
			}

			if tc.missing != errors.Is(err, errOriginDirectoryMissing) {
				t.Fatalf("missing=%v, err=%v", tc.missing, err)
			}

			// The origin never creates or repairs directories.
			after, _ := os.Lstat(cache)
			if (before == nil) != (after == nil) || (before != nil && before.Mode() != after.Mode()) {
				t.Fatal("changed the directory tree")
			}

			if _, statErr := os.Lstat(filepath.Join(cache, "origin")); tc.missing && !os.IsNotExist(statErr) {
				t.Fatal("created the origin directory")
			}
		})
	}
}

func TestCheckOriginDirectoryRejectsForeignLeaf(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to chown")
	}

	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, "origin"), 0o700)

	if err := os.Chown(filepath.Join(dir, "origin"), originUID, originUID); err != nil {
		t.Fatal(err)
	}

	parent, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()

	if err := checkOriginDirectory(parent, []string{"origin", testCacheArg}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("accepted origin directory owned by another user: %v", err)
	}
}

func mkdir(t *testing.T, path string, mode os.FileMode) {
	t.Helper()

	if err := os.Mkdir(path, mode); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
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

func TestOriginHTTPClientPoolsUpstreamConnections(t *testing.T) {
	first := originHTTPClient(options{requestTimeout: 3 * time.Second, maxRequests: 512})
	second := originHTTPClient(options{requestTimeout: time.Second, maxRequests: 1})

	transport, ok := first.Transport.(*http.Transport)
	if !ok || first.Timeout != 3*time.Second || transport.MaxIdleConns != 512 || transport.MaxIdleConnsPerHost != 512 {
		t.Fatalf("unexpected origin client: %+v", first)
	}

	if transport == http.DefaultTransport || transport == second.Transport {
		t.Fatal("origin clients must not share or mutate the default transport")
	}

	if http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost != 0 {
		t.Fatal("default transport was mutated")
	}
}
