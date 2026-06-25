// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"io"
	"log/slog"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
