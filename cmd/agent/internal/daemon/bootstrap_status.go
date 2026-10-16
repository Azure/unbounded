// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

const (
	bootstrapConditionMessageMaxLen = 1024
	bootstrapStatusCertWaitTimeout  = 10 * time.Second
)

// BootstrapStatusReporter publishes the agent's initial bootstrap progress to
// the Machine status. It is best-effort so bootstrap can continue when the
// cluster is temporarily unavailable or RBAC has not been updated yet.
type BootstrapStatusReporter struct {
	log      *slog.Logger
	client   client.Client
	machine  string
	ready    bool
	stop     context.CancelFunc
	stopOnce sync.Once
}

type bootstrapStatusCredentialsFunc func(
	context.Context,
	*slog.Logger,
	*provision.AgentConfig,
	*runOptions,
) (*rest.Config, context.CancelFunc, error)

// NewBootstrapStatusReporter builds a best-effort Machine status reporter using
// the Machine-bound daemon certificate obtained from the bootstrap credentials.
func NewBootstrapStatusReporter(ctx context.Context, log *slog.Logger, cfg *provision.AgentConfig) *BootstrapStatusReporter {
	return newBootstrapStatusReporter(ctx, log, cfg, &runOptions{}, bootstrapStatusCredentials)
}

func newBootstrapStatusReporter(
	ctx context.Context,
	log *slog.Logger,
	cfg *provision.AgentConfig,
	runOpts *runOptions,
	credentials bootstrapStatusCredentialsFunc,
) *BootstrapStatusReporter {
	reporter := &BootstrapStatusReporter{
		log:     log,
		machine: cfg.MachineName,
	}

	if err := runOpts.validate(); err != nil {
		log.Warn("failed to configure bootstrap status credentials", "error", err)
		return reporter
	}

	controllerCfg, stopControllerCreds, err := credentials(ctx, log, cfg, runOpts)
	if err != nil {
		log.Warn("failed to issue bootstrap status credentials", "error", err)
		return reporter
	}

	kubeClient, err := runOpts.NewClient(controllerCfg, client.Options{Scheme: newScheme()})
	if err != nil {
		stopControllerCreds()
		log.Warn("failed to build bootstrap status kube client", "error", err)

		return reporter
	}

	if err := registerMachine(ctx, log, kubeClient, cfg); err != nil {
		stopControllerCreds()
		log.Warn("failed to register Machine for bootstrap status", "machine", cfg.MachineName, "error", err)

		return reporter
	}

	reporter.client = kubeClient
	reporter.ready = true
	reporter.stop = stopControllerCreds

	return reporter
}

func bootstrapStatusCredentials(
	ctx context.Context,
	log *slog.Logger,
	cfg *provision.AgentConfig,
	runOpts *runOptions,
) (*rest.Config, context.CancelFunc, error) {
	bootstrapCfg, err := buildBootstrapRESTConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build bootstrap rest config: %w", err)
	}

	opts := daemoncred.ControllerCertificateOptions{
		Name:          daemonControllerCertificateName,
		SignerName:    daemoncred.DefaultControllerCertificateSignerName,
		DaemonGroup:   daemonControllerGroup,
		CredentialDir: runOpts.DaemonCredentialDir,
		WaitTimeout:   bootstrapStatusCertWaitTimeout,
	}

	provider, err := daemoncred.NewRESTConfigProvider(ctx, bootstrapCfg, cfg.NodeName, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("issue daemon controller certificate: %w", err)
	}

	providerCtx, stopProvider := context.WithCancel(ctx)
	go provider.Run(providerCtx)

	log.Info("bootstrap status daemon certificate ready", "credentialDir", runOpts.DaemonCredentialDir)

	return provider.RESTConfig(), stopProvider, nil
}

// Running reports that initial bootstrap is in progress.
func (r *BootstrapStatusReporter) Running(ctx context.Context) {
	r.set(ctx, metav1.ConditionFalse, "Running", "unbounded-agent bootstrap is running")
}

// Failed reports that initial bootstrap failed.
func (r *BootstrapStatusReporter) Failed(ctx context.Context, reason string, err error) {
	if err == nil {
		err = fmt.Errorf("bootstrap failed")
	}

	r.set(ctx, metav1.ConditionFalse, reason, truncateBootstrapConditionMessage(err.Error()))
	r.close()
}

// Succeeded reports that initial bootstrap completed successfully.
func (r *BootstrapStatusReporter) Succeeded(ctx context.Context) {
	r.set(ctx, metav1.ConditionTrue, "Succeeded", "unbounded-agent bootstrap completed successfully")
	r.close()
}

func (r *BootstrapStatusReporter) close() {
	if r == nil || r.stop == nil {
		return
	}

	r.stopOnce.Do(r.stop)
}

func (r *BootstrapStatusReporter) set(ctx context.Context, status metav1.ConditionStatus, reason, message string) {
	if r == nil || !r.ready {
		return
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var machine v1alpha3.Machine
		if err := r.client.Get(ctx, client.ObjectKey{Name: r.machine}, &machine); err != nil {
			return fmt.Errorf("get Machine %q: %w", r.machine, err)
		}

		apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:               v1alpha3.MachineConditionAgentBootstrapped,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: machine.Generation,
		})

		return r.client.Status().Update(ctx, &machine)
	})
	if err != nil {
		r.log.Warn("failed to update AgentBootstrapped condition", "machine", r.machine, "reason", reason, "error", err)
	}
}

func truncateBootstrapConditionMessage(message string) string {
	if len(message) <= bootstrapConditionMessageMaxLen {
		return message
	}

	return message[:bootstrapConditionMessageMaxLen-3] + "..."
}
