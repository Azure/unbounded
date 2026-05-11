// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azureblob

import (
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/Azure/unbounded/internal/orca/origin"
)

// TestValidateBlobType covers every branch of the unconditional
// block-blob-only enforcement. PageBlob and AppendBlob are always
// rejected; BlockBlob and the nil/unknown response shape pass.
func TestValidateBlobType(t *testing.T) {
	pageBlob := blob.BlobTypePageBlob
	appendBlob := blob.BlobTypeAppendBlob
	blockBlob := blob.BlobTypeBlockBlob

	tests := []struct {
		name            string
		blobType        *blob.BlobType
		wantUnsupported bool
	}{
		{"nil blob type passes (no info to validate)", nil, false},
		{"block blob accepted", &blockBlob, false},
		{"page blob refused", &pageBlob, true},
		{"append blob refused", &appendBlob, true},
	}

	const (
		container = "ctr"
		key       = "key"
	)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBlobType(container, key, tt.blobType)

			if (err != nil) != tt.wantUnsupported {
				t.Fatalf("err=%v, wantUnsupported=%v", err, tt.wantUnsupported)
			}

			if !tt.wantUnsupported {
				return
			}

			var ube *origin.UnsupportedBlobTypeError
			if !errors.As(err, &ube) {
				t.Fatalf("err type=%T (want *origin.UnsupportedBlobTypeError): %v", err, err)
			}

			if ube.Bucket != container {
				t.Errorf("Bucket=%q want %q", ube.Bucket, container)
			}

			if ube.Key != key {
				t.Errorf("Key=%q want %q", ube.Key, key)
			}

			if tt.blobType != nil && ube.BlobType != string(*tt.blobType) {
				t.Errorf("BlobType=%q want %q", ube.BlobType, string(*tt.blobType))
			}
		})
	}
}

// TestValidateBlobType_NonBlockBlob_AlwaysRejected is the regression
// test for the fix that removed the user-overridable
// EnforceBlockBlobOnly flag. There is no longer any code path that
// accepts a Page or Append blob.
func TestValidateBlobType_NonBlockBlob_AlwaysRejected(t *testing.T) {
	pageBlob := blob.BlobTypePageBlob

	if err := validateBlobType("ctr", "key", &pageBlob); err == nil {
		t.Fatalf("page blob accepted; want UnsupportedBlobTypeError")
	}

	appendBlob := blob.BlobTypeAppendBlob
	if err := validateBlobType("ctr", "key", &appendBlob); err == nil {
		t.Fatalf("append blob accepted; want UnsupportedBlobTypeError")
	}
}
