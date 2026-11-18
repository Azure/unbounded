// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package streamcopy

import (
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

func TestObserveReportsLiveBytesAndFinishesExactlyOnce(t *testing.T) {
	var active, bytes atomic.Int64

	done := BeginActivity(func(delta int) { active.Add(int64(delta)) })
	body := Observe(io.NopCloser(strings.NewReader("payload")), func(n int64) { bytes.Add(n) }, done)

	if active.Load() != 1 {
		t.Fatal("transfer did not start")
	}

	if _, err := io.ReadFull(body, make([]byte, 3)); err != nil {
		t.Fatal(err)
	}

	if bytes.Load() != 3 || active.Load() != 1 {
		t.Fatal("bytes were delayed until transfer completion")
	}

	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}

	if active.Load() != 0 || bytes.Load() != 7 {
		t.Fatal("terminal read did not finish transfer")
	}

	for range 2 {
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
	}

	done()

	if active.Load() != 0 || bytes.Load() != 7 {
		t.Fatal("close double-counted completion or bytes")
	}
}

type failingObservedBody struct{}

func (failingObservedBody) Read([]byte) (int, error) { return 0, errors.New("connection failed") }
func (failingObservedBody) Close() error             { return errors.New("close failed") }

func TestObserveFinishesFailuresAndPartialCloses(t *testing.T) {
	for _, underlying := range []io.ReadCloser{failingObservedBody{}, io.NopCloser(strings.NewReader("longer payload"))} {
		var active atomic.Int64

		body := Observe(underlying, nil, BeginActivity(func(delta int) { active.Add(int64(delta)) }))
		_, _ = body.Read(make([]byte, 1))
		_ = body.Close()

		if active.Load() != 0 {
			t.Fatal("failed or abandoned transfer remained active")
		}
	}
}
