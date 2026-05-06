// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentbinary

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallFromTarGzVerifiesInstalledBinary(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		exitCode int
		wantErr  string
	}{
		{name: "valid", exitCode: 0},
		{name: "broken", exitCode: 42, wantErr: "verify agent binary"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				require.NoError(t, writeTestAgentArchive(w, testAgentScript(tt.name, tt.exitCode)))
			}))
			t.Cleanup(server.Close)

			targetPath := filepath.Join(t.TempDir(), "unbounded-agent")
			err := InstallFromTarGz(context.Background(), server.URL, targetPath, "unbounded-agent", 0o755)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
		})
	}
}

func TestInstallFromTarGzRejectsUnsupportedScheme(t *testing.T) {
	t.Parallel()

	err := InstallFromTarGz(context.Background(), "file:///tmp/unbounded-agent.tar.gz", filepath.Join(t.TempDir(), "agent"), "unbounded-agent", 0o755)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported agent download URL scheme")
}

func writeTestAgentArchive(w io.Writer, binary []byte) error {
	gz := gzip.NewWriter(w)
	defer gz.Close()

	tw := tar.NewWriter(gz)
	defer tw.Close()

	header := &tar.Header{
		Name: "unbounded-agent",
		Mode: 0o755,
		Size: int64(len(binary)),
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	_, err := io.Copy(tw, bytes.NewReader(binary))
	return err
}

func testAgentScript(version string, exitCode int) []byte {
	return []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s\nexit %d\n", posixShellQuote(version), exitCode))
}

func posixShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
