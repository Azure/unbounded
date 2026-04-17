// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/provision"
)

// kubeClientFunc constructs a controller-runtime client from a rest.Config.
// The production implementation is client.NewWithWatch; tests can supply a fake.
type kubeClientFunc func(cfg *rest.Config, opts client.Options) (client.WithWatch, error)

// Run is the main daemon entry point. It discovers the active nspawn
// machine, builds a Kubernetes client, registers the Machine CR if needed,
// and blocks until the context is cancelled.
//
// TODO: Add a trigger mechanism (e.g. file watch, signal, API) to invoke
// updateNode when the desired config changes.
func Run(ctx context.Context, log *slog.Logger) error {
	return run(ctx, log, client.NewWithWatch)
}

// run is the inner loop, accepting a client constructor so tests can
// inject a fake.
func run(ctx context.Context, log *slog.Logger, newClient kubeClientFunc) error {
	// Find the active machine and its applied config.
	active, err := findActiveMachine(log)
	if err != nil {
		return fmt.Errorf("find active machine: %w", err)
	}

	log.Info("daemon starting",
		"machine_cr", active.Config.MachineName,
		"nspawn_machine", active.Name,
		"applied_version", active.Config.Cluster.Version,
	)

	// Build a controller-runtime client from the applied config.
	kubeClient, err := buildKubeClient(active.Config, newClient)
	if err != nil {
		return fmt.Errorf("build kube client: %w", err)
	}

	log.Info("kube client ready",
		"api_server", active.Config.Kubelet.ApiServer,
	)

	// Ensure a Machine CR exists before blocking. In dynamic environments
	// (manual-bootstrap, cloud-init) a Machine CR may not have been
	// pre-created by machina.
	if err := registerMachine(ctx, log, kubeClient, active.Config); err != nil {
		return fmt.Errorf("register machine: %w", err)
	}

	// Block until shutdown.
	<-ctx.Done()
	log.Info("daemon shutting down")

	return nil
}

// buildKubeClient creates a controller-runtime WithWatch client from the
// applied agent config. It authenticates with the bootstrap token and trusts
// the cluster CA certificate embedded in the config.
//
// This avoids reading kubeconfig files from inside the nspawn machine, which
// contain nspawn-internal paths that do not resolve on the host filesystem.
func buildKubeClient(cfg *provision.AgentConfig, newClient kubeClientFunc) (client.WithWatch, error) {
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

	if cfg.Kubelet.BootstrapToken == "" {
		return nil, fmt.Errorf("applied config has no bootstrap token")
	}

	// TODO: Bootstrap tokens are short-lived and not intended for long-running
	// daemons. We need to define a proper agent credential strategy - for
	// example, signing dedicated client certificates for the agent so it remains
	// authenticated even when the bootstrap token expires or the kubelet is
	// unavailable.
	restCfg := &rest.Config{
		Host:        cfg.Kubelet.ApiServer,
		BearerToken: cfg.Kubelet.BootstrapToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caData,
		},
	}

	s := newScheme()

	return newClient(restCfg, client.Options{Scheme: s})
}

// registerMachine ensures a Machine CR exists for this node. If the CR
// already exists, it is left untouched. Otherwise a minimal CR is created
// from the applied config. This supports dynamic environments where a
// Machine CR may not have been pre-created by machina.
func registerMachine(ctx context.Context, log *slog.Logger, c client.Client, cfg *provision.AgentConfig) error {
	machineName := cfg.MachineName
	token := cfg.Kubelet.BootstrapToken
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
	tokenID := cfg.Kubelet.BootstrapToken
	if i := strings.IndexByte(tokenID, '.'); i >= 0 {
		tokenID = tokenID[:i]
	}

	return v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: cfg.MachineName,
		},
		Spec: v1alpha3.MachineSpec{
			Kubernetes: &v1alpha3.KubernetesSpec{
				BootstrapTokenRef: v1alpha3.LocalObjectReference{
					Name: "bootstrap-token-" + tokenID,
				},
				NodeLabels:         cfg.Kubelet.Labels,
				RegisterWithTaints: cfg.Kubelet.RegisterWithTaints,
			},
		},
	}
}
