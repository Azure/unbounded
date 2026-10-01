// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netlink

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// rtTablesPath is replaceable in tests so route-table repair never touches the host.
var rtTablesPath = "/etc/iproute2/rt_tables"

var validIfaceNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func validateInterfaceName(name string) error {
	if !validIfaceNameRe.MatchString(name) {
		return fmt.Errorf("invalid interface name %q: must match %s", name, validIfaceNameRe.String())
	}

	return nil
}

// atomicWriteFile avoids partial route-table files by replacing them atomically.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("failed to create temp file for atomic write: %w", err)
	}

	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // renamed on success; remove partial file on failure

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() //nolint:errcheck
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close() //nolint:errcheck
		return fmt.Errorf("failed to set permissions on temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("failed to rename temp file to %s: %w", path, err)
	}

	return nil
}
