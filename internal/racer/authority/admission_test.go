// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/racer/wire"
)

type admissionBoundaryWriter struct {
	calls int
	after func()
}

type admissionTestKey struct{}

func (w *admissionBoundaryWriter) Write(b []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		w.after()
	}

	return len(b), nil
}

func TestAdmissionPublicationEpochAndOriginalDeadline(t *testing.T) {
	for _, action := range []string{"advance", "suspend recover", "process cancel", "expiry"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newServingFixture(t)
				a := f.a.authority

				process, stopProcess := context.WithCancel(t.Context())
				defer stopProcess()
				// Start a process-bound epoch and use a large immutable response to
				// exercise checks after the first chunk, before callback scheduling.
				a.publications.Suspend()
				a.BindProcess(process)
				next := *a.publications.current
				next.encoded = strings.Repeat("x", 64*1024)
				next.record.Sequence++
				require.NoError(t, a.publications.Install(&next))
				handle, err := a.Current()
				require.NoError(t, err)
				guard, stop, err := handle.Admit(t.Context())
				require.NoError(t, err)

				defer stop()

				deadline, _ := guard.Context().Deadline()
				// Ordinary derived contexts must not hide admission or revocation.
				ctx, cancel := context.WithCancel(context.WithValue(guard.Context(), admissionTestKey{}, "value"))
				defer cancel()

				response := handle.ForBase(0, "")
				writer := &admissionBoundaryWriter{after: func() {
					advanced := *a.publications.current
					advanced.record.Sequence++
					require.NoError(t, a.publications.Install(&advanced))
					_, _, err := handle.Admit(t.Context())
					require.ErrorIs(t, err, wire.Unavailable)

					switch action {
					case "suspend recover":
						a.publications.Suspend()
						require.NoError(t, a.publications.Install(&advanced))
					case "process cancel":
						stopProcess()
					case "expiry":
						time.Sleep(time.Until(deadline))
					}
				}}

				n, err := response.WriteTo(ctx, guard, writer)
				if action == "advance" {
					require.NoError(t, err)
					require.EqualValues(t, 64*1024, n)
					require.Equal(t, 2, writer.calls)
					require.NoError(t, guard.Check(ctx))
					time.Sleep(time.Until(deadline))
					require.ErrorIs(t, guard.Check(ctx), context.DeadlineExceeded)
				} else {
					require.Error(t, err)
					require.EqualValues(t, 32*1024, n)
					require.Equal(t, 1, writer.calls)
				}

				got, _ := guard.Context().Deadline()
				require.Equal(t, deadline, got)
				// Check is synchronous; cancellation notification may arrive later.
				synctest.Wait()
				require.Error(t, guard.Context().Err())

				select {
				case <-guard.Context().Done():
				default:
					t.Fatal("context Err returned before Done closed")
				}
			})
		})
	}
}

func TestAdmissionRejectsMissingWrongImageAndExpiredTrust(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newServingFixture(t)
		a := f.a.authority
		old, err := a.Current()
		require.NoError(t, err)
		guard, stop, err := old.Admit(t.Context())
		require.NoError(t, err)

		defer stop()

		for _, missing := range []*Admission{nil, {}} {
			_, err = old.ForBase(0, "").WriteTo(t.Context(), missing, io.Discard)
			require.ErrorIs(t, err, wire.Forbidden)
		}

		next := *a.publications.current
		next.record.Sequence++
		require.NoError(t, a.publications.Install(&next))
		current, err := a.Current()
		require.NoError(t, err)
		_, err = current.ForBase(0, "").WriteTo(t.Context(), guard, io.Discard)
		require.ErrorIs(t, err, wire.Forbidden)
		trust, stopTrust, err := a.AdmitTrust(t.Context())
		require.NoError(t, err)

		defer stopTrust()

		combined, stopCombined, err := current.AdmitWithTrust(t.Context(), trust)
		require.NoError(t, err)

		defer stopCombined()

		trustDeadline, _ := trust.Context().Deadline()
		combinedDeadline, _ := combined.Context().Deadline()
		require.False(t, combinedDeadline.After(trustDeadline))
		a.trust.invalidate()
		require.ErrorIs(t, combined.Check(t.Context()), context.Canceled)
		_, _, err = current.AdmitWithTrust(t.Context(), trust)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestTrustAdmissionProcessCancellation(t *testing.T) {
	f := newServingFixture(t)
	a := f.a.authority

	process, cancel := context.WithCancel(t.Context())
	defer cancel()

	a.BindProcess(process)
	guard, stop, err := a.AdmitTrust(t.Context())
	require.NoError(t, err)

	defer stop()

	cancel()
	require.ErrorIs(t, guard.Check(t.Context()), context.Canceled)
	_, _, err = a.AdmitTrust(t.Context())
	require.ErrorIs(t, err, wire.Unavailable)
	<-guard.Context().Done()
	require.ErrorIs(t, guard.Context().Err(), context.Canceled)
}
