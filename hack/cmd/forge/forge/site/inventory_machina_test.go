// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package site

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/hack/cmd/forge/forge/site/azuredev"
)

func TestMachinaNameFromInventory(t *testing.T) {
	t.Parallel()

	m := azuredev.Machine{
		Name:      "vmss_worker_0",
		IPAddress: "20.48.100.5",
		Port:      50001,
	}

	require.Equal(t, "dc1-20.48.100.5-50001", machinaNameFromInventory("dc1", m))
}

func TestMachineHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ip       string
		port     int
		expected string
	}{
		{name: "default port omitted", ip: "10.1.0.4", port: 22, expected: "10.1.0.4"},
		{name: "zero port omitted", ip: "10.1.0.4", port: 0, expected: "10.1.0.4"},
		{name: "non-default port included", ip: "20.48.100.5", port: 50001, expected: "20.48.100.5:50001"},
		{name: "custom port included", ip: "1.2.3.4", port: 2222, expected: "1.2.3.4:2222"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, machineHost(tt.ip, tt.port))
		})
	}
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

	err := WriteInventoryAsMachina(&buf, inv, MachinaInventoryOptions{Site: "mysite", SSHUsername: "kubedev"})
	require.NoError(t, err)

	output := buf.String()

	// Should contain the document separator between the two machines.
	require.Equal(t, 1, strings.Count(output, "---\n"))

	// Spot-check apiVersion, kind, names, host.
	require.Contains(t, output, "apiVersion: unbounded-cloud.io/v1alpha3")
	require.Contains(t, output, "kind: Machine")
	require.Contains(t, output, "name: mysite-20.48.100.5-50001")
	require.Contains(t, output, "name: mysite-20.48.100.5-50002")
	require.Contains(t, output, "host: 20.48.100.5:50001")
	require.Contains(t, output, "host: 20.48.100.5:50002")
	require.Contains(t, output, "username: kubedev")
}

func TestWriteInventoryAsMachina_Empty(t *testing.T) {
	t.Parallel()

	inv := &azuredev.Inventory{}

	var buf bytes.Buffer

	err := WriteInventoryAsMachina(&buf, inv, MachinaInventoryOptions{})
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

	err := WriteInventoryAsMachina(&buf, inv, MachinaInventoryOptions{Site: "mysite", SSHUsername: "kubedev"})
	require.NoError(t, err)

	// Single document should have no separator.
	require.NotContains(t, buf.String(), "---")
}

func TestWriteInventoryAsMachina_WithBastion(t *testing.T) {
	t.Parallel()

	inv := &azuredev.Inventory{
		Machines: []azuredev.Machine{
			{Name: "vmss_0", IPAddress: "10.1.0.4", Port: 22},
			{Name: "vmss_1", IPAddress: "10.1.0.5", Port: 22},
		},
		Bastion: &azuredev.Machine{
			Name:      "bastion_0",
			IPAddress: "20.48.100.5",
			Port:      22,
		},
	}

	var buf bytes.Buffer

	err := WriteInventoryAsMachina(&buf, inv, MachinaInventoryOptions{Site: "dc1", BastionHost: "20.48.100.5", SSHUsername: "kubedev", BastionSSHUsername: "kubedev"})
	require.NoError(t, err)

	output := buf.String()

	// Should contain the document separator between the two machines.
	require.Equal(t, 1, strings.Count(output, "---\n"))

	// Workers should use private IPs without port (22 is the default).
	require.Contains(t, output, "host: 10.1.0.4\n")
	require.Contains(t, output, "host: 10.1.0.5\n")

	// Each worker should have bastion configuration (also without port).
	require.Contains(t, output, "bastion:")
	require.Contains(t, output, "host: 20.48.100.5\n")

	// Bastion machine itself should NOT appear as a worker.
	require.NotContains(t, output, "bastion_0")
}

func TestWriteInventoryAsSSH_WithBastion(t *testing.T) {
	t.Parallel()

	inv := &azuredev.Inventory{
		Machines: []azuredev.Machine{
			{Name: "mysite-worker_0", IPAddress: "10.1.0.4", Port: 22},
			{Name: "mysite-worker_1", IPAddress: "10.1.0.5", Port: 22},
		},
		Bastion: &azuredev.Machine{
			Name:      "mysite-bastion_0",
			IPAddress: "20.48.100.5",
			Port:      22,
		},
	}

	var buf bytes.Buffer

	err := WriteInventoryAsSSH(&buf, inv, "mysite", "kubedev", "/home/user/.ssh/id_ed25519")
	require.NoError(t, err)

	output := buf.String()

	// Bastion entry.
	require.Contains(t, output, "Host bastion-mysite")
	require.Contains(t, output, "HostName 20.48.100.5")

	// Worker entries with ProxyJump.
	require.Contains(t, output, "Host mysite-worker_0")
	require.Contains(t, output, "HostName 10.1.0.4")
	require.Contains(t, output, "Host mysite-worker_1")
	require.Contains(t, output, "HostName 10.1.0.5")
	require.Contains(t, output, "ProxyJump bastion-mysite")

	// Common fields.
	require.Contains(t, output, "User kubedev")
	require.Contains(t, output, "IdentityFile /home/user/.ssh/id_ed25519")
	require.Contains(t, output, "StrictHostKeyChecking no")
	require.Contains(t, output, "UserKnownHostsFile /dev/null")
}

func TestWriteInventoryAsSSH_Direct(t *testing.T) {
	t.Parallel()

	inv := &azuredev.Inventory{
		Machines: []azuredev.Machine{
			{Name: "mysite-worker_0", IPAddress: "20.48.100.5", Port: 50001},
			{Name: "mysite-worker_1", IPAddress: "20.48.100.5", Port: 50002},
		},
	}

	var buf bytes.Buffer

	err := WriteInventoryAsSSH(&buf, inv, "mysite", "kubedev", "/home/user/.ssh/id_ed25519")
	require.NoError(t, err)

	output := buf.String()

	// Direct entries with port.
	require.Contains(t, output, "Host mysite-worker_0")
	require.Contains(t, output, "HostName 20.48.100.5")
	require.Contains(t, output, "Port 50001")
	require.Contains(t, output, "Port 50002")

	// No ProxyJump in direct mode.
	require.NotContains(t, output, "ProxyJump")
}

func TestWriteInventoryAsSSH_Empty(t *testing.T) {
	t.Parallel()

	inv := &azuredev.Inventory{}

	var buf bytes.Buffer

	err := WriteInventoryAsSSH(&buf, inv, "mysite", "kubedev", "/home/user/.ssh/key")
	require.NoError(t, err)
	require.Empty(t, buf.String())
}

func TestParseSecretKeyRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		expected  machinav1alpha3.SecretKeySelector
		expectErr bool
	}{
		{
			name:  "full format",
			input: "unbounded-kube/machina-ssh:ssh-private-key",
			expected: machinav1alpha3.SecretKeySelector{
				Namespace: "unbounded-kube",
				Name:      "machina-ssh",
				Key:       "ssh-private-key",
			},
		},
		{
			name:  "no namespace defaults to unbounded-kube",
			input: "machina-ssh:ssh-private-key",
			expected: machinav1alpha3.SecretKeySelector{
				Namespace: "unbounded-kube",
				Name:      "machina-ssh",
				Key:       "ssh-private-key",
			},
		},
		{
			name:  "no key omits key field",
			input: "unbounded-kube/machina-ssh",
			expected: machinav1alpha3.SecretKeySelector{
				Namespace: "unbounded-kube",
				Name:      "machina-ssh",
			},
		},
		{
			name:  "bare name only",
			input: "machina-ssh",
			expected: machinav1alpha3.SecretKeySelector{
				Namespace: "unbounded-kube",
				Name:      "machina-ssh",
			},
		},
		{
			name:  "custom namespace no key",
			input: "my-ns/my-secret",
			expected: machinav1alpha3.SecretKeySelector{
				Namespace: "my-ns",
				Name:      "my-secret",
			},
		},
		{
			name:      "empty string errors",
			input:     "",
			expectErr: true,
		},
		{
			name:      "namespace slash but no name errors",
			input:     "unbounded-kube/",
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ref, err := parseSecretKeyRef(tt.input)
			if tt.expectErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expected, ref)
		})
	}
}

func TestWriteInventoryAsMachina_WithSSHSecretRef(t *testing.T) {
	t.Parallel()

	inv := &azuredev.Inventory{
		Machines: []azuredev.Machine{
			{Name: "vmss_0", IPAddress: "10.0.0.1", Port: 22},
		},
	}

	ref := machinav1alpha3.SecretKeySelector{
		Namespace: "unbounded-kube",
		Name:      "machina-ssh",
		Key:       "ssh-private-key",
	}

	var buf bytes.Buffer

	err := WriteInventoryAsMachina(&buf, inv, MachinaInventoryOptions{
		Site:         "mysite",
		SSHSecretRef: &ref,
		SSHUsername:  "kubedev",
	})
	require.NoError(t, err)

	output := buf.String()
	require.Contains(t, output, "privateKeyRef:")
	require.Contains(t, output, "name: machina-ssh")
	require.Contains(t, output, "namespace: unbounded-kube")
	require.Contains(t, output, "key: ssh-private-key")
}

func TestWriteInventoryAsMachina_WithBastionSecretRef(t *testing.T) {
	t.Parallel()

	inv := &azuredev.Inventory{
		Machines: []azuredev.Machine{
			{Name: "vmss_0", IPAddress: "10.1.0.4", Port: 22},
		},
		Bastion: &azuredev.Machine{
			Name:      "bastion_0",
			IPAddress: "20.48.100.5",
			Port:      22,
		},
	}

	sshRef := machinav1alpha3.SecretKeySelector{
		Namespace: "unbounded-kube",
		Name:      "machina-ssh",
		Key:       "ssh-private-key",
	}

	bastionRef := machinav1alpha3.SecretKeySelector{
		Namespace: "unbounded-kube",
		Name:      "bastion-ssh",
		Key:       "bastion-key",
	}

	var buf bytes.Buffer

	err := WriteInventoryAsMachina(&buf, inv, MachinaInventoryOptions{
		Site:                "dc1",
		BastionHost:         "20.48.100.5",
		SSHSecretRef:        &sshRef,
		BastionSSHSecretRef: &bastionRef,
		SSHUsername:         "kubedev",
		BastionSSHUsername:  "kubedev",
	})
	require.NoError(t, err)

	output := buf.String()

	// Worker SSH privateKeyRef.
	require.Contains(t, output, "name: machina-ssh")

	// Bastion section with its own privateKeyRef.
	require.Contains(t, output, "bastion:")
	require.Contains(t, output, "host: 20.48.100.5")
	require.Contains(t, output, "name: bastion-ssh")
	require.Contains(t, output, "key: bastion-key")
}
