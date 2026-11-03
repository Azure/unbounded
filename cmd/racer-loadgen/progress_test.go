// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Azure/unbounded/pkg/racersdk"
)

type fakeProgress struct {
	*strings.Reader
	n atomic.Int64
}

func (f *fakeProgress) Progress() int64 { return f.n.Load() }

func TestReportProgressCreditsDuringTheDrain(t *testing.T) {
	p := &puller{metrics: newMetrics(prometheus.NewRegistry())}
	body := &fakeProgress{Reader: strings.NewReader("")}
	body.n.Store(100)

	stop := p.reportProgress(body)
	time.Sleep(progressInterval + progressInterval/2)

	if got := testutil.ToFloat64(p.metrics.receivedBytes); got != 100 {
		t.Fatalf("credited %v during the drain, want 100", got)
	}

	if reported := stop(); reported != 100 {
		t.Fatalf("stop reported %d, want 100", reported)
	}
}

func TestReportProgressIgnoresPlainReaders(t *testing.T) {
	p := &puller{metrics: newMetrics(prometheus.NewRegistry())}
	if reported := p.reportProgress(strings.NewReader("x"))(); reported != 0 {
		t.Fatalf("plain reader reported %d", reported)
	}
}

func TestFailureCauseNamesClassesWithoutText(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("read blob: %w", io.ErrUnexpectedEOF):              "unexpected_eof",
		fmt.Errorf("get: %w", racersdk.ErrUnavailable):                "unavailable",
		context.DeadlineExceeded:                                      "deadline_exceeded",
		errors.New("http://user:password@secret/path?token=secret\n"): "other",
	} {
		if got := failureCause(err); got != want {
			t.Fatalf("failureCause(%q) = %q, want %q", err, got, want)
		}
	}
}
