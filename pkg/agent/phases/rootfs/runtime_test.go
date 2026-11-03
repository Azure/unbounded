// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func runtimeServer(t *testing.T, body string, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}

		_, _ = w.Write([]byte(body)) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)

	return srv, &hits
}

func runtimeGoalState(base string) *goalstates.RootFS {
	return &goalstates.RootFS{
		HostArch:    "amd64",
		RunCVersion: "1.5.0",
		Downloads:   &goalstates.DownloadOverrides{Runc: &goalstates.DownloadSource{BaseURL: base}},
	}
}

func TestInstallHostRuntimeDownloadsAndSkipsWhenCurrent(t *testing.T) {
	t.Parallel()

	srv, hits := runtimeServer(t, "#!/bin/sh\necho runc version 1.5.0\n", http.StatusOK)
	path := filepath.Join(t.TempDir(), "runtime", "runc")
	task := InstallHostRuntime(slog.Default(), runtimeGoalState(srv.URL), path)

	require.NoError(t, task.Do(context.Background()))
	require.Equal(t, int32(1), hits.Load())

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&0o111, "the runtime must be executable")

	// The installed runtime reports the expected version, so no second download.
	require.NoError(t, task.Do(context.Background()))
	require.Equal(t, int32(1), hits.Load())
}

func TestInstallHostRuntimeReplacesAWrongVersion(t *testing.T) {
	t.Parallel()

	srv, hits := runtimeServer(t, "#!/bin/sh\necho runc version 1.5.0\n", http.StatusOK)
	path := filepath.Join(t.TempDir(), "runc")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\necho runc version 1.0.0\n"), 0o755))

	require.NoError(t, InstallHostRuntime(slog.Default(), runtimeGoalState(srv.URL), path).Do(context.Background()))
	require.Equal(t, int32(1), hits.Load())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), "1.5.0")
}

func TestInstallHostRuntimeHonorsTheVersionOverride(t *testing.T) {
	t.Parallel()

	srv, hits := runtimeServer(t, "#!/bin/sh\necho runc version 1.9.9\n", http.StatusOK)
	gs := runtimeGoalState(srv.URL)
	gs.Downloads.Runc.Version = "1.9.9"
	path := filepath.Join(t.TempDir(), "runc")

	require.NoError(t, InstallHostRuntime(slog.Default(), gs, path).Do(context.Background()))
	require.NoError(t, InstallHostRuntime(slog.Default(), gs, path).Do(context.Background()))
	require.Equal(t, int32(1), hits.Load(), "the overridden version is the one matched")
}

func TestInstallHostRuntimeFailureLeavesNoBinary(t *testing.T) {
	t.Parallel()

	srv, _ := runtimeServer(t, "", http.StatusNotFound)
	path := filepath.Join(t.TempDir(), "runc")

	err := InstallHostRuntime(slog.Default(), runtimeGoalState(srv.URL), path).Do(context.Background())
	require.ErrorContains(t, err, "download host runtime")
	require.NoFileExists(t, path)
}

func TestInstallHostRuntimeRejectsAnotherFileName(t *testing.T) {
	t.Parallel()

	err := InstallHostRuntime(slog.Default(), runtimeGoalState("http://127.0.0.1:1"), filepath.Join(t.TempDir(), "runtime")).Do(context.Background())
	require.ErrorContains(t, err, "must end in runc")
}
