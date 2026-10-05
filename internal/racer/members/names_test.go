// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package members

import (
	"reflect"
	"testing"
)

func TestManagedNames(t *testing.T) {
	for _, tt := range []struct {
		name string
		want []string
	}{
		{DataplaneDaemonSetName, []string{DataplaneDaemonSetName, PodNetworkDaemonSetName}},
		{"custom-racer", []string{"custom-racer"}},
		{PodNetworkDaemonSetName, []string{PodNetworkDaemonSetName}},
		{"", []string{""}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := ManagedNames(tt.name)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ManagedNames(%q) = %v, want %v", tt.name, got, tt.want)
			}

			got[0] = "mutated"

			if !reflect.DeepEqual(ManagedNames(tt.name), tt.want) {
				t.Fatal("caller mutation changed managed names")
			}
		})
	}
}
