// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"os"
	"syscall"
)

func fileSys(info os.FileInfo) (dev, ino uint64, ctime int64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0
	}

	return uint64(st.Dev), st.Ino, st.Ctim.Nano() //nolint:unconvert // Dev width varies by arch
}
