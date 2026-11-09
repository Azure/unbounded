// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package meshfetch

import "os"

// openChunkFile creates path. Only Linux supports O_DIRECT here.
func openChunkFile(path string, _ bool) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	return f, false, err
}

func preallocate(*os.File, int64) error { return nil }

func dropWritten(*os.File, int64, int64) {}

func dropRead(*os.File, int64, int64) {}

func mapDiskErr(err error) error { return err }
