// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/Azure/unbounded/pkg/racersdk"
)

type originFuncClient func()

func (originFuncClient) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	panic("constructor must not call HeadObject")
}

func (originFuncClient) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	panic("constructor must not call GetObject")
}

type originMapClient map[string]string

func (originMapClient) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	panic("constructor must not call HeadObject")
}

func (originMapClient) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	panic("constructor must not call GetObject")
}

type originSliceClient []byte

func (originSliceClient) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	panic("constructor must not call HeadObject")
}

func (originSliceClient) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	panic("constructor must not call GetObject")
}

type originChanClient chan struct{}

func (originChanClient) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	panic("constructor must not call HeadObject")
}

func (originChanClient) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	panic("constructor must not call GetObject")
}

type originValueClient int

func (originValueClient) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	panic("constructor must not call HeadObject")
}

func (originValueClient) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	panic("constructor must not call GetObject")
}

func TestNewOriginClientKinds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		client  S3Client
		wantNil bool
	}{
		{name: "nil-interface", wantNil: true},
		{name: "nil-pointer", client: (*originS3Stub)(nil), wantNil: true},
		{name: "nil-function", client: originFuncClient(nil), wantNil: true},
		{name: "nil-map", client: originMapClient(nil), wantNil: true},
		{name: "nil-slice", client: originSliceClient(nil), wantNil: true},
		{name: "nil-channel", client: originChanClient(nil), wantNil: true},
		{name: "pointer", client: &originS3Stub{}},
		{name: "function", client: originFuncClient(func() {})},
		{name: "empty-map", client: originMapClient{}},
		{name: "empty-slice", client: originSliceClient{}},
		{name: "channel", client: make(originChanClient)},
		{name: "zero-value", client: originValueClient(0)},
		{name: "value", client: originValueClient(1)},
		{name: "struct", client: struct{ S3Client }{S3Client: &originS3Stub{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin, err := NewOrigin(tc.client, OriginConfig{Namespace: "store"})
			if tc.wantNil {
				if origin != nil || !errors.Is(err, racersdk.ErrInvalidRequest) {
					t.Fatalf("nil client: origin nil = %t, error = %v", origin == nil, err)
				}

				return
			}

			if origin == nil || err != nil {
				t.Fatalf("nonnil client: origin nil = %t, error = %v", origin == nil, err)
			}
		})
	}
}
