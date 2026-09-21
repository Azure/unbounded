// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"compress/gzip"
	"io"
)

const gzipIdleWriterLimit = 4

type gzipWriterPool struct {
	idle chan *gzip.Writer
}

func newGzipWriterPool() *gzipWriterPool {
	return &gzipWriterPool{idle: make(chan *gzip.Writer, gzipIdleWriterLimit)}
}

func (p *gzipWriterPool) get(output io.Writer) (*gzip.Writer, error) {
	var writer *gzip.Writer

	select {
	case writer = <-p.idle:
	default:
		var err error

		writer, err = gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		if err != nil {
			return nil, err
		}
	}

	writer.Reset(output)

	return writer, nil
}

func (p *gzipWriterPool) put(writer *gzip.Writer, completed bool) {
	closed := false

	defer func() {
		// Detach even if Close panics, and reset before another request can
		// acquire the writer. Failed or interrupted responses are not pooled.
		writer.Reset(io.Discard)

		if !completed || !closed {
			return
		}

		select {
		case p.idle <- writer:
		default:
		}
	}()

	closed = writer.Close() == nil
}
