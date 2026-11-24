// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

// fakeOriginClient implements originClient in-memory so that tests
// can drive runRoundtripWith / runScenarioColdWarmWith / cache
// inspect without an Azure or S3 backend. The zero value is unusable;
// callers seed objects via .put() before invoking the code under
// test.
type fakeOriginClient struct {
	driver  string
	bucket  string
	objects map[string]fakeOriginObject

	headErr   error
	deleteErr error
}

type fakeOriginObject struct {
	body []byte
	etag string
}

func newFakeOriginClient(driver, bucket string) *fakeOriginClient {
	return &fakeOriginClient{
		driver:  driver,
		bucket:  bucket,
		objects: map[string]fakeOriginObject{},
	}
}

func (f *fakeOriginClient) put(key, etag string, body []byte) {
	f.objects[key] = fakeOriginObject{body: body, etag: etag}
}

func (f *fakeOriginClient) Driver() string { return f.driver }
func (f *fakeOriginClient) Bucket() string { return f.bucket }

func (f *fakeOriginClient) EnsureBucket(_ context.Context) error { return nil }

func (f *fakeOriginClient) Head(_ context.Context, key string) (originInfo, error) {
	if f.headErr != nil {
		return originInfo{}, f.headErr
	}

	obj, ok := f.objects[key]
	if !ok {
		return originInfo{}, fmt.Errorf("fake origin: %q not found", key)
	}

	return originInfo{Size: int64(len(obj.body)), ETag: obj.etag}, nil
}

func (f *fakeOriginClient) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	obj, ok := f.objects[key]
	if !ok {
		return nil, 0, fmt.Errorf("fake origin: %q not found", key)
	}

	return io.NopCloser(bytes.NewReader(obj.body)), int64(len(obj.body)), nil
}

func (f *fakeOriginClient) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}

	f.objects[key] = fakeOriginObject{body: body, etag: fmt.Sprintf("fake-etag-%d", len(f.objects)+1)}

	return nil
}

func (f *fakeOriginClient) List(_ context.Context, prefix string, limit int) ([]originObject, error) {
	var out []originObject

	for name, obj := range f.objects {
		if prefix != "" && len(name) < len(prefix) {
			continue
		}

		if prefix != "" && name[:len(prefix)] != prefix {
			continue
		}

		out = append(out, originObject{Name: name, Size: int64(len(obj.body)), ETag: obj.etag})
		if limit > 0 && len(out) >= limit {
			break
		}
	}

	return out, nil
}

func (f *fakeOriginClient) Delete(_ context.Context, key string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}

	delete(f.objects, key)

	return nil
}

// compile-time check that the fake satisfies the production
// interface.
var _ originClient = (*fakeOriginClient)(nil)
