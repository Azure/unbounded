// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import "testing"

func TestOriginCopyBufferRetentionAndClearing(t *testing.T) {
	for range cap(originCopyBuffers) + 1 {
		b := new([copyBufferSize]byte)
		b[0], b[len(b)-1] = 1, 2
		releaseOriginBuffer(b)
	}

	if len(originCopyBuffers) != cap(originCopyBuffers) {
		t.Fatal("unbounded retention")
	}

	for range cap(originCopyBuffers) + 1 {
		b := acquireOriginBuffer()
		for _, value := range b {
			if value != 0 {
				t.Fatal("retained payload")
			}
		}
	}
}
