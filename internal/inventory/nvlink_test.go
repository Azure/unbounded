package inventory

import (
	"encoding/json"
	"os"
	"testing"
)

func TestParseIMEXOutput(t *testing.T) {
	data, err := os.ReadFile("testdata/nvidia-imex-ctl.txt")
	if err != nil {
		t.Fatalf("failed to read testdata: %v", err)
	}

	records, err := parseIMEXOutput("test-host", data)
	if err != nil {
		t.Fatalf("parseIMEXOutput: %v", err)
	}

	if len(records) != 17 {
		t.Fatalf("expected 17 records, got %d", len(records))
	}

	// Build a map by node index for deterministic assertions.
	byIndex := make(map[int]IMEXNodeAttributes)

	for _, r := range records {
		if r.HostIdentifier != "test-host" {
			t.Errorf("unexpected host identifier %q", r.HostIdentifier)
		}

		var attrs IMEXNodeAttributes
		if err := json.Unmarshal(r.Attributes, &attrs); err != nil {
			t.Fatalf("unmarshal attributes for %s: %v", r.LocalInterface, err)
		}

		byIndex[attrs.NodeIndex] = attrs
	}

	checks := []struct {
		index    int
		ip       string
		status   string
		version  string
		hostname string
		isLocal  bool
		iface    string
	}{
		{
			index:    0,
			ip:       "1.1.1.1",
			status:   "READY",
			version:  "580.95.09",
			hostname: "node-01",
			isLocal:  false,
			iface:    "imex-domain/node0",
		},
		{
			index:    1,
			ip:       "1.1.1.2",
			status:   "UNAVAILABLE",
			version:  "",
			hostname: "node-02",
			isLocal:  false,
			iface:    "imex-domain/node1",
		},
		{
			index:    4,
			ip:       "1.1.1.5",
			status:   "READY",
			version:  "580.95.09",
			hostname: "node-05",
			isLocal:  true,
			iface:    "imex-domain/node4",
		},
		{
			index:    16,
			ip:       "1.1.1.17",
			status:   "READY",
			version:  "580.95.09",
			hostname: "node-17",
			isLocal:  false,
			iface:    "imex-domain/node16",
		},
	}

	for _, check := range checks {
		attrs, ok := byIndex[check.index]
		if !ok {
			t.Errorf("missing record for node index %d", check.index)

			continue
		}

		if attrs.IPAddress != check.ip {
			t.Errorf("node %d: IPAddress = %q, want %q", check.index, attrs.IPAddress, check.ip)
		}

		if attrs.Status != check.status {
			t.Errorf("node %d: Status = %q, want %q", check.index, attrs.Status, check.status)
		}

		if attrs.Version != check.version {
			t.Errorf("node %d: Version = %q, want %q", check.index, attrs.Version, check.version)
		}

		if attrs.Hostname != check.hostname {
			t.Errorf("node %d: Hostname = %q, want %q", check.index, attrs.Hostname, check.hostname)
		}

		if attrs.IsLocal != check.isLocal {
			t.Errorf("node %d: IsLocal = %v, want %v", check.index, attrs.IsLocal, check.isLocal)
		}
	}

	// Verify LocalInterface format on the matching records.
	ifaceByIndex := make(map[int]string)

	for _, r := range records {
		var attrs IMEXNodeAttributes
		if err := json.Unmarshal(r.Attributes, &attrs); err == nil {
			ifaceByIndex[attrs.NodeIndex] = r.LocalInterface
		}
	}

	for _, check := range checks {
		if got := ifaceByIndex[check.index]; got != check.iface {
			t.Errorf("node %d: LocalInterface = %q, want %q", check.index, got, check.iface)
		}
	}
}

func TestParseIMEXOutput_Empty(t *testing.T) {
	records, err := parseIMEXOutput("test-host", []byte(""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(records) != 0 {
		t.Errorf("expected 0 records for empty input, got %d", len(records))
	}
}

func TestParseIMEXOutput_HeaderOnly(t *testing.T) {
	input := `Connectivity Table Legend:
I - Invalid
N - Never Connected
C - Connected

3/31/2026 16:57:17.721
Nodes:
`

	records, err := parseIMEXOutput("test-host", []byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(records) != 0 {
		t.Errorf("expected 0 records for header-only input, got %d", len(records))
	}
}
