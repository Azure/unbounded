// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package none

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// Directories, relative to the host root, that the installers treat as the
// node stack's executable and helper locations. Root redirects them into the
// version directory so a flip of the current link swaps them atomically.
const (
	installBinDir     = "/usr/local/bin"
	installLibexecDir = "/usr/local/libexec"
)

// Root is the NodeRoot of one installed version. Every path is the identity
// mapping, which is the same-path contract for /etc/containerd, /var/lib/kubelet,
// /opt/cni and the rest, except the installer's bin and libexec directories,
// which resolve into the version directory. That is what lets the installers
// the nspawn rootfs uses place binaries in <base>/<version>/bin unchanged.
type Root struct {
	Layout  Layout
	Version string
	// HostRoot prefixes the identity-mapped paths. Empty is the real host; tests
	// and offline tooling point it at a directory tree.
	HostRoot string
}

var _ isolation.NodeRoot = Root{}

// Slot implements isolation.NodeRoot. For this backend the slot is the version.
func (r Root) Slot() string { return r.Version }

// Path implements isolation.NodeRoot.
func (r Root) Path(inNode string) string {
	p := path.Clean("/" + strings.TrimPrefix(inNode, "/"))

	for _, m := range []struct{ from, to string }{
		{installBinDir, "bin"},
		{installLibexecDir, "libexec"},
	} {
		if p == m.from || strings.HasPrefix(p, m.from+"/") {
			return filepath.Join(r.Layout.VersionDir(r.Version), m.to, strings.TrimPrefix(p, m.from))
		}
	}

	if r.HostRoot != "" {
		return filepath.Join(r.HostRoot, p)
	}

	return p
}
