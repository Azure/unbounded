// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"io"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOffsetOriginClampsRangeWithoutOverflow(t *testing.T) {
	const finalPageStart = math.MaxInt64 / PageSize * PageSize

	for _, tt := range []struct {
		name    string
		size    int64
		request OriginRequest
		want    int64
	}{
		{"normal page", 2 * PageSize, OriginRequest{Offset: PageSize, Length: PageSize}, PageSize},
		{"short final page", PageSize + 7, OriginRequest{Offset: PageSize, Length: PageSize}, 7},
		{"empty object", 0, OriginRequest{Length: PageSize}, 0},
		{"at end", PageSize, OriginRequest{Offset: PageSize, Length: PageSize}, 0},
		{"past end", 7, OriginRequest{Offset: PageSize, Length: PageSize}, 0},
		{"zero length", PageSize, OriginRequest{}, 0},
		{"head", math.MaxInt64, OriginRequest{Head: true}, 0},
		{"near max short page", finalPageStart + 7, OriginRequest{Offset: finalPageStart, Length: PageSize}, 7},
		{"max size final page", math.MaxInt64, OriginRequest{Offset: finalPageStart, Length: PageSize}, PageSize - 1},
		{"near max at end", finalPageStart, OriginRequest{Offset: finalPageStart, Length: PageSize}, 0},
		{"near max past end", finalPageStart - 1, OriginRequest{Offset: finalPageStart, Length: PageSize}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			metadata, body, err := offsetOrigin(tt.size)(t.Context(), tt.request)
			if body != nil {
				t.Cleanup(func() { require.NoError(t, body.Close()) })
			}

			require.NoError(t, err)
			require.Equal(t, originMeta(tt.size), metadata)

			if tt.want == 0 {
				require.Nil(t, body)
				return
			}

			require.NotNil(t, body)

			n, err := io.Copy(&offsetSink{offset: tt.request.Offset}, body)
			require.NoError(t, err)
			require.Equal(t, tt.want, n)
		})
	}
}
