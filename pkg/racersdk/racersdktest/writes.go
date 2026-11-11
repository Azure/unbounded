// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdktest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/internal/fakeracer"
	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// WrittenObject is an object created through the fake's write API.
type WrittenObject struct {
	Data          []byte
	ETag          string
	ContentType   string
	Metadata      string
	Replicas      int
	OriginDurable bool
	Upload        racersdk.UploadMode
	Hold          time.Duration
	UploadState   racersdk.UploadState
}

// WriteCall is one write request the fake received.
type WriteCall struct {
	// Kind is "put", "host-write", "persist", or "delete".
	Kind string
	Key  racersdk.Key
	// Header holds the request header fields.
	Header map[string][]string
	// Files is how many descriptors arrived with the request head.
	Files int
}

// WriteRejection is a failure the fake sends instead of performing a write.
type WriteRejection struct {
	// Status is the HTTP status, such as 503.
	Status int
	// Code is sent as Racer-Write-Error when nonempty.
	Code string
	// RetryAfter is sent as Retry-After, in whole seconds, when positive.
	RetryAfter time.Duration
}

// Writes is the fake's in-memory write directory. It accepts writes with the
// header rules of the Racer write API: create-once, durability and upload
// options, hold bounds, and SCM_RIGHTS host-write. Written objects are
// readable through the same client with Get, HostRead, and Stat. Nothing is
// replicated or uploaded; upload states change only through [Writes.SetUploadState]
// and Persist.
type Writes struct {
	dir *fakeracer.Writes
}

// NewClientWithWrites is [NewClient] with the write API enabled. origin may
// be nil for a cache whose only objects are written ones. The default upload
// mode is auto.
func NewClientWithWrites(t testing.TB, origin racersdk.Origin) (*racersdk.Client, *Writes) {
	t.Helper()

	if origin == nil {
		origin = func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
			return racersdk.Metadata{}, nil, racersdk.ErrNotFound
		}
	}

	w := &Writes{dir: fakeracer.NewWrites()}

	client, cleanup, err := startWith(origin, w)
	if err != nil {
		t.Fatalf("racersdktest: %v", err)
	}

	t.Cleanup(cleanup)

	return client, w
}

func (w *Writes) wrap(next racersdk.Origin) racersdk.Origin {
	w.dir.SetOriginExists(func(ctx context.Context, key [32]byte, metadata, authorization string) (bool, error) {
		request := racersdk.OriginRequest{Request: racersdk.Request{Key: key, Metadata: metadata, Authorization: authorization}, Head: true}

		_, body, err := callOrigin(ctx, next, request)
		if body != nil {
			closeBody(body)
		}

		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, racersdk.ErrNotFound):
			return false, nil
		default:
			return false, err
		}
	})

	return func(ctx context.Context, request racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		o, ok := w.dir.Object(request.Key)
		if !ok {
			return next(ctx, request)
		}

		m := racersdk.Metadata{Size: int64(len(o.Data)), ETag: o.ETag, ContentType: o.ContentType, ExpiresAt: time.Now().Add(time.Hour)}
		if request.ETag != "" && request.ETag != o.ETag {
			return racersdk.Metadata{}, nil, racersdk.ErrVersionMismatch
		}

		if request.Head || request.Offset >= m.Size {
			return m, nil, nil
		}

		end := min(m.Size, request.Offset+request.Length)

		return m, io.NopCloser(bytes.NewReader(o.Data[request.Offset:end])), nil
	}
}

// callOrigin recovers origin panics so a create-once probe cannot crash the fake.
func callOrigin(ctx context.Context, origin racersdk.Origin, request racersdk.OriginRequest) (m racersdk.Metadata, body io.ReadCloser, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("origin panic")
		}
	}()

	return origin(ctx, request)
}

// SetEnabled turns the write API on or off. While off, writes fail with
// [racersdk.ErrWritesDisabled], as on a cache without writes.
func (w *Writes) SetEnabled(enabled bool) { w.dir.SetEnabled(enabled) }

// SetDefaultUpload sets the upload mode used when a write names none.
func (w *Writes) SetDefaultUpload(mode racersdk.UploadMode) { w.dir.SetDefaultUpload(string(mode)) }

// Reject installs a hook that runs after header validation. Returning a
// non-nil rejection fails the write with it; nil performs the write.
func (w *Writes) Reject(reject func(WriteCall) *WriteRejection) {
	if reject == nil {
		w.dir.SetReject(nil)
		return
	}

	w.dir.SetReject(func(call fakeracer.WriteCall) *fakeracer.WriteReply {
		r := reject(publicCall(call))
		if r == nil {
			return nil
		}

		return &fakeracer.WriteReply{Status: r.Status, Code: r.Code, RetryAfter: int(r.RetryAfter / time.Second)}
	})
}

// Object returns the written object for key.
func (w *Writes) Object(key racersdk.Key) (WrittenObject, bool) {
	o, ok := w.dir.Object(key)
	if !ok {
		return WrittenObject{}, false
	}

	return WrittenObject{
		Data: o.Data, ETag: o.ETag, ContentType: o.ContentType, Metadata: o.Metadata,
		Replicas: o.Replicas, OriginDurable: o.OriginDurable, Upload: racersdk.UploadMode(o.Upload),
		Hold: time.Duration(o.HoldSeconds) * time.Second, UploadState: racersdk.UploadState(o.State),
	}, true
}

// SetUploadState changes a written object's upload state, for example to
// simulate a committed upload. It reports whether the object exists.
func (w *Writes) SetUploadState(key racersdk.Key, state racersdk.UploadState) bool {
	if !wire.ValidUploadState(string(state)) {
		return false
	}

	return w.dir.SetState(key, string(state))
}

// Calls returns the write requests received so far, in order.
func (w *Writes) Calls() []WriteCall {
	calls := w.dir.Calls()
	out := make([]WriteCall, len(calls))

	for i, c := range calls {
		out[i] = publicCall(c)
	}

	return out
}

func publicCall(c fakeracer.WriteCall) WriteCall {
	return WriteCall{Kind: c.Kind, Key: c.Key, Header: c.Header, Files: c.Files}
}
