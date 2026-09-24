// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

type racerFailurePhase int

const (
	racerAdmission racerFailurePhase = iota
	racerHead
	racerPrepare
	racerForward
)

// Fixed phase buckets prevent cancellation/admission storms from hiding the
// first upstream failure. No digest, target, or error string becomes a key.
type racerDiagnostics struct {
	mu         sync.Mutex
	next       [4]time.Time
	suppressed [4]uint64
}

func (d *racerDiagnostics) sample(phase racerFailurePhase, now time.Time) (uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if now.Before(d.next[phase]) {
		d.suppressed[phase]++
		return 0, false
	}

	n := d.suppressed[phase]
	d.suppressed[phase] = 0
	d.next[phase] = now.Add(30 * time.Second)

	return n, true
}

func racerErrorClass(err error) string {
	var (
		status  *sdk.HTTPError
		network net.Error
	)

	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.As(err, &status):
		return "http_status"
	case errors.Is(err, sdk.ErrProtocol):
		return "protocol"
	case errors.Is(err, sdk.ErrVersionChanged):
		return "version_changed"
	case errors.Is(err, sdk.ErrNoValidator):
		return "no_validator"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection_reset"
	case errors.Is(err, syscall.EPIPE):
		return "broken_pipe"
	case errors.As(err, &network) && network.Timeout():
		return "io_timeout"
	default:
		return "other"
	}
}

func (s *Server) reportRacerFailure(ref ifaces.OriginRef, phase racerFailurePhase, err error, stream *sdk.Stream, written int64) {
	operation, pageOffset, offset, statusCode := "", int64(-1), int64(-1), 0
	contextClass := "none"

	if stream != nil {
		if failure := stream.Failure(); failure != nil {
			operation, pageOffset, offset, statusCode = failure.Operation, failure.PageOffset, failure.Offset, failure.StatusCode

			err = failure.Err
			if failure.ContextErr != nil {
				contextClass = racerErrorClass(failure.ContextErr)

				var status *sdk.HTTPError
				if errors.Is(failure.ContextErr, context.Canceled) && !errors.As(err, &status) {
					return
				}
			}
		}
	}

	// Sibling/disconnect cancellation is not initiating failure evidence and
	// must not consume the bounded sample reserved for upstream failures.
	if errors.Is(err, context.Canceled) {
		return
	}

	suppressed, ok := s.racer.diagnostics.sample(phase, time.Now())
	if !ok {
		return
	}

	var status *sdk.HTTPError
	if errors.As(err, &status) {
		statusCode = status.StatusCode
	}

	// Use only the base logger and bounded fields. The request logger includes
	// registry/repository names; raw errors can contain URLs or credentials.
	s.logger.Warn("mirror: sampled Racer failure",
		slog.String("digest", ref.Digest.String()),
		slog.String("phase", [...]string{"admission", "HEAD", "Prepare", "forward"}[phase]),
		slog.String("operation", operation),
		slog.String("error_class", racerErrorClass(err)),
		slog.String("context_error", contextClass),
		slog.Int("racer_status", statusCode),
		slog.Int64("page_offset", pageOffset),
		slog.Int64("object_offset", offset),
		slog.Int64("written", written),
		slog.Uint64("suppressed", suppressed))
}
