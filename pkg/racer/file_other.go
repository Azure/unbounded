// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package racer

import "os"

func (s *Stream) spliceToFile(dst *os.File, offset int64) (int64, error) {
	return s.copyToFile(dst, offset)
}
