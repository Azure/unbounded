// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerblock

import (
	"math"
	"testing"
)

func TestDeviceSize(t *testing.T) {
	tests := []struct {
		in      int64
		want    int64
		wantErr bool
	}{
		{in: 1, want: 512},
		{in: 511, want: 512},
		{in: 512, want: 512},
		{in: 513, want: 1024},
		{in: 16 << 20, want: 16 << 20},
		{in: 16<<20 + 1, want: 16<<20 + 512},
		{in: math.MaxInt64 - 511, want: math.MaxInt64 - 511},
		{in: math.MaxInt64 - 510, wantErr: true},
		{in: math.MaxInt64, wantErr: true},
		{in: 0, wantErr: true},
		{in: -1, wantErr: true},
	}

	for _, tt := range tests {
		got, err := DeviceSize(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("DeviceSize(%d) = %d, want error", tt.in, got)
			}

			continue
		}

		if err != nil || got != tt.want {
			t.Errorf("DeviceSize(%d) = %d, %v; want %d", tt.in, got, err, tt.want)
		}
	}
}
