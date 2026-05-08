// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azureblob

import (
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/Azure/unbounded/internal/orca/origin"
)

// TestValidateBlobType covers every branch of the EnforceBlockBlobOnly
// gate. The integration suite previously only exercised the
// PageBlob-refused case; this unit test fills in disabled, nil,
// BlockBlob, and AppendBlob.
func TestValidateBlobType(t *testing.T) {
	pageBlob := blob.BlobTypePageBlob
	appendBlob := blob.BlobTypeAppendBlob
	blockBlob := blob.BlobTypeBlockBlob

	tests := []struct {
		name            string
		enforce         bool
		blobType        *blob.BlobType
		wantUnsupported bool
	}{
		{"enforce off accepts any type", false, &pageBlob, false},
		{"nil blob type passes when enforced (no info)", true, nil, false},
		{"block blob accepted", true, &blockBlob, false},
		{"page blob refused", true, &pageBlob, true},
		{"append blob refused", true, &appendBlob, true},
	}

	const (
		container = "ctr"
		key       = "key"
	)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBlobType(tt.enforce, container, key, tt.blobType)

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
