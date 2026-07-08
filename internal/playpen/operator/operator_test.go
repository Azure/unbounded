// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import "testing"

func TestNormalizeArchitecture(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "default", want: ArchitectureAMD64},
		{name: "amd64", value: ArchitectureAMD64, want: ArchitectureAMD64},
		{name: "arm64", value: ArchitectureARM64, want: ArchitectureARM64},
		{name: "bad", value: "s390x", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeArchitecture(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}

			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAllocationIDIsStableAndShort(t *testing.T) {
	first := allocationID("key")
	second := allocationID("key")
	if first != second {
		t.Fatalf("ids differ: %s != %s", first, second)
	}

	if len(first) != 16 {
		t.Fatalf("id length = %d", len(first))
	}
}

func TestAllocationParamsAreDeterministic(t *testing.T) {
	cfg := DefaultConfig()
	first := allocationParams("0123456789abcdef", 51820, cfg)
	second := allocationParams("0123456789abcdef", 51820, cfg)
	if first != second {
		t.Fatalf("params differ: %#v != %#v", first, second)
	}

	if first.mac == "" || first.serverWG == "" || first.clientWG == "" || first.vni == 0 {
		t.Fatalf("incomplete params: %#v", first)
	}
}
