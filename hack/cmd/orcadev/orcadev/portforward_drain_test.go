// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package orcadev

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestWaitForForwardingAndDrainKeepsReadingAfterSentinel(t *testing.T) {
	t.Parallel()

	r, w := io.Pipe()
	done := make(chan error, 1)

	go func() {
		_, err := io.WriteString(w, "Forwarding from 127.0.0.1:8443 -> 8443\n")
		if err != nil {
			done <- err

			return
		}

		line := strings.Repeat("Handling connection for 8443\n", 1024)
		for i := 0; i < 128; i++ {
			if _, err := io.WriteString(w, line); err != nil {
				done <- err

				return
			}
		}

		done <- w.Close()
	}()

	if err := waitForForwardingAndDrain(r); err != nil {
		t.Fatalf("waitForForwardingAndDrain() error = %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("writer error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer blocked; stdout was not drained after readiness")
	}
}
