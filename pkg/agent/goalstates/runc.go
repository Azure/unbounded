// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
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
