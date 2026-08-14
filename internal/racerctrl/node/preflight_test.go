// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A host can export its own namespace and still be unable to read a single
// peer's, because the target and the initiator are different modules. Preflight
// exists to turn that into a startup failure, so the missing device has to be
// reported with the modprobe that fixes it.
func TestFabricsControlCheckReportsAMissingInitiator(t *testing.T) {
	err := checkFabricsControl(filepath.Join(t.TempDir(), "nvme-fabrics"))
	if err == nil {
		t.Fatal("a host with no initiator device passed preflight")
	}

	if !strings.Contains(err.Error(), "nvme_tcp") {
		t.Fatalf("error %q does not say how to load the initiator", err)
	}
}

// The check has to open the device rather than stat it: a device node left
// behind by a module that is no longer loaded still stats.
func TestFabricsControlCheckAcceptsAnOpenableDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nvme-fabrics")

	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create stand-in device: %v", err)
	}

	if err := checkFabricsControl(path); err != nil {
		t.Fatalf("an openable device was rejected: %v", err)
	}
}

// Anything other than a missing device is reported as itself, so a permission
// problem does not read as an unloaded module and send an operator after the
// wrong fix.
func TestFabricsControlCheckReportsOtherOpenFailures(t *testing.T) {
	dir := t.TempDir()

	err := checkFabricsControl(dir)
	if err == nil {
		t.Fatal("a path that cannot be opened for writing passed preflight")
	}

	if strings.Contains(err.Error(), "nvme_tcp") {
		t.Fatalf("error %q blamed an unloaded module for an unrelated failure", err)
	}
}
