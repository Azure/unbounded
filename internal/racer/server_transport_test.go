// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestOperationalStartCancellationAndTLSFiles(t *testing.T) {
	f := newServingFixture(t)
	dir := t.TempDir()
	f.a.Server.Config.ControlAddress = "127.0.0.1:0"
	f.a.Server.Config.TLSCertificateFile = filepath.Join(dir, "tls.crt")

	f.a.Server.Config.TLSPrivateKeyFile = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(f.a.Server.Config.TLSCertificateFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.serverCertificate.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}

	key, err := x509.MarshalPKCS8PrivateKey(f.key)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(f.a.Server.Config.TLSPrivateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}

	f.a.Lifecycle.SetServingReady(false)

	done := make(chan error, 1)

	go func() { done <- f.a.Server.Start(f.ctx) }()

	deadline := time.After(5 * time.Second)

	for f.a.Server.Ready(nil) != nil {
		select {
		case err := <-done:
			t.Fatalf("start: %v", err)
		case <-deadline:
			t.Fatal("not ready")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	f.cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown blocked")
	}

	if f.a.Server.Ready(nil) == nil {
		t.Fatal("canceled server ready")
	}
}

func TestLeaderCancellationClosesActiveTLSPoll(t *testing.T) {
	f := newServingFixture(t)
	f.a.Server.initializeAdmission()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() { done <- f.a.Server.serve(f.ctx, listener, f.a.Server.tlsConfig(f.ctx, f.serverCertificate)) }()

	c := f.client(t, &f.certificate)

	publication, err := f.a.authority.Current()
	if err != nil {
		t.Fatal(err)
	}

	requestDone := make(chan error, 1)

	go func() {
		response, err := c.Get(fmt.Sprintf("https://%s/v1/snapshot?after=%d", listener.Addr(), publication.Sequence()))
		if response != nil {
			response.Body.Close()
		}

		requestDone <- err
	}()

	awaitServerPolls(t, f.a.Server, 1)
	f.cancel()

	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("leader cancellation left active poll")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server shutdown blocked")
	}

	awaitServerPolls(t, f.a.Server, 0)
}

func TestTLSPollExpirationAndRequestCancellation(t *testing.T) {
	for _, expiration := range []bool{true, false} {
		t.Run(fmt.Sprint(expiration), func(t *testing.T) {
			f := newServingFixture(t)

			cert := f.certificate
			if expiration {
				cert = f.signLeaf(t, func(c *x509.Certificate) { c.NotAfter = time.Now().Add(2 * time.Second).Truncate(time.Second) })
			}

			endpoint := f.start(t)
			c := f.client(t, &cert)

			publication, err := f.a.authority.Current()
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()

			r, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/v1/snapshot?after=%d", endpoint, publication.Sequence()), nil)
			if err != nil {
				t.Fatal(err)
			}

			done := make(chan struct{})

			go func() {
				defer close(done)

				response, err := c.Do(r)
				if expiration {
					responseBody(t, response, err, 401)
				} else {
					if response != nil {
						response.Body.Close()
					}

					if err == nil {
						t.Error("canceled poll succeeded")
					}
				}
			}()

			awaitServerPolls(t, f.a.Server, 1)

			if !expiration {
				cancel()
			}

			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("poll outlived expiration/cancellation")
			}

			awaitServerPolls(t, f.a.Server, 0)
		})
	}
}

func TestHTTPWriteBootstrapAndGlobalAdmission(t *testing.T) {
	f := newServingFixture(t)
	f.a.Server.Config.Limits.MaxPolls = 1
	f.a.Server.Config.Limits.MaxConcurrentWrites = 1
	f.a.Server.Config.Limits.MaxConcurrentBootstrap = 1
	handler := f.a.Server.Handler()
	r := httptest.NewRequest("GET", wire.SnapshotPath, nil)

	r.TLS = f.requestState(t)
	for _, resource := range []string{"write", "bootstrap", "headers"} {
		t.Run(resource, func(t *testing.T) {
			request := r.Clone(f.ctx)

			switch resource {
			case "write":
				take(f.a.Server.writes)
				defer release(f.a.Server.writes)
			case "bootstrap":
				take(f.a.Server.bootstrapSlots)
				defer release(f.a.Server.bootstrapSlots)

				request.Method = "POST"
				request.URL.Path = wire.BootstrapPath
			case "headers":
				request.Header.Set("X-Large", strings.Repeat("a", f.a.Server.Config.Limits.HeaderBytes))
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request)

			want := 429
			if resource == "headers" {
				want = 413
			}

			if w.Code != want {
				t.Fatalf("unbounded %s: %d", resource, w.Code)
			}
		})
	}
}

func TestBootstrapIssuanceBeforeWriteAdmission(t *testing.T) {
	for _, scenario := range []string{"success", "write saturation", "readiness lost", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newServingFixture(t)
			s := f.a.Server
			s.Config.Limits.MaxConcurrentWrites = 1
			handler := s.Handler()

			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()

			wantWrites, wantStatus := 0, http.StatusOK

			if scenario == "write saturation" {
				take(s.writes)
				defer release(s.writes)

				wantWrites, wantStatus = 1, http.StatusTooManyRequests
			} else if scenario != "success" {
				wantStatus = http.StatusServiceUnavailable
			}

			reads := 0
			fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads++

				if len(s.writes) != wantWrites || len(s.bootstrapSlots) != 1 || len(s.authSlots) != 0 {
					t.Error("issuance changed write/auth admission timing")
				}

				switch scenario {
				case "readiness lost":
					f.a.Lifecycle.SetServingReady(false)
				case "canceled":
					cancel()
				}

				return c.Get(ctx, key, obj, opts...)
			}})

			body, err := wire.EncodeBootstrapRequest(f.request)
			if err != nil {
				t.Fatal(err)
			}

			r := httptest.NewRequestWithContext(ctx, "POST", wire.BootstrapPath, bytes.NewReader(body))
			r.TLS = &tls.ConnectionState{HandshakeComplete: true}
			r.Header.Set("Authorization", "Bearer "+f.token)
			r.Header.Set("Content-Type", "application/json")

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			encoded := responseBody(t, w.Result(), nil, wantStatus)
			if reads == 0 || len(s.bootstrapSlots) != 0 || len(s.authSlots) != 0 || len(s.writes) != wantWrites {
				t.Fatal("issuance skipped or admission leaked")
			}

			if scenario == "success" {
				response := decodeIssuedResponse(t, encoded)
				if response.Enrollment != f.request.Enrollment || response.Node != wire.NodeID(testNodeUID) || !w.Flushed || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("issued response was not served completely")
				}
			}
		})
	}
}

func TestBootstrapReadDeadlineAndChunkedBound(t *testing.T) {
	f := newServingFixture(t)
	f.a.Server.Config.Limits.WriteTimeout = 100 * time.Millisecond
	endpoint := f.start(t)
	c := f.client(t, nil)
	// A body of unknown length must still be bounded by the wire decoder.
	r, err := http.NewRequestWithContext(f.ctx, "POST", endpoint+wire.BootstrapPath, io.NopCloser(strings.NewReader(strings.Repeat("x", wire.MaxBootstrapBytes+1))))
	if err != nil {
		t.Fatal(err)
	}

	r.Header.Set("Content-Type", "application/json")
	response, err := c.Do(r)
	responseBody(t, response, err, 413)
	// A client that never completes its body cannot hold bootstrap admission.
	address := strings.TrimPrefix(endpoint, "https://")

	conn, err := tls.Dial("tcp", address, &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "POST /v1/bootstrap HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	_, _ = io.Copy(io.Discard, conn)
	deadline := time.After(time.Second)

	for len(f.a.Server.bootstrapSlots) != 0 {
		select {
		case <-deadline:
			t.Fatal("slow body retained admission")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestTLSSlowSnapshotWriteDeadline(t *testing.T) {
	f := newServingFixture(t)
	f.a.Server.Config.Limits.WriteTimeout = 200 * time.Millisecond
	// Exercise socket backpressure without constructing a large topology. The
	// immutable publication remains valid JSON with bounded trailing whitespace.
	largeFixturePublication(t, f)
	endpoint := f.start(t)

	conn, err := tls.Dial("tcp", strings.TrimPrefix(endpoint, "https://"), &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.certificate}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, "GET /v1/snapshot HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)

	for len(f.a.Server.writes) == 0 {
		select {
		case <-deadline:
			t.Fatal("write not admitted")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// Do not read response bytes. The write deadline must release both slots.
	for {
		n := f.a.Server.polls.count()

		if n == 0 && len(f.a.Server.writes) == 0 {
			break
		}

		select {
		case <-deadline:
			t.Fatal("slow socket bypassed write deadline")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestTLSNodeExclusionRemovesRoutingMembershipWhilePolling(t *testing.T) {
	f := newServingFixture(t)
	endpoint := f.start(t)
	c := f.client(t, &f.certificate)

	publication, err := f.a.authority.Current()
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		response, err := c.Get(fmt.Sprintf("%s/v1/snapshot?after=%d", endpoint, publication.Sequence()))
		body := responseBody(t, response, err, 200)

		updated, decodeErr := wire.DecodePublication(bytes.NewReader(body))
		if decodeErr != nil || len(updated.Members) != 0 {
			t.Errorf("exclusion must remove routing membership: %v", decodeErr)
		}
	}()

	awaitServerPolls(t, f.a.Server, 1)

	node := &corev1.Node{}
	if err := f.a.Topology.Get(f.ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
		t.Fatal(err)
	}

	node.Labels = map[string]string{wire.ExclusionLabel: ""}
	if err := f.a.Topology.Update(f.ctx, node); err != nil {
		t.Fatal(err)
	}

	reconcileTopology(t, f.a.Topology, f.ctx)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("routing membership poll did not wake after exclusion")
	}
}
