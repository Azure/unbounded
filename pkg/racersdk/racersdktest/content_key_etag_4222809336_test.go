// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdktest

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestContentKeySameBytesNewETag(t *testing.T) {
	const content = "unchanged bytes"

	request := racersdk.Request{Key: racersdk.Key(sha256.Sum256([]byte(content)))}

	var current atomic.Value
	current.Store(`"v1"`)

	client := NewClient(t, func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if r.Key != request.Key {
			t.Error("origin received a different content key")
		}

		m := originMeta(int64(len(content)))

		m.ETag = current.Load().(string)
		if r.Head {
			return m, nil, nil
		}

		// Return the current ETag so the SDK rejects an unavailable pin.
		return m, io.NopCloser(strings.NewReader(content)), nil
	})

	read := func(wantETag string, options ...racersdk.ReadOptions) {
		t.Helper()

		object, err := client.Get(t.Context(), request, options...)
		if err != nil {
			t.Fatal(err)
		}
		defer object.Close()

		data, err := io.ReadAll(object)
		if err != nil || string(data) != content {
			t.Fatalf("read = %q, %v; want %q", data, err, content)
		}

		if got := object.Metadata().ETag; got != wantETag {
			t.Fatalf("ETag = %q; want %q", got, wantETag)
		}
	}

	stat, err := client.Stat(t.Context(), request)
	if err != nil || stat.ETag != `"v1"` {
		t.Fatalf("initial Stat ETag = %q, error = %v", stat.ETag, err)
	}

	pin := racersdk.ReadOptions{ETag: stat.ETag}
	read(`"v1"`, pin)

	current.Store(`"v2"`)
	read(`"v2"`)
	read(`"v2"`, racersdk.ReadOptions{ETag: `"v2"`})

	object, err := client.Get(t.Context(), request, pin)
	if object != nil {
		defer object.Close()
	}

	if !errors.Is(err, racersdk.ErrVersionMismatch) {
		t.Fatalf("old pin with identical bytes: error = %v; want ErrVersionMismatch", err)
	}
}
