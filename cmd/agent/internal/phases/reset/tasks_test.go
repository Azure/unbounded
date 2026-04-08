package reset

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTaskNames(t *testing.T) {
	t.Parallel()

	log := slog.Default()

	tests := []struct {
		name string
		task interface{ Name() string }
	}{
		{"stop-machine", StopMachine(log, "test")},
		{"remove-machine", RemoveMachine(log, "test")},
		{"remove-network-interfaces", RemoveNetworkInterfaces(log)},
		{"remove-wireguard-keys", RemoveWireGuardKeys(log)},
		{"remove-nspawn-config", RemoveNSpawnConfig(log, "test")},
		{"remove-nftables", RemoveNFTables(log)},
		{"remove-sysctl-config", RemoveSysctlConfig(log)},
		{"restore-docker", RestoreDocker(log)},
		{"restore-swap", RestoreSwap(log)},
		{"cleanup-routes", CleanupRoutes(log)},
		{"remove-agent-artifacts", RemoveAgentArtifacts(log)},
		{"reload-systemd", ReloadSystemd(log)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.name, tt.task.Name())
		})
	}
}

func TestWireguardTableRange(t *testing.T) {
	t.Parallel()

	// Verify the expected range matches the unbounded-net WireGuard port range.
	assert.Equal(t, 51820, wireguardTableStart)
	assert.Equal(t, 51899, wireguardTableEnd)
}
