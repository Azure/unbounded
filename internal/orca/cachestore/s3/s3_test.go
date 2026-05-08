// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package s3

import (
	"strings"
	"testing"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// TestValidateBucketVersioning covers every BucketVersioningStatus
// branch the gate cares about. The integration suite only exercises
// the Enabled case end-to-end; this unit test fills in the empty
// (never-enabled) and Suspended cases.
func TestValidateBucketVersioning(t *testing.T) {
	tests := []struct {
		name    string
		status  s3types.BucketVersioningStatus
		wantErr bool
	}{
		{"empty (never enabled)", "", false},
		{"enabled", s3types.BucketVersioningStatusEnabled, true},
		{"suspended", s3types.BucketVersioningStatusSuspended, true},
	}

	const bucket = "test-bucket"

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBucketVersioning(bucket, tt.status)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tt.wantErr)
			}

			if !tt.wantErr {
				return
			}

			if !strings.Contains(err.Error(), bucket) {
				t.Errorf("error %q does not include bucket name %q", err, bucket)
			}

			if !strings.Contains(err.Error(), string(tt.status)) {
				t.Errorf("error %q does not include status %q", err, tt.status)
			}
		})
	}
}
