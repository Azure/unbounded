// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
)

// TestApplyMachineConfigurationTemplateSystemExtension covers the fleet-level
// default. The versioned configuration is authoritative, so a system extension
// declared there replaces whatever the machine was bootstrapped with. That is
// what lets a fleet move to a host OS with a different systemd through a
// version bump rather than an edit of every machine.
func TestApplyMachineConfigurationTemplateSystemExtension(t *testing.T) {
	t.Parallel()

	t.Run("template value is applied", func(t *testing.T) {
		t.Parallel()

		cfg := &provision.UnboundedAgentConfig{}
		applyMachineConfigurationTemplate(cfg, v1alpha3.MachineConfigurationTemplate{
			Agent: &v1alpha3.MachineConfigurationAgent{
				Image: "ghcr.io/example/rootfs:v1",
				SystemExtension: &v1alpha3.SystemExtensionSpec{
					Name:   "unbounded-nspawn",
					Source: "oci://ghcr.io/example/sysext:255-33.azl3-amd64#unbounded-nspawn.raw",
				},
			},
		})

		require.NotNil(t, cfg.SystemExtension)
		assert.Equal(t, "unbounded-nspawn", cfg.SystemExtension.Name)
		assert.Equal(t,
			"oci://ghcr.io/example/sysext:255-33.azl3-amd64#unbounded-nspawn.raw",
			cfg.SystemExtension.Source)
	})

	t.Run("template value replaces the bootstrapped value", func(t *testing.T) {
		t.Parallel()

		cfg := &provision.UnboundedAgentConfig{}
		cfg.SystemExtension = &config.AgentSystemExtension{
			Name:   "unbounded-nspawn",
			Source: "/tmp/staged-at-bootstrap.raw",
		}

		applyMachineConfigurationTemplate(cfg, v1alpha3.MachineConfigurationTemplate{
			Agent: &v1alpha3.MachineConfigurationAgent{
				Image: "ghcr.io/example/rootfs:v1",
				SystemExtension: &v1alpha3.SystemExtensionSpec{
					Name:   "unbounded-nspawn",
					Source: "oci://ghcr.io/example/sysext:255-33.azl3-amd64#unbounded-nspawn.raw",
				},
			},
		})

		require.NotNil(t, cfg.SystemExtension)
		assert.Equal(t,
			"oci://ghcr.io/example/sysext:255-33.azl3-amd64#unbounded-nspawn.raw",
			cfg.SystemExtension.Source,
			"the versioned configuration must win over the bootstrapped value")
	})

	t.Run("unset template leaves the bootstrapped value alone", func(t *testing.T) {
		t.Parallel()

		cfg := &provision.UnboundedAgentConfig{}
		cfg.SystemExtension = &config.AgentSystemExtension{
			Name:   "unbounded-nspawn",
			Source: "/tmp/staged-at-bootstrap.raw",
		}

		applyMachineConfigurationTemplate(cfg, v1alpha3.MachineConfigurationTemplate{
			Agent: &v1alpha3.MachineConfigurationAgent{Image: "ghcr.io/example/rootfs:v1"},
		})

		require.NotNil(t, cfg.SystemExtension)
		assert.Equal(t, "/tmp/staged-at-bootstrap.raw", cfg.SystemExtension.Source,
			"a configuration that does not mention the extension must not clear it")
	})

	t.Run("nil agent block is a no-op", func(t *testing.T) {
		t.Parallel()

		cfg := &provision.UnboundedAgentConfig{}
		cfg.SystemExtension = &config.AgentSystemExtension{Name: "n", Source: "/tmp/x.raw"}

		applyMachineConfigurationTemplate(cfg, v1alpha3.MachineConfigurationTemplate{})

		require.NotNil(t, cfg.SystemExtension)
		assert.Equal(t, "/tmp/x.raw", cfg.SystemExtension.Source)
	})
}
