// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"log/slog"
)

func closeResource(c io.Closer) {
	if err := c.Close(); err != nil {
		slog.Debug("close resource", "error", err)
	}
}
