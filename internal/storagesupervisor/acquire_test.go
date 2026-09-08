// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package storagesupervisor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tarEntry describes one member of a synthetic tar archive.
type tarEntry struct {
	name     string
	body     string
	mode     int64
	linkname string
	typ      byte
}

// buildTarGz builds an in-memory gzip-compressed tar archive from entries.
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}

		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Typeflag: typ,
			Linkname: e.linkname,
		}

		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}

		require.NoError(t, tw.WriteHeader(hdr))

		if typ == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	return buf.Bytes()
}

// releaseLayoutTar returns a valid release-layout archive (top-level dir that
// gets stripped, an executable daemon binary, a lib file, and a symlink).
func releaseLayoutTar(t *testing.T) []byte {
	t.Helper()

	return buildTarGz(t, []tarEntry{
		{name: "unbounded-storage-linux-amd64/", typ: tar.TypeDir, mode: 0o755},
		{name: "unbounded-storage-linux-amd64/bin/", typ: tar.TypeDir, mode: 0o755},
		{name: "unbounded-storage-linux-amd64/bin/unbounded-storage", body: "#!/bin/true\n", mode: 0o755},
		{name: "unbounded-storage-linux-amd64/lib/", typ: tar.TypeDir, mode: 0o755},
		{name: "unbounded-storage-linux-amd64/lib/libfoo.so.1", body: "elf", mode: 0o644},
		{name: "unbounded-storage-linux-amd64/lib/libfoo.so", typ: tar.TypeSymlink, linkname: "libfoo.so.1", mode: 0o777},
	})
}

func writeTempTarGz(t *testing.T, data []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	require.NoError(t, os.WriteFile(path, data, 0o644))

	return path
}

func TestExtractTarGzStripsTopLevel(t *testing.T) {
	archive := writeTempTarGz(t, releaseLayoutTar(t))
	dest := t.TempDir()

	require.NoError(t, extractTarGz(archive, dest))

	bin := filepath.Join(dest, "bin", "unbounded-storage")
	info, err := os.Stat(bin)
	require.NoError(t, err)
	assert.False(t, info.IsDir())
	assert.NotZero(t, info.Mode().Perm()&0o111, "binary should be executable")

	libBody, err := os.ReadFile(filepath.Join(dest, "lib", "libfoo.so.1"))
	require.NoError(t, err)
	assert.Equal(t, "elf", string(libBody))

	link, err := os.Readlink(filepath.Join(dest, "lib", "libfoo.so"))
	require.NoError(t, err)
	assert.Equal(t, "libfoo.so.1", link)
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	tests := []struct {
		name      string
		entries   []tarEntry
		alwaysErr bool
	}{
		{
			name: "dotdot escape",
			entries: []tarEntry{
				{name: "top/", typ: tar.TypeDir, mode: 0o755},
				{name: "top/../escape", body: "x", mode: 0o644},
			},
			alwaysErr: true,
		},
		{
			name: "absolute path",
			entries: []tarEntry{
				{name: "top/", typ: tar.TypeDir, mode: 0o755},
				{name: "top//etc/passwd", body: "x", mode: 0o644},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive := writeTempTarGz(t, buildTarGz(t, tt.entries))
			dest := t.TempDir()

			err := extractTarGz(archive, dest)
			if tt.alwaysErr {
				require.Error(t, err)
			}

			if err == nil {
				// Absolute-style names may normalize into dest; ensure nothing
				// escaped the destination directory.
				escaped := filepath.Join(filepath.Dir(dest), "escape")
				_, statErr := os.Stat(escaped)
				assert.True(t, os.IsNotExist(statErr), "no file should escape dest")
			}
		})
	}
}

func TestExtractTarGzBadGzip(t *testing.T) {
	archive := writeTempTarGz(t, []byte("not a gzip stream"))

	err := extractTarGz(archive, t.TempDir())
	assert.Error(t, err)
}

func TestParseSHA256(t *testing.T) {
	valid := hex.EncodeToString(make([]byte, sha256.Size))

	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "bare hash", raw: valid, want: valid},
		{name: "hash with filename", raw: valid + "  archive.tar.gz\n", want: valid},
		{name: "trailing whitespace", raw: "  " + valid + "  \n", want: valid},
		{name: "bad length", raw: "abcd", wantErr: true},
		{name: "bad hex", raw: "zz" + valid[2:], wantErr: true},
		{name: "empty", raw: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSHA256(tt.raw)
			if tt.wantErr {
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReleaseTarballURL(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "latest amd64",
			cfg:  Config{Repo: "Azure/unbounded-kube", Version: "latest", Arch: "amd64"},
			want: "https://github.com/Azure/unbounded-kube/releases/latest/download/unbounded-storage-linux-amd64.tar.gz",
		},
		{
			name: "pinned arm64",
			cfg:  Config{Repo: "acme/widgets", Version: "v1.2.3", Arch: "arm64"},
			want: "https://github.com/acme/widgets/releases/download/v1.2.3/unbounded-storage-linux-arm64.tar.gz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, releaseTarballURL(tt.cfg))
		})
	}
}

func TestTarballName(t *testing.T) {
	assert.Equal(t, "unbounded-storage-linux-amd64.tar.gz", tarballName("amd64"))
	assert.Equal(t, "unbounded-storage-linux-arm64.tar.gz", tarballName("arm64"))
}

// withHTTPClient temporarily swaps the package httpClient with the test
// server's client and restores it on cleanup.
func withHTTPClient(t *testing.T, srv *httptest.Server) {
	t.Helper()

	prev := httpClient
	httpClient = srv.Client()

	t.Cleanup(func() { httpClient = prev })
}

func TestAcquireRemoteSuccess(t *testing.T) {
	data := releaseLayoutTar(t)
	sum := sha256.Sum256(data)
	sumHex := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/archive.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/archive.tar.gz.sha256", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  archive.tar.gz\n", sumHex)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	withHTTPClient(t, srv)

	dest := t.TempDir()
	cfg := Config{SourceMode: SourceURL}

	err := acquireRemote(context.Background(), cfg, srv.URL+"/archive.tar.gz", dest)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dest, "bin", "unbounded-storage"))
	require.NoError(t, err)
}

func TestAcquireRemoteChecksumMismatch(t *testing.T) {
	data := releaseLayoutTar(t)
	wrong := hex.EncodeToString(make([]byte, sha256.Size))

	mux := http.NewServeMux()
	mux.HandleFunc("/archive.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/archive.tar.gz.sha256", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, wrong)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	withHTTPClient(t, srv)

	err := acquireRemote(context.Background(), Config{SourceMode: SourceURL}, srv.URL+"/archive.tar.gz", t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checksum verification failed")
}

func TestAcquireRemoteNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	withHTTPClient(t, srv)

	err := acquireRemote(context.Background(), Config{SourceMode: SourceURL}, srv.URL+"/archive.tar.gz", t.TempDir())
	assert.Error(t, err)
}

func TestAcquireAndExtractFileMode(t *testing.T) {
	archive := writeTempTarGz(t, releaseLayoutTar(t))
	dest := t.TempDir()

	cfg := Config{SourceMode: SourceFile, Source: archive, Arch: "amd64", Version: "latest"}

	require.NoError(t, acquireAndExtract(context.Background(), cfg, dest))

	_, err := os.Stat(filepath.Join(dest, "bin", "unbounded-storage"))
	require.NoError(t, err)
}
