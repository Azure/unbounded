// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/ifaces"
	sdk "github.com/Azure/unbounded/pkg/racersdk"
)

func TestRacerFailureSamplingBoundedAndIndependent(t *testing.T) {
	d := &racerDiagnostics{}
	now := time.Now()

	var (
		accepted atomic.Int32
		wg       sync.WaitGroup
	)
	for range 100 {
		wg.Go(func() {
			if _, ok := d.sample(racerForward, now); ok {
				accepted.Add(1)
			}
		})
	}

	wg.Wait()

	if accepted.Load() != 1 {
		t.Fatal(accepted.Load())
	}

	if n, ok := d.sample(racerForward, now.Add(30*time.Second)); !ok || n != 99 {
		t.Fatal(n, ok)
	}

	for _, phase := range []racerFailurePhase{racerAdmission, racerHead, racerPrepare} {
		if n, ok := d.sample(phase, now); !ok || n != 0 {
			t.Fatal(phase, n, ok)
		}
	}
}

func TestRacerFailureLogRedactionAndCancellation(t *testing.T) {
	var output bytes.Buffer

	s := &Server{logger: slog.New(slog.NewJSONHandler(&output, nil)), racer: &racerState{}}
	ref := ifaces.OriginRef{Registry: "registry-secret", Repository: "repo-secret", Digest: digest.MustParse("sha256:" + strings.Repeat("a", 64))}
	s.reportRacerFailure(ref, racerHead, context.Canceled, nil, 0)

	if output.Len() != 0 {
		t.Fatal("cancellation consumed a sample")
	}

	err := &sdk.HTTPError{Method: "HEAD", Target: "/target?token=target-secret", StatusCode: 503, WWWAuthenticate: "challenge-secret", RetryAfter: "retry-secret"}
	s.reportRacerFailure(ref, racerHead, err, nil, 0)

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}

	if record["phase"] != "HEAD" || record["racer_status"] != float64(503) || record["error_class"] != "http_status" || record["digest"] != ref.Digest.String() || record["page_offset"] != float64(-1) {
		t.Fatal(record)
	}

	if strings.Contains(output.String(), "secret") {
		t.Fatal("request metadata leaked", output.String())
	}

	length := output.Len()

	s.reportRacerFailure(ref, racerHead, errors.New("raw-secret"), nil, 0)

	if output.Len() != length {
		t.Fatal("rate limit did not suppress second error")
	}

	s.reportRacerFailure(ref, racerPrepare, errors.New("raw-secret"), nil, 0)

	if strings.Contains(output.String(), "secret") || output.Len() == length {
		t.Fatal("phase isolation or redaction failed")
	}
}
