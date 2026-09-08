// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package utilio

import "os"

func IsExecutable(filePath string) bool {
	s, err := os.Stat(filePath)
	if err != nil {
		return false
	}

	return !s.IsDir() && s.Mode()&0o111 != 0
}
