// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"io"
)

type requestWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w requestWriter) Write(b []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}

	n, err := w.writer.Write(b)
	if err == nil {
		err = w.ctx.Err()
	}

	return n, err
}
