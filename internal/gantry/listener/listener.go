// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package listener creates network listeners for Gantry HTTP endpoints.
package listener

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const unixPrefix = "unix://"

// Parse returns the network and address represented by endpoint. TCP
// endpoints use host:port; Unix endpoints use unix:///absolute/path.
func Parse(endpoint string) (string, string, error) {
	if !strings.HasPrefix(endpoint, unixPrefix) {
		if _, _, err := net.SplitHostPort(endpoint); err != nil {
			return "", "", fmt.Errorf("parse TCP endpoint %q: %w", endpoint, err)
		}

		return "tcp", endpoint, nil
	}

	path := strings.TrimPrefix(endpoint, unixPrefix)
	if path == "" || !filepath.IsAbs(path) {
		return "", "", fmt.Errorf("parse Unix endpoint %q: socket path must be absolute", endpoint)
	}

	return "unix", filepath.Clean(path), nil
}

// Listen creates a listener for endpoint. A stale Unix socket left by an
// unclean shutdown is removed before binding.
func Listen(endpoint string) (net.Listener, error) {
	network, address, err := Parse(endpoint)
	if err != nil {
		return nil, err
	}

	if network == "unix" {
		if err := os.MkdirAll(filepath.Dir(address), 0o750); err != nil {
			return nil, fmt.Errorf("create Unix socket directory for %q: %w", address, err)
		}

		info, statErr := os.Lstat(address)
		switch {
		case statErr == nil && info.Mode()&os.ModeSocket == 0:
			return nil, fmt.Errorf("refusing to replace non-socket path %q", address)
		case statErr == nil:
			if err := os.Remove(address); err != nil {
				return nil, fmt.Errorf("remove stale Unix socket %q: %w", address, err)
			}
		case !errors.Is(statErr, os.ErrNotExist):
			return nil, fmt.Errorf("inspect Unix socket %q: %w", address, statErr)
		}
	}

	ln, err := net.Listen(network, address)
	if err != nil {
		return nil, fmt.Errorf("listen on %q: %w", endpoint, err)
	}

	return ln, nil
}
