// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostagent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/unbounded/cmd/centerpivot/internal/names"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// fakeSystemctl records calls, and answers list-unit-files and is-enabled.
type fakeSystemctl struct {
	installed, enabled bool
	calls              []string
}

func (f *fakeSystemctl) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))

	switch args[0] {
	case "list-unit-files":
		if f.installed {
			return unit + " disabled enabled\n", nil
		}

		return "", errors.New("exit status 1")
	case "is-enabled":
		if f.enabled {
			return "enabled\n", nil
		}

		return "disabled\n", errors.New("exit status 1")
	default:
		return "", nil
	}
}

// managedNode is a node in slot whose MachineConfiguration labels it managed,
// and whose kubelet joins with a bootstrap token.
func managedNode(slot string) *goalstates.NodeStart {
	return &goalstates.NodeStart{
		MachineName: slot,
		MachineDir:  "/var/lib/machines/" + slot,
		Kubelet: goalstates.Kubelet{
			APIServer:      "https://api.example.com:6443",
			CACertData:     []byte("-----BEGIN CERTIFICATE-----\n"),
			BootstrapToken: "abcdef.0123456789abcdef",
			NodeLabels:     map[string]string{names.ManagedLabel: "true"},
		},
	}
}

// aclHost returns a configure task for gs on a fake ACL host under a
// temporary directory.
func aclHost(t *testing.T, systemctl *fakeSystemctl) (func(*goalstates.NodeStart) error, tridentACLAgentHost) {
	t.Helper()

	root := t.TempDir()
	host := tridentACLAgentHost{
		log:       slog.New(slog.DiscardHandler),
		dir:       filepath.Join(root, "etc/unbounded/trident-acl-agent"),
		dropIn:    filepath.Join(root, "etc/systemd/system/trident-acl-agent.service.d/50-unbounded-centerpivot.conf"),
		systemctl: systemctl.run,
	}

	configure := func(gs *goalstates.NodeStart) error {
		task := &configureTridentACLAgent{log: host.log, goalState: gs, host: &host, isACL: func() bool { return true }}

		return task.reconcile(t.Context())
	}

	return configure, host
}

func TestConfigureTridentACLAgent(t *testing.T) {
	t.Parallel()

	systemctl := &fakeSystemctl{installed: true}
	configure, host := aclHost(t, systemctl)

	require.NoError(t, configure(managedNode("kube1")))

	cert := "/var/lib/kubelet/pki/kubelet-client-current.pem"

	dropIn, err := os.ReadFile(host.dropIn)
	require.NoError(t, err)

	for _, line := range []string{
		"Environment=TRIDENT_ACL_AGENT_ORCHESTRATION_MODE=annotations",
		"Environment=TRIDENT_ACL_AGENT_KUBERNETES_ANNOTATION_PREFIX=centerpivot.unbounded-cloud.io",
		"Environment=TRIDENT_ACL_AGENT_KUBERNETES_KUBECONFIG=" + host.kubeconfig(),
		"Environment=TRIDENT_ACL_AGENT_KUBERNETES_NODE_NAME_SOURCE=kubelet-cert",
		"Environment=TRIDENT_ACL_AGENT_KUBERNETES_KUBELET_CERT_PATH=" + cert,
		"Environment=TRIDENT_ACL_AGENT_CURRENT_VERSION_KEY=VERSION_ID",
		"Environment=TRIDENT_ACL_AGENT_CURRENT_VERSION_FALLBACK=error",
		"Environment=TRIDENT_ACL_AGENT_ORCHESTRATION_STAGE_TIMEOUT=20m",
		"Environment=TRIDENT_ACL_AGENT_ORCHESTRATION_FINALIZE_TIMEOUT=10m",
		"Environment=TRIDENT_ACL_AGENT_ORCHESTRATION_COOKIE_ENABLED=true",
		// kubelet's certificate is an absolute symlink inside the machine.
		"BindReadOnlyPaths=/var/lib/machines/kube1/var/lib/kubelet/pki:/var/lib/kubelet/pki",
	} {
		assert.Contains(t, string(dropIn), line+"\n")
	}

	kubeconfig, err := clientcmd.LoadFromFile(host.kubeconfig())
	require.NoError(t, err)

	cluster, user := kubeconfig.Clusters[kubeconfig.Contexts[kubeconfig.CurrentContext].Cluster], kubeconfig.AuthInfos[kubeconfig.Contexts[kubeconfig.CurrentContext].AuthInfo]
	assert.Equal(t, "https://api.example.com:6443", cluster.Server)
	assert.Equal(t, []byte("-----BEGIN CERTIFICATE-----\n"), cluster.CertificateAuthorityData)
	assert.Equal(t, cert, user.ClientCertificate, "kubelet's own credentials, from the machine's directory the drop-in binds")
	assert.Equal(t, cert, user.ClientKey)

	info, err := os.Stat(host.kubeconfig())
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	assert.Equal(t, []string{
		"list-unit-files --no-legend trident-acl-agent.service",
		"is-enabled trident-acl-agent.service",
		"daemon-reload",
		"enable trident-acl-agent.service",
		"restart trident-acl-agent.service",
	}, systemctl.calls)

	// Starting again changes nothing, so nothing restarts.
	systemctl.calls = nil

	require.NoError(t, configure(managedNode("kube1")))
	assert.Equal(t, []string{
		"list-unit-files --no-legend trident-acl-agent.service",
		"enable --now trident-acl-agent.service",
	}, systemctl.calls)

	// A new slot moves kubelet's certificate, so the agent restarts once.
	systemctl.calls = nil

	require.NoError(t, configure(managedNode("kube2")))
	assert.Contains(t, systemctl.calls, "restart trident-acl-agent.service")

	dropIn, err = os.ReadFile(host.dropIn)
	require.NoError(t, err)
	assert.Contains(t, string(dropIn), "BindReadOnlyPaths=/var/lib/machines/kube2/var/lib/kubelet/pki:/var/lib/kubelet/pki\n")
}

func TestConfigureTridentACLAgentLeavesOtherNodesAlone(t *testing.T) {
	t.Parallel()

	unmanaged := managedNode("kube1")
	unmanaged.Kubelet.NodeLabels = nil

	directKubeconfig := managedNode("kube1")
	directKubeconfig.Kubelet.BootstrapToken = ""
	directKubeconfig.Kubelet.KubeconfigData = []byte("apiVersion: v1\n")

	for name, tt := range map[string]struct {
		gs        *goalstates.NodeStart
		installed bool
		notACL    bool
	}{
		"not Azure Container Linux":         {gs: managedNode("kube1"), installed: true, notACL: true},
		"not managed":                       {gs: unmanaged, installed: true},
		"a kubelet without TLS bootstrap":   {gs: directKubeconfig, installed: true},
		"trident-acl-agent isn't installed": {gs: managedNode("kube1")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			systemctl := &fakeSystemctl{installed: tt.installed}
			_, host := aclHost(t, systemctl)
			task := &configureTridentACLAgent{log: host.log, goalState: tt.gs, host: &host, isACL: func() bool { return !tt.notACL }}

			require.NoError(t, task.Do(t.Context()))
			assert.NoFileExists(t, host.dropIn)
			assert.NoFileExists(t, host.kubeconfig())
			assert.NotContains(t, systemctl.calls, "daemon-reload")
		})
	}
}

func TestUnmanagingANodeUndoesTheConfiguration(t *testing.T) {
	t.Parallel()

	for name, tt := range map[string]struct {
		enabledByImage bool
		want           string
	}{
		"the agent turned it on, so it turns it off":  {want: "disable --now trident-acl-agent.service"},
		"the image turned it on, so it keeps running": {enabledByImage: true, want: "try-restart trident-acl-agent.service"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			systemctl := &fakeSystemctl{installed: true, enabled: tt.enabledByImage}
			configure, host := aclHost(t, systemctl)
			require.NoError(t, configure(managedNode("kube1")))

			unmanaged := managedNode("kube1")
			delete(unmanaged.Kubelet.NodeLabels, names.ManagedLabel)

			systemctl.calls = nil

			require.NoError(t, configure(unmanaged))

			assert.NoFileExists(t, host.dropIn)
			assert.NoFileExists(t, host.kubeconfig())
			assert.NoFileExists(t, host.enabledMarker())
			assert.Equal(t, []string{"daemon-reload", tt.want}, systemctl.calls)

			systemctl.calls = nil

			require.NoError(t, configure(unmanaged))
			assert.Empty(t, systemctl.calls, "nothing left to undo")
		})
	}
}

func TestHostMountsBindTheMachineIDOnAzureContainerLinux(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []config.AdditionalHostMount{{
		Source: "/etc/machine-id", Target: "/run/unbounded/host/machine-id", ReadOnly: true,
	}}, hostMounts(true))
	assert.Empty(t, hostMounts(false))
}
