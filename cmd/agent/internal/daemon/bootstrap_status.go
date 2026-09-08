// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
)

const bootstrapConditionMessageMaxLen = 1024

// BootstrapStatusReporter publishes the agent's initial bootstrap progress to
// the Machine status. It is best-effort so bootstrap can continue when the
// cluster is temporarily unavailable or RBAC has not been updated yet.
type BootstrapStatusReporter struct {
	log     *slog.Logger
	client  client.Client
	machine string
	ready   bool
}

// NewBootstrapStatusReporter builds a best-effort Machine status reporter from
// bootstrap credentials in cfg. When credentials are incomplete, the returned
// reporter logs and skips updates.
func NewBootstrapStatusReporter(ctx context.Context, log *slog.Logger, cfg *provision.AgentConfig) *BootstrapStatusReporter {
	reporter := &BootstrapStatusReporter{
		log:     log,
		machine: cfg.MachineName,
	}

	if strings.TrimSpace(cfg.Kubelet.Auth.BootstrapToken) == "" {
		log.Info("bootstrap token not set, skipping AgentBootstrapped status reporting")
		return reporter
	}

	restCfg, err := buildBootstrapRESTConfig(cfg)
	if err != nil {
		log.Warn("failed to build bootstrap status kube config", "error", err)
		return reporter
	}

	kubeClient, err := client.New(rest.CopyConfig(restCfg), client.Options{Scheme: newScheme()})
	if err != nil {
		log.Warn("failed to build bootstrap status kube client", "error", err)
		return reporter
	}

	if err := registerMachine(ctx, log, kubeClient, cfg); err != nil {
		log.Warn("failed to register Machine for bootstrap status", "machine", cfg.MachineName, "error", err)
		return reporter
	}

	reporter.client = kubeClient
	reporter.ready = true

	return reporter
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
}

// Succeeded reports that initial bootstrap completed successfully.
func (r *BootstrapStatusReporter) Succeeded(ctx context.Context) {
	r.set(ctx, metav1.ConditionTrue, "Succeeded", "unbounded-agent bootstrap completed successfully")
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
