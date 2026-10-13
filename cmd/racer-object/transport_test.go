// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func testOriginHTTPClient(t *testing.T) *awshttp.BuildableClient {
	t.Helper()

	isolateOriginAWSConfig(t)

	return configuredOriginHTTPClient(t)
}

func configuredOriginHTTPClient(t *testing.T) *awshttp.BuildableClient {
	t.Helper()

	cfg, err := originAWSConfig(t.Context(), options{region: "us-east-1", requestTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	s3client, err := newOriginS3Client(cfg, options{pathStyle: true})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeResource(s3client) })

	client, ok := s3client.Options().HTTPClient.(*awshttp.BuildableClient)
	if !ok {
		t.Fatalf("HTTP client type = %T", cfg.HTTPClient)
	}

	return client
}

func TestOriginHTTPTransport(t *testing.T) {
	client := testOriginHTTPClient(t)

	transport := client.GetTransport()

	defaults := http.DefaultTransport.(*http.Transport)
	if transport == defaults {
		t.Fatal("origin shares the default transport")
	}

	if transport.MaxIdleConnsPerHost < 68 || transport.MaxIdleConns < transport.MaxIdleConnsPerHost {
		t.Fatalf("idle pool cannot retain origin concurrency: per-host=%d total=%d", transport.MaxIdleConnsPerHost, transport.MaxIdleConns)
	}

	if client.GetTimeout() != time.Minute || transport.DialContext == nil || transport.Proxy == nil ||
		transport.TLSHandshakeTimeout != defaults.TLSHandshakeTimeout || transport.IdleConnTimeout != defaults.IdleConnTimeout ||
		transport.ExpectContinueTimeout != defaults.ExpectContinueTimeout || transport.ForceAttemptHTTP2 != defaults.ForceAttemptHTTP2 {
		t.Fatal("request timeout or default transport behavior changed")
	}

	other := testOriginHTTPClient(t).GetTransport()
	if other == transport {
		t.Fatal("origin clients share a transport")
	}

	defaultLimit := defaults.MaxIdleConnsPerHost

	transport.MaxIdleConnsPerHost++
	if defaults.MaxIdleConnsPerHost != defaultLimit || other.MaxIdleConnsPerHost == transport.MaxIdleConnsPerHost {
		t.Fatal("transport configuration is not isolated")
	}
}

func TestOriginHTTPTransportInvalidDefault(t *testing.T) {
	defaults := http.DefaultTransport

	t.Cleanup(func() { http.DefaultTransport = defaults })

	http.DefaultTransport = nil

	isolateOriginAWSConfig(t)

	if _, err := originAWSConfig(t.Context(), options{region: "us-east-1", requestTimeout: time.Minute}); err != nil {
		t.Fatalf("SDK client must not depend on http.DefaultTransport: %v", err)
	}
}

func TestOriginHTTPTransportCustomCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	for _, source := range []string{"env", "shared"} {
		t.Run(source, func(t *testing.T) {
			dir := isolateOriginAWSConfig(t)

			bundle := filepath.Join(dir, "ca.pem")
			if err := os.WriteFile(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
				t.Fatal(err)
			}

			if source == "env" {
				t.Setenv("AWS_CA_BUNDLE", bundle)
			} else if err := os.WriteFile(filepath.Join(dir, "config"), []byte("[default]\nca_bundle = "+bundle+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			client := configuredOriginHTTPClient(t)

			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}

			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}

			closeResource(response.Body)

			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d", response.StatusCode)
			}
		})
	}
}

func TestOriginHTTPTransportInvalidCA(t *testing.T) {
	dir := isolateOriginAWSConfig(t)
	path := filepath.Join(dir, "private-bundle.pem")
	t.Setenv("AWS_CA_BUNDLE", path)

	for _, contents := range []string{"", "not a certificate"} {
		if contents != "" {
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		_, err := originAWSConfig(t.Context(), options{region: "us-east-1", requestTimeout: time.Minute})

		category := "config"
		if contents == "" {
			category = "notfound"

			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing CA file cause lost: %v", err)
			}
		}

		if err == nil || !strings.Contains(err.Error(), "category="+category) || errors.Unwrap(err) == nil ||
			!strings.HasPrefix(err.Error(), "could not load AWS configuration") || strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "private-bundle") {
			t.Fatalf("error = %v, want redacted CA configuration error", err)
		}
	}
}

func TestOriginHTTPRedirects(t *testing.T) {
	client := testOriginHTTPClient(t)
	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var calls atomic.Int32

			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)

				if r.Header.Get("X-Amz-Security-Token") != "" {
					t.Error("session token forwarded to another host")
				}

				if r.Method != http.MethodHead {
					t.Errorf("redirect changed method to %s", r.Method)
				}

				w.WriteHeader(http.StatusNoContent)
			}))
			defer target.Close()

			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Amz-Security-Token") != "test-session" {
					t.Error("session token missing from initial request")
				}

				http.Redirect(w, r, target.URL, code)
			}))
			defer source.Close()

			request, err := http.NewRequestWithContext(t.Context(), http.MethodHead, source.URL, nil)
			if err != nil {
				t.Fatal(err)
			}

			request.Header.Set("X-Amz-Security-Token", "test-session")

			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}

			closeResource(response.Body)

			if code == 307 || code == 308 {
				if response.StatusCode != http.StatusNoContent || calls.Load() != 1 {
					t.Fatalf("redirect not followed: status=%d calls=%d", response.StatusCode, calls.Load())
				}
			} else if response.StatusCode != code || calls.Load() != 0 {
				t.Fatalf("unsafe redirect followed: status=%d calls=%d", response.StatusCode, calls.Load())
			}
		})
	}
}

func TestOriginHTTPPoolClose(t *testing.T) {
	isolateOriginAWSConfig(t)

	cfg, err := originAWSConfig(t.Context(), options{region: "us-east-1", requestTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	client, err := newOriginS3Client(cfg, options{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeResource(client)

	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}

	server.Start()
	defer server.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Options().HTTPClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}

	closeResource(response.Body)

	select {
	case <-closed:
		t.Fatal("connection closed before pool cleanup")
	default:
	}

	closeResource(client)

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not close the active transport's idle connection")
	}
}

func TestOriginHTTPPoolClosePreservesSharedCredentials(t *testing.T) {
	isolateOriginAWSConfig(t)

	for _, key := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
	} {
		t.Setenv(key, "")
	}

	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN", "test-container-token")

	var credentialRequests atomic.Int32

	credentials := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credentialRequests.Add(1)

		if r.Header.Get("Authorization") != "test-container-token" {
			t.Error("credential endpoint authorization missing")
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"AccessKeyId":"test-access","SecretAccessKey":"test-secret","Token":"test-session"}`)
	}))
	defer credentials.Close()

	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", credentials.URL)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Security-Token") != "test-session" || !strings.Contains(r.Header.Get("Authorization"), "Credential=test-access/") {
			t.Error("S3 request not signed with endpoint credentials")
		}

		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	o := options{region: "us-east-1", requestTimeout: time.Second, endpoint: upstream.URL, pathStyle: true}

	cfg, err := originAWSConfig(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}

	first, err := newOriginS3Client(cfg, o)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResource(first)

	second, err := newOriginS3Client(cfg, o)
	if err != nil {
		t.Fatal(err)
	}
	defer closeResource(second)

	request := &s3.HeadObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key")}
	if _, err := first.HeadObject(t.Context(), request); err != nil {
		t.Fatal(err)
	}

	closeResource(first)

	cache, ok := cfg.Credentials.(*aws.CredentialsCache)
	if !ok {
		t.Fatalf("credentials type = %T, want SDK cache", cfg.Credentials)
	}

	cache.Invalidate()

	if _, err := second.HeadObject(t.Context(), request); err != nil {
		t.Fatal("closing one S3 client broke shared credentials:", err)
	}

	if got := credentialRequests.Load(); got != 2 {
		t.Fatalf("credential requests = %d, want initial retrieval and refresh", got)
	}
}

func TestOriginConnectionPoolDial(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		var pool originConnectionPool

		dial := pool.dial(func(context.Context, string, string) (net.Conn, error) {
			return nil, net.ErrClosed
		})
		if _, err := dial(t.Context(), "tcp", "unused"); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("dial error = %v", err)
		}

		pool.close()
	})
	t.Run("close-during-dial", func(t *testing.T) {
		var pool originConnectionPool

		local, remote := net.Pipe()
		defer closeResource(remote)

		dial := pool.dial(func(context.Context, string, string) (net.Conn, error) {
			pool.close()
			return local, nil
		})
		if _, err := dial(t.Context(), "tcp", "unused"); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("dial after close = %v", err)
		}

		if _, err := remote.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("late connection not closed: %v", err)
		}
	})
	t.Run("forget-closed-connection", func(t *testing.T) {
		var pool originConnectionPool

		local, remote := net.Pipe()
		defer closeResource(remote)

		dial := pool.dial(func(context.Context, string, string) (net.Conn, error) {
			return local, nil
		})

		conn, err := dial(t.Context(), "tcp", "unused")
		if err != nil {
			t.Fatal(err)
		}

		if len(pool.connections) != 1 {
			t.Fatal("connection not tracked")
		}

		closeResource(conn)

		if len(pool.connections) != 0 {
			t.Fatal("closed connection retained")
		}

		pool.close()
		pool.close()
	})
}

func TestOriginHTTPSameHostRedirect(t *testing.T) {
	client := testOriginHTTPClient(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
			return
		}

		if r.Header.Get("X-Amz-Security-Token") != "test-session" {
			t.Error("same-host redirect lost session token")
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("X-Amz-Security-Token", "test-session")

	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}

	closeResource(response.Body)

	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

func TestOriginHTTPRequestTimeout(t *testing.T) {
	client := testOriginHTTPClient(t).WithTimeout(20 * time.Millisecond)

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(request)
	if response != nil {
		closeResource(response.Body)
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request timeout = %v", err)
	}
}

func TestOriginHTTPConnectionReuse(t *testing.T) {
	const concurrency = 64

	client := testOriginHTTPClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)

	var connections atomic.Int32

	entered := make(chan struct{}, concurrency)
	release := make(chan struct{}, concurrency)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		w.Header().Set("Content-Length", "1")
		_, _ = io.WriteString(w, "x")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}

	server.Start()
	defer server.Close()
	defer cancel()

	for burst := range 2 {
		var workers sync.WaitGroup

		results := make(chan error, concurrency)
		for range concurrency {
			workers.Go(func() {
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
				if err != nil {
					results <- err
					return
				}

				response, err := client.Do(request)
				if err != nil {
					results <- err
					return
				}

				_, err = io.Copy(io.Discard, response.Body)

				closeErr := response.Body.Close()
				if err == nil {
					err = closeErr
				}

				results <- err
			})
		}

		for range concurrency {
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("concurrent requests did not all arrive:", ctx.Err())
			}
		}

		for range concurrency {
			release <- struct{}{}
		}

		workers.Wait()

		for range concurrency {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}

		if got := connections.Load(); got != concurrency {
			t.Fatalf("burst %d opened %d total connections, want %d with keep-alive reuse", burst+1, got, concurrency)
		}
	}
}
