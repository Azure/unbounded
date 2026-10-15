// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
)

const diagnosticSecret = "private-diagnostic-sentinel"

func TestSafeDiagnostic(t *testing.T) {
	for _, tt := range []struct {
		cause    error
		category string
	}{
		{os.ErrPermission, "permission"},
		{&os.PathError{Op: "open", Path: diagnosticSecret, Err: os.ErrPermission}, "permission"},
		{syscall.EADDRINUSE, "addressinuse"},
		{syscall.ENOTDIR, "notdirectory"},
		{syscall.ENOSPC, "nospace"},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "timeout"},
		{net.ErrClosed, "closed"},
		{racersdk.ErrForbidden, "forbidden"},
		{racersdk.ErrUnavailable, "unavailable"},
		{&net.DNSError{Name: diagnosticSecret, Err: diagnosticSecret}, "dns"},
		{&net.DNSError{Name: diagnosticSecret, IsTimeout: true}, "timeout"},
		{errors.New(diagnosticSecret), "config"},
	} {
		t.Run(tt.category, func(t *testing.T) {
			cause := fmt.Errorf("%s: %w", diagnosticSecret, tt.cause)

			err := safeDiagnostic("setup failed", "config", cause)
			if !errors.Is(err, cause) || !errors.Is(err, tt.cause) {
				t.Fatal("lost diagnostic cause")
			}

			if got := fmt.Sprintf("%s %v %+v", err, err, err); strings.Contains(got, diagnosticSecret) || !strings.Contains(got, "category="+tt.category) {
				t.Fatalf("unsafe or missing diagnostic category: %s", got)
			}
		})
	}
}

type diagnosticOutput struct{ err error }

func (w diagnosticOutput) Write([]byte) (int, error) { return 0, w.err }

func TestOutputDiagnosticCause(t *testing.T) {
	cause := errors.New(diagnosticSecret)

	err := writeHelp(diagnosticOutput{err: cause}, "help")
	if !errors.Is(err, cause) || strings.Contains(err.Error(), diagnosticSecret) {
		t.Fatal("output cause lost or exposed")
	}
}

func TestListenAndServeDiagnosticCauses(t *testing.T) {
	l, err := listenSidecar(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	other, err := listenSidecar(context.Background(), l.Addr().String())
	if other != nil {
		other.Close()
	}

	if !errors.Is(err, syscall.EADDRINUSE) || !strings.Contains(err.Error(), "category=addressinuse") {
		t.Fatalf("listen diagnostic: %v", err)
	}

	l.Close()

	err = serveSidecar(context.Background(), l, http.NotFoundHandler(), time.Second)
	if !errors.Is(err, net.ErrClosed) || !strings.Contains(err.Error(), "category=closed") {
		t.Fatalf("server diagnostic: %v", err)
	}
}

func TestAWSConfigDiagnosticCause(t *testing.T) {
	dir := t.TempDir()

	configPath := filepath.Join(dir, diagnosticSecret)
	if err := os.WriteFile(configPath, []byte("[default]\nmax_attempts = "+diagnosticSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for key, value := range map[string]string{
		"AWS_CONFIG_FILE": configPath, "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"),
		"AWS_PROFILE": "", "AWS_DEFAULT_PROFILE": "", "AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(key, value)
	}

	_, err := originAWSConfig(context.Background(), options{region: "us-east-1", requestTimeout: time.Second})
	if err == nil || errors.Unwrap(err) == nil || !strings.Contains(err.Error(), "category=config") || strings.Contains(err.Error(), diagnosticSecret) {
		t.Fatalf("AWS config diagnostic: %v", err)
	}
}

type diagnosticLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *diagnosticLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(p)
}

func (b *diagnosticLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}

func captureDiagnosticLog(t *testing.T) *diagnosticLog {
	t.Helper()

	var output diagnosticLog

	previous := log.Writer()

	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })

	return &output
}

func TestSidecarStartupAndPanicDiagnostics(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(fmt.Sprint(abort), func(t *testing.T) {
			output := captureDiagnosticLog(t)
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				if abort {
					panic(http.ErrAbortHandler)
				}

				panic(diagnosticSecret)
			})
			address, cancel, done := startTestSidecar(t, handler, time.Second)

			client := &http.Client{Timeout: 2 * time.Second}
			defer client.CloseIdleConnections()

			response, err := client.Get(address + "/" + diagnosticSecret)
			if response != nil {
				response.Body.Close()
			}

			cancel()
			waitSidecar(t, done)

			if err == nil {
				t.Fatal("panicked response did not abort")
			}

			got := output.String()
			if !strings.Contains(got, "mode=sidecar event=listening address="+strings.TrimPrefix(address, "http://")) {
				t.Fatalf("missing actual bound startup address: %s", got)
			}

			if strings.Contains(got, diagnosticSecret) || strings.Contains(got, "goroutine") || strings.Contains(got, "category=panic") == abort {
				t.Fatalf("unsafe or incorrect panic log: %s", got)
			}
		})
	}
}

func TestHTTPLogSanitization(t *testing.T) {
	output := captureDiagnosticLog(t)

	logger := log.New(safeHTTPLogWriter{}, "", 0)
	for _, prefix := range []string{"http: panic serving ", "http: Accept error:", "http: TLS handshake error", "unknown "} {
		logger.Print(prefix + diagnosticSecret)
	}

	got := output.String()
	if strings.Contains(got, diagnosticSecret) {
		t.Fatal("HTTP log exposed private text")
	}

	for _, category := range []string{"panic", "accept", "tls", "server"} {
		if !strings.Contains(got, "category="+category) {
			t.Fatalf("missing %s category: %s", category, got)
		}
	}
}

func TestOriginRuntimeDiagnostics(t *testing.T) {
	output := captureDiagnosticLog(t)
	origin := diagnoseOrigin(func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		return racersdk.Metadata{Size: 42}, nil, nil
	})

	metadata, _, err := origin(context.Background(), racersdk.OriginRequest{})
	if err != nil || metadata.Size != 42 || output.String() != "" {
		t.Fatal("successful origin request changed or logged")
	}

	// Forbidden is an expected outcome logged only at debug level.
	cause := fmt.Errorf("%s: %w", diagnosticSecret, racersdk.ErrUnavailable)
	origin = diagnoseOrigin(func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		return racersdk.Metadata{}, nil, cause
	})

	_, _, err = origin(context.Background(), racersdk.OriginRequest{})
	if !errors.Is(err, cause) || !strings.Contains(output.String(), "mode=origin event=request_failed category=unavailable") || strings.Contains(output.String(), diagnosticSecret) {
		t.Fatal("origin cause lost or diagnostic unsafe")
	}

	origin = diagnoseOrigin(func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		panic(diagnosticSecret)
	})

	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Error("origin panic not passed to SDK recovery safely")
		}

		if !strings.Contains(output.String(), "mode=origin event=request_failed category=panic") || strings.Contains(output.String(), diagnosticSecret) {
			t.Error("origin panic log missing or unsafe")
		}
	}()

	_, _, _ = origin(context.Background(), racersdk.OriginRequest{})
}

type privateDiagnosticAddress struct{}

func (privateDiagnosticAddress) Network() string { return diagnosticSecret }
func (privateDiagnosticAddress) String() string  { return diagnosticSecret }

func TestStartupAddressSanitization(t *testing.T) {
	output := captureDiagnosticLog(t)

	for _, address := range []net.Addr{
		privateDiagnosticAddress{},
		&net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1234},
		&net.TCPAddr{IP: net.ParseIP("::1"), Port: 1234, Zone: diagnosticSecret},
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0},
	} {
		logSidecarStartup(address)
	}

	if got := output.String(); strings.Contains(got, "address=") || strings.Contains(got, diagnosticSecret) {
		t.Fatalf("logged unsafe address: %s", got)
	}

	logSidecarStartup(&net.TCPAddr{IP: net.ParseIP("::1"), Port: 1234})

	if !strings.Contains(output.String(), "address=[::1]:1234") {
		t.Fatal("IPv6 loopback address not logged")
	}
}

type privateDiagnosticPanic struct{ formatted *bool }

func (p privateDiagnosticPanic) String() string {
	*p.formatted = true
	return diagnosticSecret
}

func TestPanicValueNeverFormatted(t *testing.T) {
	output := captureDiagnosticLog(t)
	formatted := false

	defer func() {
		if recover() != http.ErrAbortHandler || formatted || strings.Contains(output.String(), diagnosticSecret) {
			t.Error("panic value inspected or exposed")
		}
	}()

	func() {
		defer diagnoseSidecarPanic()

		panic(privateDiagnosticPanic{formatted: &formatted})
	}()
}
