// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//go:build integrationtest

package inttest

import (
	"context"
	"testing"

	"github.com/Azure/unbounded/internal/orca/origin"
	"github.com/Azure/unbounded/internal/orca/origin/awss3"
)

// s3BackendOrigin builds an awss3.Origin pointed at the package-level
// S3 backend with the given bucket. Used by tests that need to wrap
// the origin in a CountingOrigin decorator.
func s3BackendOrigin(ctx context.Context, t *testing.T, bucket string) (origin.Origin, error) {
	t.Helper()

	return awss3.New(ctx, awss3.Config{
		Endpoint:     pkgS3.Endpoint(),
		Region:       pkgS3.Region(),
		Bucket:       bucket,
		AccessKey:    pkgS3.AccessKey(),
		SecretKey:    pkgS3.SecretKey(),
		UsePathStyle: true,
	}, nil)
}
