package site

import (
	"bytes"
	"strings"
	"testing"

	"github.com/project-unbounded/unbounded-kube/hack/cmd/forge/forge/site/azuredev"
	"github.com/stretchr/testify/require"
)

func TestSanitizeK8sName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "dots in IP preserved",
			input:    "20.48.100.5-50001",
			expected: "20.48.100.5-50001",
		},
		{
			name:     "colons replaced with dashes",
			input:    "10.0.0.1:22",
			expected: "10.0.0.1-22",
		},
		{
			name:     "underscores replaced",
			input:    "vmss_worker_0",
			expected: "vmss-worker-0",
		},
		{
			name:     "uppercase lowered",
			input:    "MyMachine-01",
			expected: "mymachine-01",
		},
		{
			name:     "leading non-alnum stripped",
			input:    "---abc",
			expected: "abc",
		},
		{
			name:     "trailing non-alnum stripped",
			input:    "abc---",
			expected: "abc",
		},
		{
			name:     "consecutive dashes collapsed",
			input:    "a::b",
			expected: "a-b",
		},
		{
			name:     "already valid name unchanged",
			input:    "machine-1",
			expected: "machine-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, sanitizeK8sName(tt.input))
		})
	}
}

func TestMachinaNameFromInventory(t *testing.T) {
	t.Parallel()

	m := azuredev.Machine{
		Name:      "vmss_worker_0",
		IPAddress: "20.48.100.5",
		Port:      50001,
	}

	require.Equal(t, "20.48.100.5-50001", machinaNameFromInventory(m))
}

func TestWriteInventoryAsMachina(t *testing.T) {
	t.Skip()

	t.Parallel()

	inv := &azuredev.Inventory{
		Machines: []azuredev.Machine{
			{Name: "vmss_0", IPAddress: "20.48.100.5", Port: 50001},
			{Name: "vmss_1", IPAddress: "20.48.100.5", Port: 50002},
		},
	}

	var buf bytes.Buffer

	err := WriteInventoryAsMachina(&buf, inv)
	require.NoError(t, err)

	output := buf.String()

	// Should contain the document separator between the two machines.
	require.Equal(t, 1, strings.Count(output, "---\n"))

	// Spot-check apiVersion, kind, names, namespace, ip, port.
	require.Contains(t, output, "apiVersion: machina.unboundedkube.io/v1alpha2")
	require.Contains(t, output, "kind: Machine")
	require.Contains(t, output, "name: 20.48.100.5-50001")
	require.Contains(t, output, "name: 20.48.100.5-50002")
	require.Contains(t, output, "namespace: machina-system")
	require.Contains(t, output, "ip: 20.48.100.5")
	require.Contains(t, output, "port: 50001")
	require.Contains(t, output, "port: 50002")

	// Check vm-name annotations.
	require.Contains(t, output, "forge.unboundedkube.io/vm-name: vmss_0")
	require.Contains(t, output, "forge.unboundedkube.io/vm-name: vmss_1")
}

func TestWriteInventoryAsMachina_Empty(t *testing.T) {
	t.Parallel()

	inv := &azuredev.Inventory{}

	var buf bytes.Buffer

	err := WriteInventoryAsMachina(&buf, inv)
	require.NoError(t, err)
	require.Empty(t, buf.String())
}

func TestWriteInventoryAsMachina_SingleMachine(t *testing.T) {
	t.Parallel()

	inv := &azuredev.Inventory{
		Machines: []azuredev.Machine{
			{Name: "vmss_0", IPAddress: "10.0.0.1", Port: 22},
		},
	}

	var buf bytes.Buffer

	err := WriteInventoryAsMachina(&buf, inv)
	require.NoError(t, err)

	// Single document should have no separator.
	require.NotContains(t, buf.String(), "---")

	// Check vm-name annotation.
	require.Contains(t, buf.String(), "forge.unboundedkube.io/vm-name: vmss_0")
}
