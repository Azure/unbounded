// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"crypto/x509"
	"encoding/pem"
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

	machineName := r.goalState.KubeMachineName

	var machine v1alpha3.Machine
	if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err == nil {
		r.log.Info("Machine CR already exists, skipping registration",
			slog.String("machine", machineName), slog.String("machineID", string(machine.UID)))

		return nil
	} else if apimeta.IsNoMatchError(err) {
		return fmt.Errorf("machine CRD is not installed (machina not deployed?): %w", err)
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get Machine CR %q: %w", machineName, err)
	}

	// Machine CR does not exist; create a minimal one.
	r.log.Info("Machine CR not found, creating", slog.String("machine", machineName))

	machine = r.buildMachine(token)
	if err := c.Create(ctx, &machine); apierrors.IsAlreadyExists(err) {
		r.log.Info("Machine CR was created by another client", slog.String("machine", machineName))
		return nil
	} else if err != nil {
		return fmt.Errorf("create Machine CR %q: %w", machineName, err)
	}

	r.log.Info("Machine CR created", slog.String("machine", machineName), slog.String("machineID", string(machine.UID)))

	return nil
}

// buildClient creates a controller-runtime client that authenticates with
// the bootstrap token and trusts the cluster CA certificate.
func (r *registerMachine) buildClient() (client.Client, error) {
	caData := r.goalState.Kubelet.CACertData
	if len(caData) == 0 {
		return nil, fmt.Errorf("CA certificate data is empty; cannot verify API server identity")
	}

	// Verify the data contains at least one valid PEM block so we get a
	// clear error here instead of a cryptic x509 failure later.
	block, _ := pem.Decode(caData)
	if block == nil {
		return nil, fmt.Errorf("CA certificate data (%d bytes) does not contain a valid PEM block", len(caData))
	}

	// Log CA certificate identity at Info level so CI output always shows
	// what the agent trusts, even without --debug.
	if cert, err := x509.ParseCertificate(block.Bytes); err != nil {
		r.log.Warn("CA certificate PEM is valid but could not be parsed as X.509",
			slog.String("parse_error", err.Error()),
			slog.Int("ca_cert_bytes", len(caData)),
		)
	} else {
		r.log.Info("CA certificate for Machine CR registration",
			slog.String("subject", cert.Subject.String()),
			slog.String("issuer", cert.Issuer.String()),
			slog.String("not_before", cert.NotBefore.UTC().String()),
			slog.String("not_after", cert.NotAfter.UTC().String()),
			slog.Int("ca_cert_bytes", len(caData)),
			slog.String("api_server", r.goalState.Kubelet.APIServer),
		)
	}

	restCfg := &rest.Config{
		Host:        r.goalState.Kubelet.APIServer,
		BearerToken: r.goalState.Kubelet.BootstrapToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caData,
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
			Name: r.goalState.KubeMachineName,
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
