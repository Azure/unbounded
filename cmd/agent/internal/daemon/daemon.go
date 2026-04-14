// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package daemon implements the long-running agent daemon that registers the
// Machine CR and blocks until signalled to stop.
package daemon

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
)

// Config holds the values the daemon needs to build a Kubernetes client and
// register the Machine CR. It is intentionally decoupled from the goalstates
// package so the daemon does not depend on rootfs or nspawn-specific types.
type Config struct {
	// MachineName is the Kubernetes Machine CR name (e.g. "agent-e2e").
	MachineName string

	// APIServer is the HTTPS endpoint of the Kubernetes API server.
	APIServer string

	// CACertData is the PEM-encoded CA certificate of the API server.
	CACertData []byte

	// BootstrapToken is the bootstrap token in "<token-id>.<token-secret>"
	// format used for authenticating with the API server.
	//
	// TODO: using the bootstrap token as a long-lived daemon credential is
	// temporary. We will switch to a more appropriate authentication
	// mechanism (e.g. node identity certificate) in the future.
	BootstrapToken string

	// NodeLabels are key=value labels applied to the node at registration.
	NodeLabels map[string]string

	// RegisterWithTaints are taints applied to the node at registration.
	RegisterWithTaints []string
}

// newClientFunc is the constructor for the Kubernetes client. It is a package
// variable so tests can override it to inject a fake client.
var newClientFunc = defaultNewClient

// Run registers the Machine CR and then blocks until the context is cancelled.
func Run(ctx context.Context, log *slog.Logger, cfg *Config) error {
	log.Info("daemon starting", "machine_cr", cfg.MachineName)

	if err := registerMachine(ctx, log, cfg); err != nil {
		return err
	}

	// Block until the context is done.
	<-ctx.Done()

	log.Info("daemon shutting down")

	return nil
}

// registerMachine ensures a Machine CR exists for this node. When the node is
// joining via manual bootstrap, cloud-init, or another dynamic mechanism there
// may not be a pre-existing Machine CR. The function performs a get-or-create.
//
// It is a no-op when the bootstrap token is empty.
//
// When the Machine CRD is not installed in the cluster (machina is not
// deployed), the function returns a descriptive error.
func registerMachine(ctx context.Context, log *slog.Logger, cfg *Config) error {
	if cfg.BootstrapToken == "" {
		log.Info("bootstrap token not set, skipping Machine CR registration")
		return nil
	}

	c, err := buildClient(log, cfg)
	if err != nil {
		return fmt.Errorf("build Kubernetes client: %w", err)
	}

	machineName := cfg.MachineName

	var machine v1alpha3.Machine
	if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err == nil {
		log.Info("Machine CR already exists, skipping registration",
			slog.String("machine", machineName), slog.String("machineID", string(machine.UID)))
		return nil
	} else if apimeta.IsNoMatchError(err) {
		return fmt.Errorf("machine CRD is not installed (machina not deployed?): %w", err)
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get Machine CR %q: %w", machineName, err)
	}

	// Machine CR does not exist; create a minimal one.
	log.Info("Machine CR not found, creating", slog.String("machine", machineName))

	machine = buildMachine(cfg)
	if err := c.Create(ctx, &machine); apierrors.IsAlreadyExists(err) {
		log.Info("Machine CR was created by another client", slog.String("machine", machineName))
		return nil
	} else if err != nil {
		return fmt.Errorf("create Machine CR %q: %w", machineName, err)
	}

	log.Info("Machine CR created", slog.String("machine", machineName), slog.String("machineID", string(machine.UID)))

	return nil
}

// buildClient creates a controller-runtime client that authenticates with
// the bootstrap token and trusts the cluster CA certificate.
func buildClient(log *slog.Logger, cfg *Config) (client.Client, error) {
	if len(cfg.CACertData) == 0 {
		return nil, fmt.Errorf("CA certificate data is empty; cannot verify API server identity")
	}

	// Verify the data contains at least one valid PEM block so we get a
	// clear error here instead of a cryptic x509 failure later.
	block, _ := pem.Decode(cfg.CACertData)
	if block == nil {
		return nil, fmt.Errorf("CA certificate data (%d bytes) does not contain a valid PEM block", len(cfg.CACertData))
	}

	// Log CA certificate identity at Info level so CI output always shows
	// what the agent trusts, even without --debug.
	if cert, err := x509.ParseCertificate(block.Bytes); err != nil {
		log.Warn("CA certificate PEM is valid but could not be parsed as X.509",
			slog.String("parse_error", err.Error()),
			slog.Int("ca_cert_bytes", len(cfg.CACertData)),
		)
	} else {
		log.Info("CA certificate for Machine CR registration",
			slog.String("subject", cert.Subject.String()),
			slog.String("issuer", cert.Issuer.String()),
			slog.String("not_before", cert.NotBefore.UTC().String()),
			slog.String("not_after", cert.NotAfter.UTC().String()),
			slog.Int("ca_cert_bytes", len(cfg.CACertData)),
			slog.String("api_server", cfg.APIServer),
		)
	}

	restCfg := &rest.Config{
		Host:        cfg.APIServer,
		BearerToken: cfg.BootstrapToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: cfg.CACertData,
		},
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(v1alpha3.AddToScheme(scheme))

	return newClientFunc(restCfg, client.Options{Scheme: scheme})
}

// buildMachine constructs a minimal Machine CR populated from the daemon config.
func buildMachine(cfg *Config) v1alpha3.Machine {
	return v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: cfg.MachineName,
		},
		Spec: v1alpha3.MachineSpec{
			Kubernetes: &v1alpha3.KubernetesSpec{
				BootstrapTokenRef: v1alpha3.LocalObjectReference{
					Name: "bootstrap-token-" + bootstrapTokenID(cfg.BootstrapToken),
				},
				NodeLabels:         cfg.NodeLabels,
				RegisterWithTaints: cfg.RegisterWithTaints,
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
