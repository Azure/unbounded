// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	daemonControllerCertificateName = "unbounded-agent-daemon-controller"
	daemonControllerGroup           = "unbounded-agent-daemons"
	daemonControllerCertWaitTimeout = 2 * time.Minute
)

// kubeClientFunc constructs a controller-runtime client from a rest.Config.
// The production implementation is client.NewWithWatch; tests can supply a fake.
type kubeClientFunc func(cfg *rest.Config, opts client.Options) (client.WithWatch, error)

// runOptions configures daemon runtime behavior.
type runOptions struct {
	// DaemonCredentialDir stores the daemon-controller client certificate and key.
	// When empty, the default path under the agent config directory is used.
	DaemonCredentialDir string

	// NewClient constructs controller-runtime clients. Defaults to client.NewWithWatch.
	NewClient kubeClientFunc

	// NodeOperator performs host-local nspawn operations. Defaults to nspawnNodeOperator.
	NodeOperator nodeOperator
}

func (o *runOptions) validate() error {
	if o == nil {
		return fmt.Errorf("run options are required")
	}

	if o.NewClient == nil {
		o.NewClient = client.NewWithWatch
	}

	if o.NodeOperator == nil {
		o.NodeOperator = nspawnNodeOperator{}
	}

	if o.DaemonCredentialDir == "" {
		o.DaemonCredentialDir = filepath.Join(goalstates.AgentConfigDir, "daemon-controller")
	}

	return nil
}

// Run is the main daemon entry point. It discovers the active nspawn
// machine, builds a Kubernetes client, registers the Machine CR if needed,
// and blocks until the context is cancelled.
func Run(ctx context.Context, log *slog.Logger) error {
	return run(ctx, log, runOptions{})
}

// run is the inner loop. Tests can override external dependencies through opts.
func run(ctx context.Context, log *slog.Logger, opts runOptions) error {
	runOpts := &opts
	if err := runOpts.validate(); err != nil {
		return err
	}

	// Find the active machine and its applied config.
	active, err := runOpts.NodeOperator.FindActiveMachine(log)
	if err != nil {
		return fmt.Errorf("find active machine: %w", err)
	}

	log.Info("daemon starting",
		"machine_cr", active.Config.MachineName,
		"nspawn_machine", active.Name,
		"applied_version", active.Config.Cluster.Version,
	)

	if err := runOpts.NodeOperator.EnsureLifecycleMigration(ctx, log, active); err != nil {
		return fmt.Errorf("ensure nspawn lifecycle migration: %w", err)
	}

	controllerCfg, stopControllerCreds, err := daemonControllerCredentials(ctx, log, active.Config, runOpts)
	if err != nil {
		return fmt.Errorf("build daemon controller credentials: %w", err)
	}
	defer stopControllerCreds()

	kubeClient, err := runOpts.NewClient(controllerCfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return fmt.Errorf("build kube client: %w", err)
	}

	log.Info("daemon controller kube client ready",
		"api_server", active.Config.Kubelet.ApiServer,
	)

	// Ensure a Machine CR exists before blocking. In dynamic environments
	// (manual-bootstrap, cloud-init) a Machine CR may not have been
	// pre-created by machina.
	if err := registerMachine(ctx, log, kubeClient, active.Config); err != nil {
		return fmt.Errorf("register machine: %w", err)
	}

	if err := publishAndClearAgentUpgradeSignals(ctx, log, kubeClient); err != nil {
		log.Warn("failed to publish and clear AgentUpgrade daemon signals", "error", err)
	}

	return runController(ctx, log, controllerCfg, active.Config.MachineName, active.Config.NodeName, runOpts.NodeOperator)
}

func daemonControllerCredentials(
	ctx context.Context,
	log *slog.Logger,
	agentCfg *provision.AgentConfig,
	runOpts *runOptions,
) (*rest.Config, context.CancelFunc, error) {
	// Build bootstrap credentials from the applied config. These are used only
	// to obtain the daemon-controller certificate.
	bootstrapCfg, err := buildBootstrapRESTConfig(agentCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build bootstrap rest config: %w", err)
	}

	opts := daemoncred.ControllerCertificateOptions{
		Name:          daemonControllerCertificateName,
		SignerName:    daemoncred.DefaultControllerCertificateSignerName,
		DaemonGroup:   daemonControllerGroup,
		CredentialDir: runOpts.DaemonCredentialDir,
		WaitTimeout:   daemonControllerCertWaitTimeout,
	}

	provider, err := daemoncred.NewRESTConfigProvider(ctx, bootstrapCfg, agentCfg.NodeName, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("issue daemon controller certificate: %w", err)
	}

	log.Info("daemon controller certificate ready", "credentialDir", runOpts.DaemonCredentialDir)

	providerCtx, stopProvider := context.WithCancel(ctx)
	go provider.Run(providerCtx)

	return provider.RESTConfig(), stopProvider, nil
}

// buildBootstrapRESTConfig builds a Kubernetes REST config from the applied agent
// config. It authenticates with the bootstrap token and trusts the cluster CA
// certificate embedded in the config.
//
// This avoids reading kubeconfig files from inside the nspawn machine, which
// contain nspawn-internal paths that do not resolve on the host filesystem.
func buildBootstrapRESTConfig(cfg *provision.AgentConfig) (*rest.Config, error) {
	if cfg.Kubelet.ApiServer == "" {
		return nil, fmt.Errorf("applied config has no API server URL")
	}

	if cfg.Cluster.CaCertBase64 == "" {
		return nil, fmt.Errorf("applied config has no CA certificate")
	}

	caData, err := base64.StdEncoding.DecodeString(cfg.Cluster.CaCertBase64)
	if err != nil {
		return nil, fmt.Errorf("decode CA certificate: %w", err)
	}

	if cfg.Kubelet.Auth.BootstrapToken == "" {
		return nil, fmt.Errorf("applied config has no bootstrap token")
	}

	restCfg := &rest.Config{
		Host:        cfg.Kubelet.ApiServer,
		BearerToken: cfg.Kubelet.Auth.BootstrapToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caData,
		},
	}

	return restCfg, nil
}

// registerMachine ensures a Machine CR exists for this node. If the CR
// already exists, it is left untouched. Otherwise, a minimal CR is created
// from the applied config. This supports dynamic environments where a
// Machine CR may not have been pre-created by machina.
func registerMachine(ctx context.Context, log *slog.Logger, c client.Client, cfg *provision.AgentConfig) error {
	machineName := cfg.MachineName

	token := cfg.Kubelet.Auth.BootstrapToken
	if token == "" {
		log.Info("bootstrap token not set, skipping Machine CR registration")
		return nil
	}

	var machine v1alpha3.Machine
	if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err == nil {
		log.Info("Machine CR already exists, skipping registration",
			slog.String("machine", machineName),
			slog.String("machineID", string(machine.UID)),
		)

		return nil
	} else if apimeta.IsNoMatchError(err) {
		return fmt.Errorf("machine CRD is not installed (machina not deployed?): %w", err)
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get Machine CR %q: %w", machineName, err)
	}

	// Machine CR does not exist; create a minimal one.
	log.Info("Machine CR not found, creating", slog.String("machine", machineName))

	machine = buildMachineCR(cfg)
	if err := c.Create(ctx, &machine); apierrors.IsAlreadyExists(err) {
		log.Info("Machine CR was created by another client", slog.String("machine", machineName))
		return nil
	} else if err != nil {
		return fmt.Errorf("create Machine CR %q: %w", machineName, err)
	}

	log.Info("Machine CR created",
		slog.String("machine", machineName),
		slog.String("machineID", string(machine.UID)),
	)

	return nil
}

// buildMachineCR constructs a minimal Machine CR from the applied config.
func buildMachineCR(cfg *provision.AgentConfig) v1alpha3.Machine {
	tokenID := cfg.Kubelet.Auth.BootstrapToken
	if i := strings.IndexByte(tokenID, '.'); i >= 0 {
		tokenID = tokenID[:i]
	}

	machine := v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   cfg.MachineName,
			Labels: machineSiteLabels(cfg.Kubelet.Labels),
		},
		Spec: v1alpha3.MachineSpec{
			Kubernetes: &v1alpha3.KubernetesSpec{
				BootstrapTokenRef: &v1alpha3.LocalObjectReference{
					Name: "bootstrap-token-" + tokenID,
				},
				NodeLabels:         cfg.Kubelet.Labels,
				RegisterWithTaints: cfg.Kubelet.RegisterWithTaints,
			},
		},
	}

	return machine
}

func machineSiteLabels(labels map[string]string) map[string]string {
	for _, key := range []string{v1alpha3.MachineSiteLabelKey, netv1alpha1.SiteLabelKey} {
		if value := strings.TrimSpace(labels[key]); value != "" {
			return map[string]string{v1alpha3.MachineSiteLabelKey: value}
		}
	}

	return nil
}
