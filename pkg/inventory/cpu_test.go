package inventory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectCpuInfo(t *testing.T) {
	hostArch := detectArchitecture()

	tests := []struct {
		name            string
		file            string
		wantModel       string
		wantArch        string
		wantLogical     int
		wantPhysical    int
		wantCoresPerCPU string
		wantThreadsCore string
		wantMicrocode   string
		wantAddrSizes   string
	}{
		{
			name:            "cpuinfo-1 (Intel Xeon E-2414, 4 logical, 1 socket)",
			file:            "testdata/cpuinfo-1.txt",
			wantModel:       "Intel(R) Xeon(R) E E-2414",
			wantArch:        hostArch,
			wantLogical:     4,
			wantPhysical:    1,
			wantCoresPerCPU: "4",
			wantThreadsCore: "1",
			wantMicrocode:   "0x12f",
			wantAddrSizes:   "46 bits physical, 48 bits virtual",
		},
		{
			name:            "cpuinfo-2 (ARM, 144 logical, no physical id)",
			file:            "testdata/cpuinfo-2.txt",
			wantArch:        hostArch,
			wantLogical:     144,
			wantPhysical:    1,
			wantCoresPerCPU: "",
			wantThreadsCore: "unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			records, err := collectCpuInfo(ctx, "test-host", false, tc.file)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(records) != 1 {
				t.Fatalf("expected 1 record, got %d", len(records))
			}

			rec := records[0]

			if rec.DeviceType != DeviceTypeCPU {
				t.Errorf("device type = %q, want %q", rec.DeviceType, DeviceTypeCPU)
			}

			if rec.HostIdentifier != "test-host" {
				t.Errorf("host identifier = %q, want %q", rec.HostIdentifier, "test-host")
			}

			var attrs CPUInfo
			if err := json.Unmarshal(rec.Attributes, &attrs); err != nil {
				t.Fatalf("failed to unmarshal attributes: %v", err)
			}

			if tc.wantModel != "" && rec.DeviceName != tc.wantModel {
				t.Errorf("device name = %q, want %q", rec.DeviceName, tc.wantModel)
			}

			if attrs.Architecture != tc.wantArch {
				t.Errorf("architecture = %q, want %q", attrs.Architecture, tc.wantArch)
			}

			if attrs.LogicalCPUCount != tc.wantLogical {
				t.Errorf("logical CPU count = %d, want %d", attrs.LogicalCPUCount, tc.wantLogical)
			}

			if attrs.PhysicalCPUCount != tc.wantPhysical {
				t.Errorf("physical CPU count = %d, want %d", attrs.PhysicalCPUCount, tc.wantPhysical)
			}

			if attrs.CoresPerCPU != tc.wantCoresPerCPU {
				t.Errorf("cores per CPU = %q, want %q", attrs.CoresPerCPU, tc.wantCoresPerCPU)
			}

			if attrs.ThreadsPerCore != tc.wantThreadsCore {
				t.Errorf("threads per core = %q, want %q", attrs.ThreadsPerCore, tc.wantThreadsCore)
			}

			if tc.wantMicrocode != "" && attrs.Microcode != tc.wantMicrocode {
				t.Errorf("microcode = %q, want %q", attrs.Microcode, tc.wantMicrocode)
			}

			if tc.wantAddrSizes != "" && attrs.AddressSizes != tc.wantAddrSizes {
				t.Errorf("address sizes = %q, want %q", attrs.AddressSizes, tc.wantAddrSizes)
			}
		})
	}
}

func TestCollectCpuInfo_Errors(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "empty file",
			content: "",
		},
		{
			name:    "no processor entries",
			content: "bogus line\nanother bogus line\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpFile := filepath.Join(t.TempDir(), "cpuinfo")
			if err := os.WriteFile(tmpFile, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("failed to write test file: %v", err)
			}

			records, err := collectCpuInfo(context.Background(), "test-host", false, tmpFile)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(records) != 0 {
				t.Errorf("expected 0 records for empty/bogus input, got %d", len(records))
			}
		})
	}
}

func TestCollectCpuInfo_MissingFile(t *testing.T) {
	_, err := collectCpuInfo(context.Background(), "test-host", false, filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
