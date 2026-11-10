// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !linux

package contentstore

import (
	"errors"
	"os"
	"syscall"
)

const noFollow = syscall.O_NOFOLLOW

func openBeneath(root, rel string) (*os.File, error) { return openRoot(root, rel) }

func isSymlinkError(err error) bool { return errors.Is(err, syscall.ELOOP) }
