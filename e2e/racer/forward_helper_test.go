//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestForwardProcess supplies a real child process and HTTP listener so the
// regression exercises process death after the local forwarding banner.
func TestForwardProcess(t *testing.T) {
	mode := os.Getenv("RACER_TEST_FORWARD")
	if mode == "" {
		return
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	_, err = os.Stdout.WriteString("Forwarding from " + listener.Addr().String() + " -> 9090\n")
	require.NoError(t, err)

	var requests atomic.Int32

	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mode == "refused" {
				os.Exit(1)
			}

			if r.URL.Path != "/readyz" {
				http.NotFound(w, r)
				return
			}

			if mode == "unready" || requests.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}

			w.WriteHeader(http.StatusOK)
		}),
	}
	defer server.Close()

	require.NoError(t, server.Serve(listener))
}

func forwardProcess(t *testing.T, mode string) *exec.Cmd {
	t.Helper()

	executable, err := os.Executable()
	require.NoError(t, err)

	cmd := exec.Command(executable, "-test.run=^TestForwardProcess$")

	cmd.Env = append(os.Environ(), "RACER_TEST_FORWARD="+mode)

	return cmd
}

func TestForwardHTTPRecoversRemoteStartupRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	prefix := filepath.Join(t.TempDir(), "forward")

	var processes []*exec.Cmd

	address, stop, err := forwardHTTP(ctx, func() *exec.Cmd {
		mode := "ready"
		if len(processes) == 0 {
			mode = "refused"
		}

		cmd := forwardProcess(t, mode)
		processes = append(processes, cmd)

		return cmd
	}, prefix, "/readyz")
	require.NoError(t, err)
	t.Cleanup(stop)
	require.Len(t, processes, 2, "must replace the dead forwarder, but retain it for HTTP 503")
	require.FileExists(t, prefix+"-1.log")
	require.FileExists(t, prefix+"-2.log")

	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(address + "/readyz")
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	stop()
	require.NotNil(t, processes[1].ProcessState, "cleanup must reap the surviving forwarder")
}

func TestForwardHTTPDeadline(t *testing.T) {
	for _, mode := range []string{"refused", "unready"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			var processes []*exec.Cmd

			_, _, err := forwardHTTP(ctx, func() *exec.Cmd {
				cmd := forwardProcess(t, mode)
				processes = append(processes, cmd)

				return cmd
			}, filepath.Join(t.TempDir(), "forward"), "/readyz")
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NotEmpty(t, processes)

			for _, process := range processes {
				require.NotNil(t, process.ProcessState, "deadline must reap each forwarder")
			}
		})
	}
}

func TestForwardHTTPStartFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	directory := t.TempDir()
	_, _, err := forwardHTTP(ctx, func() *exec.Cmd {
		return exec.Command(filepath.Join(directory, "missing-kubectl"))
	}, filepath.Join(directory, "forward"), "/readyz")
	require.Error(t, err)
	require.NotErrorIs(t, err, context.DeadlineExceeded)
}
