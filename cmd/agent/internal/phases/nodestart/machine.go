// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
)

type registerMachine struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart

	// newClient is the constructor for the Kubernetes client. It is
	// overridden in tests to inject a fake client.
	newClient func(cfg *rest.Config, opts client.Options) (client.Client, error)
}

// RegisterMachine returns a task that ensures a Machine CR exists for this
// node before kubelet is started. When the node is joining via manual
// bootstrap, cloud-init, or another dynamic mechanism there may not be a
// pre-existing Machine CR. The task performs a get; if the object is absent
// it creates a minimal Machine CR populated from the agent goal state.
//
// The task is a no-op when the bootstrap token is empty. It must run after
// ApplyAttestation so the bootstrap token is fully resolved.
//
// When the Machine CRD is not installed in the cluster (machina is not
// deployed), the task logs a warning and succeeds without creating anything.
// This allows the agent to run in environments where machina is not present.
func RegisterMachine(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &registerMachine{
		log:       log,
		goalState: goalState,
		newClient: defaultNewClient,
	}
}

func (r *registerMachine) Name() string { return "register-machine" }

func (r *registerMachine) Do(ctx context.Context) error {
	token := r.goalState.Kubelet.BootstrapToken
	if token == "" {
		r.log.Info("bootstrap token not set, skipping Machine CR registration")
		return nil
	}

	c, err := r.buildClient()
	if err != nil {
		return fmt.Errorf("build Kubernetes client: %w", err)
	}

	machineName := r.goalState.MachineName

	var existing v1alpha3.Machine
	if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &existing); err == nil {
		r.log.Info("Machine CR already exists, skipping registration",
			slog.String("machine", machineName))

		return nil
	} else if apimeta.IsNoMatchError(err) {
		// The Machine CRD is not installed; machina is not deployed in this
		// cluster. Log a warning and continue without failing.
		r.log.Warn("Machine API not available (machina not installed?), skipping Machine CR registration",
			slog.String("machine", machineName))

		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get Machine CR %q: %w", machineName, err)
	}

	// Machine CR does not exist; create a minimal one.
	r.log.Info("Machine CR not found, creating", slog.String("machine", machineName))

	machine := r.buildMachine(token)
	if err := c.Create(ctx, &machine); err != nil {
		return fmt.Errorf("create Machine CR %q: %w", machineName, err)
	}

	r.log.Info("Machine CR created", slog.String("machine", machineName))

	return nil
}

// buildClient creates a controller-runtime client that authenticates with
// the bootstrap token and trusts the cluster CA certificate.
func (r *registerMachine) buildClient() (client.Client, error) {
	restCfg := &rest.Config{
		Host:        r.goalState.Kubelet.APIServer,
		BearerToken: r.goalState.Kubelet.BootstrapToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: r.goalState.Kubelet.CACertData,
		},
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(v1alpha3.AddToScheme(scheme))

	return r.newClient(restCfg, client.Options{Scheme: scheme})
}

// buildMachine constructs a minimal Machine CR populated from the goal state.
func (r *registerMachine) buildMachine(token string) v1alpha3.Machine {
	return v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.goalState.MachineName,
		},
		Spec: v1alpha3.MachineSpec{
			Kubernetes: &v1alpha3.KubernetesSpec{
				BootstrapTokenRef: v1alpha3.LocalObjectReference{
					Name: "bootstrap-token-" + bootstrapTokenID(token),
				},
				NodeLabels:         r.goalState.Kubelet.NodeLabels,
				RegisterWithTaints: r.goalState.Kubelet.RegisterWithTaints,
			},
		},
	}
}

// bootstrapTokenID returns the token-id portion of a bootstrap token that
// follows the "<token-id>.<token-secret>" format. If the token contains no
// dot, the entire token string is returned unchanged.
func bootstrapTokenID(token string) string {
	if i := strings.IndexByte(token, '.'); i >= 0 {
		return token[:i]
	}

	return token
}

// defaultNewClient is the production client constructor.
func defaultNewClient(cfg *rest.Config, opts client.Options) (client.Client, error) {
	return client.New(cfg, opts)
}
