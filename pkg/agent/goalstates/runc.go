// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// RunCLibpathrsFallbackVersion is the runc release used when the host kernel
// release cannot be parsed by libpathrs. runc 1.5 links libpathrs, which
// panics on startup for such kernels, so every container create fails. runc
// 1.4 does not link libpathrs.
const RunCLibpathrsFallbackVersion = "1.4.3"

// defaultRunCVersion returns the runc version to install when the agent config
// does not pin one. It is RunCVersion unless the host kernel release would
// crash the libpathrs linked into that runc.
func defaultRunCVersion(kernelRelease string) string {
	if libpathrsParsesKernelRelease(kernelRelease) {
		return RunCVersion
	}

	return RunCLibpathrsFallbackVersion
}

// resolveRunCVersion returns the runc version for the goal state. A configured
// version is an explicit pin and is returned unchanged. Otherwise it picks
// defaultRunCVersion for the kernel. When a download override sets a runc
// version, for example from an offline artifact manifest, that version is the
// one installed. It returns an error when the version would link libpathrs on
// a kernel that libpathrs cannot parse. It does not swap in the fallback
// version, because the override source may not contain it.
func resolveRunCVersion(log *slog.Logger, kernelRelease, configured string, downloads *DownloadOverrides) (string, error) {
	if configured != "" {
		return configured, nil
	}

	version := defaultRunCVersion(kernelRelease)

	if downloads != nil && downloads.Runc != nil && downloads.Runc.Version != "" {
		effective := downloads.Runc.Version
		if !libpathrsParsesKernelRelease(kernelRelease) && runcLinksLibpathrs(effective) {
			return "", fmt.Errorf("host kernel release %q is not supported by runc %s from the download override; "+
				"provide runc %s or set CRI.Runc.Version", kernelRelease, effective, RunCLibpathrsFallbackVersion)
		}

		return version, nil
	}

	if version != RunCVersion {
		log.Warn("host kernel release is not supported by runc "+RunCVersion+"; using an older runc",
			"kernel", kernelRelease, "runc", version)
	}

	return version, nil
}

// runcLinksLibpathrs reports whether the runc version links libpathrs, which
// runc does from 1.5 on. A version it cannot parse is treated as linking
// libpathrs.
func runcLinksLibpathrs(version string) bool {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return true
	}

	major, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return true
	}

	minorDigits := parts[1]
	if end := strings.IndexFunc(minorDigits, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
		minorDigits = minorDigits[:end]
	}

	minor, err := strconv.ParseUint(minorDigits, 10, 64)
	if err != nil {
		return true
	}

	return major > 1 || (major == 1 && minor >= 5)
}

// libpathrsParsesKernelRelease reports whether libpathrs can parse the kernel
// release. It mirrors parse_kernel_version in libpathrs
// (src/utils/kernel_version.rs): keep the leading run of digits and dots,
// split it on dots, and require at least two components that are all
// non-empty numbers.
//
// The Azure Linux MSHV kernel release 6.18.58.mshv1-1.azl3 fails this check
// because the kept prefix is "6.18.58." and its last component is empty.
func libpathrsParsesKernelRelease(release string) bool {
	end := strings.IndexFunc(release, func(r rune) bool {
		return (r < '0' || r > '9') && r != '.'
	})
	if end >= 0 {
		release = release[:end]
	}

	parts := strings.Split(release, ".")
	if len(parts) < 2 {
		return false
	}

	for _, part := range parts {
		if part == "" {
			return false
		}

		if _, err := strconv.ParseUint(part, 10, 64); err != nil {
			return false
		}
	}

	return true
}
