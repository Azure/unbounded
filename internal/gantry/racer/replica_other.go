// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package racer

import "os"

func fileSys(os.FileInfo) (dev, ino uint64, ctime int64) { return 0, 0, 0 }
