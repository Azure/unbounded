// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/unbounded/internal/centerpivot"
	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// kubeletClientCertPath is kubelet's client certificate and key after TLS
// bootstrap, inside the node's root. Its subject is system:node:<Node name>.
const kubeletClientCertPath = "/var/lib/kubelet/pki/kubelet-client-current.pem"

// tridentACLAgentHost is trident-acl-agent's configuration on an Azure
// Container Linux host: a systemd drop-in and the kubeconfig it names.
type tridentACLAgentHost struct {
	log *slog.Logger
	// dir holds the kubeconfig, and the marker that the agent enabled the
	// unit; dropIn is the drop-in's path.
	dir    string
	dropIn string
	// systemctl runs systemctl and returns its output.
	systemctl func(ctx context.Context, args ...string) (string, error)
}

func realTridentACLAgentHost(log *slog.Logger) tridentACLAgentHost {
	return tridentACLAgentHost{
		log:    log,
		dir:    goalstates.TridentACLAgentDir,
		dropIn: filepath.Join(goalstates.SystemdSystemDir, goalstates.TridentACLAgentUnit+".d", "50-unbounded-centerpivot.conf"),
		systemctl: func(ctx context.Context, args ...string) (string, error) {
			return executil.OutputCmd(ctx, log, "systemctl", args...)
		},
	}
}

func (h tridentACLAgentHost) kubeconfig() string { return filepath.Join(h.dir, "kubeconfig") }

func (h tridentACLAgentHost) enabledMarker() string {
	return filepath.Join(h.dir, "enabled-by-unbounded")
}

type configureTridentACLAgent struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart

	// host and isACL are the real host's when zero.
	host  *tridentACLAgentHost
	isACL func() bool
}

// ConfigureTridentACLAgent configures trident-acl-agent, which performs Azure
// Container Linux's A/B updates, so centerpivot can update the node: it watches
// the Node's centerpivot.unbounded-cloud.io annotations with kubelet's
// credentials, and reports the version in os-release's VERSION_ID. It runs only
// on an ACL host whose MachineConfiguration labels the node managed, and undoes
// its configuration when the label goes. See designs/centerpivot.md, section 5.
func ConfigureTridentACLAgent(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &configureTridentACLAgent{log: log, goalState: goalState}
}

func (t *configureTridentACLAgent) Name() string { return "configure-trident-acl-agent" }

// Do never fails the node's start: a node centerpivot can't update still
// runs, and centerpivot holds it when its agent doesn't answer.
func (t *configureTridentACLAgent) Do(ctx context.Context) error {
	if err := t.reconcile(ctx); err != nil {
		t.log.Error("Configuring trident-acl-agent failed, so centerpivot can't update this node", "err", err)
	}

	return nil
}

func (t *configureTridentACLAgent) reconcile(ctx context.Context) error {
	isACL := t.isACL
	if isACL == nil {
		isACL = goalstates.HostIsAzureContainerLinux
	}

	if !isACL() {
		return nil
	}

	host := realTridentACLAgentHost(t.log)
	if t.host != nil {
		host = *t.host
	}

	kubelet := t.goalState.Kubelet
	if kubelet.NodeLabels[centerpivot.ManagedLabel] != "true" {
		return host.remove(ctx)
	}

	// Only TLS bootstrap leaves kubelet a client certificate the agent can use.
	if kubelet.BootstrapToken == "" || len(kubelet.KubeconfigData) > 0 || kubelet.ExecCredential != nil {
		t.log.Warn("Not configuring trident-acl-agent: centerpivot needs kubelet to join with a bootstrap token")

		return host.remove(ctx)
	}

	return host.configure(ctx, t.goalState)
}

// configure writes the drop-in and kubeconfig, and enables the agent. It
// restarts the agent only when they changed, as when the node moves to another
// slot.
func (h tridentACLAgentHost) configure(ctx context.Context, gs *goalstates.NodeStart) error {
	unit := goalstates.TridentACLAgentUnit

	if out, err := h.systemctl(ctx, "list-unit-files", "--no-legend", unit); err != nil || !strings.Contains(out, unit) {
		h.log.Warn("trident-acl-agent isn't installed, so centerpivot can't update this node", "unit", unit)

		return nil
	}

	// Record whether the agent turns the unit on, so removing the
	// configuration turns it off only then.
	if _, err := os.Stat(h.enabledMarker()); errors.Is(err, os.ErrNotExist) {
		if state, _ := h.systemctl(ctx, "is-enabled", unit); strings.TrimSpace(state) != "enabled" { //nolint:errcheck // is-enabled exits non-zero for a disabled unit.
			if err := os.MkdirAll(h.dir, 0o700); err != nil {
				return fmt.Errorf("create %s: %w", h.dir, err)
			}

			if err := os.WriteFile(h.enabledMarker(), nil, 0o600); err != nil {
				return fmt.Errorf("record that the agent enabled trident-acl-agent: %w", err)
			}
		}
	}

	cert := gs.Root().Path(kubeletClientCertPath)

	kubeconfig, err := clientcmd.Write(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"cluster": {Server: gs.Kubelet.APIServer, CertificateAuthorityData: gs.Kubelet.CACertData},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"kubelet": {ClientCertificate: cert, ClientKey: cert},
		},
		Contexts:       map[string]*clientcmdapi.Context{"default": {Cluster: "cluster", AuthInfo: "kubelet"}},
		CurrentContext: "default",
	})
	if err != nil {
		return fmt.Errorf("serialize trident-acl-agent kubeconfig: %w", err)
	}

	dropIn := strings.Join([]string{
		"# Written by the Unbounded agent: centerpivot updates this node through",
		"# its Node's annotations. See designs/centerpivot.md in Azure/unbounded.",
		"[Service]",
		"Environment=TRIDENT_ACL_AGENT_ORCHESTRATION_MODE=annotations",
		"Environment=TRIDENT_ACL_AGENT_KUBERNETES_ANNOTATION_PREFIX=" + centerpivot.Prefix,
		"Environment=TRIDENT_ACL_AGENT_KUBERNETES_KUBECONFIG=" + h.kubeconfig(),
		"Environment=TRIDENT_ACL_AGENT_KUBERNETES_NODE_NAME_SOURCE=kubelet-cert",
		"Environment=TRIDENT_ACL_AGENT_KUBERNETES_KUBELET_CERT_PATH=" + cert,
		"Environment=TRIDENT_ACL_AGENT_CURRENT_VERSION_KEY=" + centerpivot.VersionKey,
		"",
	}, "\n")

	var files changeTracker
	if err := files.write(h.kubeconfig(), kubeconfig, 0o600); err != nil {
		return fmt.Errorf("write trident-acl-agent kubeconfig: %w", err)
	}

	if err := files.write(h.dropIn, []byte(dropIn), 0o644); err != nil {
		return fmt.Errorf("write trident-acl-agent drop-in: %w", err)
	}

	if !files.changed {
		return h.run(ctx, "enable", "--now", unit)
	}

	for _, args := range [][]string{{"daemon-reload"}, {"enable", unit}, {"restart", unit}} {
		if err := h.run(ctx, args...); err != nil {
			return err
		}
	}

	h.log.Info("Configured trident-acl-agent for centerpivot", "kubeletCert", cert)

	return nil
}

// remove undoes configure, if it ran. It turns the agent off if the Unbounded
// agent turned it on, and otherwise restarts it without the configuration.
func (h tridentACLAgentHost) remove(ctx context.Context) error {
	if _, err := os.Stat(h.dropIn); errors.Is(err, os.ErrNotExist) {
		return nil
	}

	_, err := os.Stat(h.enabledMarker())
	enabledByAgent := err == nil

	for _, path := range []string{h.dropIn, h.kubeconfig(), h.enabledMarker()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	unit := goalstates.TridentACLAgentUnit

	next := []string{"try-restart", unit}
	if enabledByAgent {
		next = []string{"disable", "--now", unit}
	}

	for _, args := range [][]string{{"daemon-reload"}, next} {
		if err := h.run(ctx, args...); err != nil {
			return err
		}
	}

	h.log.Info("Removed trident-acl-agent's configuration for centerpivot", "disabled", enabledByAgent)

	return nil
}

func (h tridentACLAgentHost) run(ctx context.Context, args ...string) error {
	if _, err := h.systemctl(ctx, args...); err != nil {
		return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}

	return nil
}

type removeTridentACLAgentConfig struct {
	log *slog.Logger
}

// RemoveTridentACLAgentConfig undoes ConfigureTridentACLAgent when the node is
// reset, so trident-acl-agent doesn't keep restarting against a machine that's
// gone.
func RemoveTridentACLAgentConfig(log *slog.Logger) phases.Task {
	return &removeTridentACLAgentConfig{log: log}
}

func (t *removeTridentACLAgentConfig) Name() string { return "remove-trident-acl-agent-config" }

func (t *removeTridentACLAgentConfig) Do(ctx context.Context) error {
	if err := realTridentACLAgentHost(t.log).remove(ctx); err != nil {
		t.log.Warn("Removing trident-acl-agent's configuration failed", "err", err)
	}

	return nil
}
