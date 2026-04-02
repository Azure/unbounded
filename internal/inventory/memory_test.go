package inventory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadTotalMemory(t *testing.T) {
	tests := []struct {
		name      string
		file      string
		wantBytes uint64
	}{
		{
			name:      "meminfo-1 (~15.5 GiB)",
			file:      "testdata/meminfo-1.txt",
			wantBytes: 16274424 * 1024,
		},
		{
			name:      "meminfo-2 (~1.6 TiB)",
			file:      "testdata/meminfo-2.txt",
			wantBytes: 1774993472 * 1024,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readTotalMemory(tc.file)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tc.wantBytes {
				t.Errorf("got %d bytes, want %d", got, tc.wantBytes)
			}
		})
	}
}

func TestReadTotalMemory_Errors(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "missing MemTotal",
			content: "MemFree:         8192000 kB\nMemAvailable:    7000000 kB\n",
		},
		{
			name:    "malformed MemTotal value",
			content: "MemTotal:       notanumber kB\n",
		},
		{
			name:    "empty file",
			content: "",
		},
		{
			name:    "truncated MemTotal line",
			content: "MemTotal:\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpFile := filepath.Join(t.TempDir(), "meminfo")
			if err := os.WriteFile(tmpFile, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("failed to write test file: %v", err)
			}

			_, err := readTotalMemory(tmpFile)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestReadTotalMemory_MissingFile(t *testing.T) {
	_, err := readTotalMemory(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestCollectMemoryInfo(t *testing.T) {
	tests := []struct {
		name      string
		file      string
		wantBytes uint64
	}{
		{
			name:      "meminfo-1",
			file:      "testdata/meminfo-1.txt",
			wantBytes: 16274424 * 1024,
		},
		{
			name:      "meminfo-2",
			file:      "testdata/meminfo-2.txt",
			wantBytes: 1774993472 * 1024,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			records, err := collectMemoryInfo(ctx, "test-host", false, tc.file)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(records) != 1 {
				t.Fatalf("expected 1 record, got %d", len(records))
			}

			rec := records[0]

			if rec.DeviceType != DeviceTypeMemory {
				t.Errorf("device type = %q, want %q", rec.DeviceType, DeviceTypeMemory)
			}

			if rec.HostIdentifier != "test-host" {
				t.Errorf("host identifier = %q, want %q", rec.HostIdentifier, "test-host")
			}

			var attrs MemoryInfo
			if err := json.Unmarshal(rec.Attributes, &attrs); err != nil {
				t.Fatalf("failed to unmarshal attributes: %v", err)
			}

			if attrs.TotalBytes != tc.wantBytes {
				t.Errorf("total bytes = %d, want %d", attrs.TotalBytes, tc.wantBytes)
			}
		})
	}
}
