// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"errors"
	"testing"
)

func TestIsPortInUseError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "unrelated", err: errors.New("kubeconfig not found"), want: false},
		{
			name: "kubectl bind",
			err: errors.New(
				`port-forward did not start: kubectl exited before forwarding: EOF; ` +
					`output:  (stderr: Unable to listen on port 8443: Listeners failed to create with the ` +
					`following errors: [unable to create listener: Error listen tcp4 127.0.0.1:8443: ` +
					`bind: address already in use unable to create listener: Error listen tcp6 [::1]:8443: ` +
					`bind: address already in use]
error: unable to listen on any of the requested ports: [{8443 8443}])`,
			),
			want: true,
		},
		{
			name: "only address already in use",
			err:  errors.New("bind: address already in use"),
			want: true,
		},
		{
			name: "only listen-on-any wrapper",
			err:  errors.New("error: unable to listen on any of the requested ports: [{8443 8443}]"),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isPortInUseError(tt.err); got != tt.want {
				t.Fatalf("isPortInUseError(%q) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
