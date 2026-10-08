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

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/Azure/unbounded/internal/racer/object"
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

func TestOptionsDefaults(t *testing.T) {
	for _, mode := range []string{"origin", "sidecar"} {
		t.Run(mode, func(t *testing.T) {
			o, err := parseOptions([]string{mode, "--namespace", "store"}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}

			if o.cache != "racer-object" || o.namespace != "store" || len(o.buckets) != 0 {
				t.Fatalf("unexpected shared defaults: %+v", o)
			}

			if mode == "origin" {
				if o.requestTimeout != time.Minute || o.metadataTTL != 30*time.Second || !o.pathStyle || o.endpoint != "" || o.region != "" {
					t.Fatalf("unexpected origin defaults: %+v", o)
				}
			} else if o.listen != "127.0.0.1:8080" || o.requestTimeout != 5*time.Minute {
				t.Fatalf("unexpected sidecar defaults: %+v", o)
			}
		})
	}

	o, err := parseOptions([]string{"origin", "--namespace=store", "--bucket=one", "--bucket=two", "--cache=custom", "--metadata-ttl=0", "--path-style=false", "--region=us-west-2", "--endpoint=https://s3.example/", "--request-timeout=2s"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(o.buckets, ",") != "one,two" || o.cache != "custom" || o.metadataTTL != 0 || o.pathStyle || o.region != "us-west-2" || o.requestTimeout != 2*time.Second {
		t.Fatalf("flags not applied: %+v", o)
	}
}

func TestRequestTimeoutFlag(t *testing.T) {
	for _, tt := range []struct {
		mode    string
		value   string
		want    time.Duration
		wantErr string
	}{
		{mode: "origin", value: "1ns", want: time.Nanosecond},
		{mode: "origin", value: "30s", want: 30 * time.Second},
		{mode: "origin", value: "1m", want: time.Minute},
		{mode: "origin", value: "61s", wantErr: "--request-timeout must not exceed 1m in origin mode"},
		{mode: "origin", value: "2m", wantErr: "--request-timeout must not exceed 1m in origin mode"},
		{mode: "sidecar", value: "5m", want: 5 * time.Minute},
		{mode: "sidecar", value: "10m", want: 10 * time.Minute},
	} {
		t.Run(tt.mode+"/"+tt.value, func(t *testing.T) {
			o, err := parseOptions([]string{tt.mode, "--namespace=store", "--request-timeout=" + tt.value}, io.Discard)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if o.requestTimeout != tt.want {
				t.Fatalf("request timeout = %v, want %v", o.requestTimeout, tt.want)
			}
		})
	}
}

func TestCacheFlag(t *testing.T) {
	for _, mode := range []string{"origin", "sidecar"} {
		t.Run(mode, func(t *testing.T) {
			for _, name := range []string{"custom", "a-b.c0", strings.Repeat("a", 63) + "." + strings.Repeat("b", 18)} {
				o, err := parseOptions([]string{mode, "--namespace=store", "--cache=" + name}, io.Discard)
				if err != nil || o.cache != name {
					t.Fatalf("valid cache %q: options=%+v err=%v", name, o, err)
				}
			}

			for _, name := range []string{"", "../bad", "Upper", strings.Repeat("a", 64), strings.Repeat("a", 63) + "." + strings.Repeat("b", 19)} {
				_, err := parseOptions([]string{mode, "--namespace=store", "--cache=" + name}, io.Discard)
				if err == nil || err.Error() != "invalid --cache" {
					t.Fatalf("invalid cache %q: %v", name, err)
				}
			}

			_, err := parseOptions([]string{mode, "--namespace=store", "--volume=custom", "--cache=custom"}, io.Discard)
			if err == nil || err.Error() != "invalid flags (use --help)" {
				t.Fatalf("unsupported flag was not rejected: %v", err)
			}

			var output bytes.Buffer

			_, err = parseOptions([]string{mode, "--help"}, &output)
			if !errors.Is(err, flag.ErrHelp) || !strings.Contains(output.String(), "-cache") || strings.Contains(output.String(), "-volume") {
				t.Fatalf("unexpected help: %q, %v", output.String(), err)
			}
		})
	}
}

func TestOptionsErrors(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"unknown"},
		{"origin"},
		{"sidecar"},
		{"origin", "--namespace=has space"},
		{"sidecar", "--namespace=store", "--cache=../bad"},
		{"origin", "--namespace=store", "--bucket="},
		{"sidecar", "--namespace=store", "--bucket=../bad"},
		{"origin", "--namespace=store", "--request-timeout=0"},
		{"sidecar", "--namespace=store", "--request-timeout=-1s"},
		{"origin", "--namespace=store", "--metadata-ttl=-1s"},
		{"origin", "--namespace=store", "--metadata-ttl=bad"},
		{"origin", "--namespace=store", "--listen=127.0.0.1:0"},
		{"sidecar", "--namespace=store", "--region=us-east-1"},
		{"sidecar", "--namespace=store", "extra"},
		{"sidecar", "--namespace=store", "--listen=0.0.0.0:8080"},
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
		{"origin", "--namespace=store", "--endpoint=https://private-token@upstream/"},
		{"origin", "--namespace=store", "--request-timeout=private-token"},
		{"sidecar", "--namespace=store", "--private-token"},
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

func TestOriginAWSConfigRequiredChecksums(t *testing.T) {
	dir := t.TempDir()
	for key, value := range map[string]string{
		"AWS_CONFIG_FILE": filepath.Join(dir, "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"),
		"AWS_PROFILE": "", "AWS_DEFAULT_PROFILE": "", "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_ACCESS_KEY_ID": "test-access", "AWS_SECRET_ACCESS_KEY": "test-secret", "AWS_SESSION_TOKEN": "test-session",
		"AWS_REQUEST_CHECKSUM_CALCULATION": "when_supported", "AWS_RESPONSE_CHECKSUM_VALIDATION": "when_supported",
	} {
		t.Setenv(key, value)
	}

	o := options{region: "us-east-1", requestTimeout: time.Second}

	cfg, err := originAWSConfig(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired {
		t.Errorf("request checksum calculation = %v, want WhenRequired", cfg.RequestChecksumCalculation)
	}

	if cfg.ResponseChecksumValidation != aws.ResponseChecksumValidationWhenRequired {
		t.Errorf("response checksum validation = %v, want WhenRequired", cfg.ResponseChecksumValidation)
	}
}

func TestPrepareOriginDirectory(t *testing.T) {
	for _, scenario := range []string{"create", "existing", "symlink", "writable", "file", "traversal"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}

			parent, err := os.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()

			path := filepath.Join(dir, "cache")
			parts := []string{"cache", "origin"}

			switch scenario {
			case "existing", "writable":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}

				if scenario == "writable" {
					if err := os.Chmod(path, 0o777); err != nil {
						t.Fatal(err)
					}
				}
			case "symlink":
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "traversal":
				parts = []string{"..", "escape"}
			}

			err = prepareOriginDirectory(parent, parts)
			if scenario != "create" && scenario != "existing" {
				if err == nil {
					t.Fatal("accepted unsafe directory")
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if err := prepareOriginDirectory(parent, parts); err != nil {
				t.Fatal("not idempotent", err)
			}

			info, err := os.Stat(filepath.Join(path, "origin"))
			if err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
				t.Fatal("unsafe created directory")
			}

			if scenario == "existing" {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0o700 {
					t.Fatal("changed existing permissions")
				}
			}
		})
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

	go func() { done <- serveSidecar(ctx, l, handler, timeout) }()

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

	handler, err := object.NewSidecar(client, object.SidecarConfig{Namespace: "store"})
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

func TestSidecarTimeoutResponse(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	address, cancel, done := startTestSidecar(t, handler, 100*time.Millisecond)
	defer func() {
		cancel()
		waitSidecar(t, done)
	}()

	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()

	response, err := client.Get(address + "/bucket/key")
	if err != nil {
		t.Fatalf("timeout response failed: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
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

	if err := serveSidecar(context.Background(), l, http.NotFoundHandler(), time.Second); err == nil {
		t.Fatal("lost listener failure")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := run(ctx, []string{"sidecar", "--namespace=store"}, io.Discard); err != nil {
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

	handler, err := object.NewSidecar(client, object.SidecarConfig{Namespace: "store"})
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
