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

// garageOrigin builds an awss3.Origin pointed at the package-level
// Garage with the given bucket. Used by tests that need to wrap
// the origin in a CountingOrigin decorator.
func garageOrigin(ctx context.Context, t *testing.T, bucket string) (origin.Origin, error) {
	t.Helper()

	return awss3.New(ctx, awss3.Config{
		Endpoint:     pkgGarage.Endpoint(),
		Region:       pkgGarage.Region(),
		Bucket:       bucket,
		AccessKey:    pkgGarage.AccessKey(),
		SecretKey:    pkgGarage.SecretKey(),
		UsePathStyle: true,
	}, nil)
}
