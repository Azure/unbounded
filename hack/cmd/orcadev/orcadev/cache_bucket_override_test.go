// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import "testing"

func TestCacheInspectOverridesOriginBucketBeforeClientConstruction(t *testing.T) {
	t.Parallel()

	g := defaultGlobalFlags()
	g.originBucket = "default-bucket"

	o := &cacheInspectOpts{
		bucket:    "requested-bucket",
		key:       "key",
		etag:      "etag",
		chunkSize: "1MiB",
	}

	prepareCacheInspectOriginBucket(g, o)

	if g.originBucket != "requested-bucket" {
		t.Fatalf("originBucket = %q, want requested-bucket", g.originBucket)
	}
}
