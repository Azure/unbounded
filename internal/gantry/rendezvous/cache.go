// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rendezvous

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	maxPeerCacheEntries = 64
	maxPeerCacheBytes   = 64 << 10
)

func readPeerCache(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}

	file, err := os.Open(path) // #nosec G304 -- configured local hostPath
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("open peer cache: %w", err)
	}

	defer func() { _ = file.Close() }() //nolint:errcheck // read-only best-effort close

	raw, err := io.ReadAll(io.LimitReader(file, maxPeerCacheBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read peer cache: %w", err)
	}

	if len(raw) > maxPeerCacheBytes {
		return nil, fmt.Errorf("peer cache exceeds %d bytes", maxPeerCacheBytes)
	}

	var peers []string
	if err := json.Unmarshal(raw, &peers); err != nil {
		return nil, fmt.Errorf("parse peer cache: %w", err)
	}

	if len(peers) > maxPeerCacheEntries {
		peers = peers[:maxPeerCacheEntries]
	}

	return peers, nil
}

func writePeerCache(path string, peers []string) error {
	if path == "" || len(peers) == 0 {
		return nil
	}

	if len(peers) > maxPeerCacheEntries {
		peers = peers[:maxPeerCacheEntries]
	}

	raw, err := json.Marshal(peers)
	if err != nil {
		return fmt.Errorf("marshal peer cache: %w", err)
	}

	if len(raw) > maxPeerCacheBytes {
		return fmt.Errorf("peer cache exceeds %d bytes", maxPeerCacheBytes)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create peer cache directory: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".bootstrap-peers-*")
	if err != nil {
		return fmt.Errorf("create peer cache temp file: %w", err)
	}

	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }() //nolint:errcheck // best-effort cleanup after rename

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close() //nolint:errcheck // preserve chmod error
		return fmt.Errorf("chmod peer cache: %w", err)
	}

	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close() //nolint:errcheck // preserve write error
		return fmt.Errorf("write peer cache: %w", err)
	}

	if err := temporary.Sync(); err != nil {
		_ = temporary.Close() //nolint:errcheck // preserve sync error
		return fmt.Errorf("sync peer cache: %w", err)
	}

	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close peer cache: %w", err)
	}

	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace peer cache: %w", err)
	}

	return nil
}
