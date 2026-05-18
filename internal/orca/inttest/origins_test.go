// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"context"
	"testing"

	"github.com/Azure/unbounded/internal/orca/origin"
	"github.com/Azure/unbounded/internal/orca/origin/awss3"
)

// localStackOrigin builds an awss3.Origin pointed at the package-level
// LocalStack with the given bucket. Used by tests that need to wrap
// the origin in a CountingOrigin decorator.
func localStackOrigin(ctx context.Context, t *testing.T, bucket string) (origin.Origin, error) {
	t.Helper()

	return awss3.New(ctx, awss3.Config{
		Endpoint:     pkgLocalStack.Endpoint(),
		Region:       pkgLocalStack.Region(),
		Bucket:       bucket,
		AccessKey:    pkgLocalStack.AccessKey(),
		SecretKey:    pkgLocalStack.SecretKey(),
		UsePathStyle: true,
	}, nil)
}
