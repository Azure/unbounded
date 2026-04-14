// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/nodeupdate"
	"github.com/Azure/unbounded-kube/internal/provision"
)

// newKubeClient is the constructor for the Kubernetes watch client. It is
// overridden in tests to inject a fake client.
var newKubeClient = defaultNewKubeClient

const (
	// watchRetryInterval is the delay between watch re-establishment attempts.
	watchRetryInterval = 10 * time.Second
)

func newCmdDaemon(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Watch the Machine CR and reconcile the node to the desired state",
		Long: "Long-running daemon that watches the Machine custom resource on the " +
			"control plane and performs node updates when the desired state diverges " +
			"from the locally applied configuration.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()
			log := cmdCtx.Logger

			return runDaemon(ctx, log)
		},
	}

	return cmd
}

// runDaemon is the main daemon loop. It discovers the active nspawn machine,
// builds a Kubernetes client from the applied config, and watches the
// Machine CR for changes.
func runDaemon(ctx context.Context, log *slog.Logger) error {
	// Find the active machine and its applied config.
	active, err := nodeupdate.FindActiveMachine()
	if err != nil {
		return fmt.Errorf("find active machine: %w", err)
	}

	machineName := active.Config.MachineName
	log.Info("daemon starting",
		"machine_cr", machineName,
		"nspawn_machine", active.Name,
		"applied_version", active.Config.Cluster.Version,
	)

	// Build a controller-runtime WithWatch client from the applied config.
	kubeClient, err := buildKubeClient(active.Config)
	if err != nil {
		return fmt.Errorf("build kube client: %w", err)
	}

	log.Info("kube client ready",
		"api_server", active.Config.Kubelet.ApiServer,
	)

	// Enter the watch loop.
	for {
		if err := watchMachine(ctx, log, kubeClient, machineName); err != nil {
			if ctx.Err() != nil {
				return nil // Graceful shutdown.
			}

			log.Error("watch failed, retrying", "error", err, "retry_in", watchRetryInterval)

			select {
			case <-ctx.Done():
				return nil
			case <-time.After(watchRetryInterval):
			}
		}
	}
}

// buildKubeClient creates a controller-runtime WithWatch client from the
// applied agent config. It authenticates with the bootstrap token and trusts
// the cluster CA certificate embedded in the config - the same approach used
// by the RegisterMachine phase during initial provisioning.
//
// This avoids reading kubeconfig files from inside the nspawn machine, which
// contain nspawn-internal paths that do not resolve on the host filesystem.
func buildKubeClient(cfg *provision.AgentConfig) (client.WithWatch, error) {
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

	restCfg := &rest.Config{
		Host:        cfg.Kubelet.ApiServer,
		BearerToken: cfg.Kubelet.BootstrapToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caData,
		},
	}

	s := newScheme()

	return newKubeClient(restCfg, client.Options{Scheme: s})
}

// defaultNewKubeClient is the production constructor for the Kubernetes
// WithWatch client.
func defaultNewKubeClient(cfg *rest.Config, opts client.Options) (client.WithWatch, error) {
	return client.NewWithWatch(cfg, opts)
}

// watchMachine establishes a watch on the named Machine CR and handles events
// until the watch closes or the context is cancelled.
func watchMachine(ctx context.Context, log *slog.Logger, wc client.WithWatch, machineName string) error {
	machineList := &v1alpha3.MachineList{}
	watcher, err := wc.Watch(ctx, machineList, client.MatchingFields{"metadata.name": machineName})
	if err != nil {
		return fmt.Errorf("start watch for Machine %q: %w", machineName, err)
	}
	defer watcher.Stop()

	log.Info("watching Machine CR", "name", machineName)

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}

			if event.Type == watch.Error {
				return fmt.Errorf("watch error event: %v", event.Object)
			}

			if event.Type != watch.Modified && event.Type != watch.Added {
				continue
			}

			machine, ok := event.Object.(*v1alpha3.Machine)
			if !ok {
				log.Warn("unexpected object type in watch event")
				continue
			}

			log.Debug("watch event",
				"type", event.Type,
				"generation", machine.Generation,
				"version", machine.ResourceVersion,
			)

			if err := handleMachineEvent(ctx, log, wc, machine); err != nil {
				log.Error("reconciliation failed", "error", err)
				// Don't return - continue watching. The next event
				// or a retry will attempt reconciliation again.
			}
		}
	}
}

// handleMachineEvent processes a single Machine CR event, checking for drift
// and performing reconciliation if needed.
func handleMachineEvent(ctx context.Context, log *slog.Logger, c client.WithWatch, machine *v1alpha3.Machine) error {
	// Re-read the Machine CR from the API server to get the latest status.
	// The watch event object may be stale: during a reconciliation the
	// daemon writes multiple status updates (Provisioning, then Joining
	// with acknowledged counters), and earlier MODIFIED events may still
	// be queued with pre-acknowledgement status. Using a fresh GET avoids
	// spurious drift detection that would trigger a redundant update cycle.
	fresh := &v1alpha3.Machine{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(machine), fresh); err != nil {
		return fmt.Errorf("re-read Machine %q: %w", machine.Name, err)
	}
	machine = fresh

	// Re-read the active machine state on each event, because a previous
	// reconciliation may have changed the active nspawn machine name.
	active, err := nodeupdate.FindActiveMachine()
	if err != nil {
		return fmt.Errorf("find active machine: %w", err)
	}

	// Check for spec drift.
	spec := machineToUpdateSpec(machine)
	specDrift := nodeupdate.HasDrift(active.Config, spec)

	// Check for operation counter drift.
	opsDrift := hasOperationsDrift(machine)

	if !specDrift && !opsDrift {
		log.Debug("no drift detected")
		return nil
	}

	log.Info("drift detected",
		"spec_drift", specDrift,
		"ops_drift", opsDrift,
		"current_version", active.Config.Cluster.Version,
		"desired_version", specVersion(machine),
	)

	// Set status to Provisioning before starting work.
	if err := updateMachinePhase(ctx, c, machine, v1alpha3.MachinePhaseProvisioning, "agent daemon reconciling"); err != nil {
		log.Warn("failed to update phase to Provisioning", "error", err)
		// Continue with reconciliation even if status update fails.
	}

	// Build the new config by merging CR spec onto applied config.
	newCfg := nodeupdate.MergeSpec(active.Config, spec)

	// Execute the node update.
	if err := nodeupdate.Execute(ctx, log, active, newCfg); err != nil {
		// Update status to Failed.
		failMsg := fmt.Sprintf("node update failed: %v", err)
		if updateErr := updateMachineStatus(ctx, c, machine, v1alpha3.MachinePhaseFailed, failMsg, false); updateErr != nil {
			log.Warn("failed to update status after failure", "error", updateErr)
		}

		return fmt.Errorf("node update: %w", err)
	}

	// Acknowledge operation counters.
	acknowledgeOperations(machine)

	// Update status to Joining with success.
	if err := updateMachineStatus(ctx, c, machine, v1alpha3.MachinePhaseJoining, "node update completed", true); err != nil {
		log.Warn("failed to update status after success", "error", err)
	}

	log.Info("reconciliation completed",
		"new_version", newCfg.Cluster.Version,
	)

	return nil
}

// machineToUpdateSpec extracts a NodeUpdateSpec from a Machine CR.
func machineToUpdateSpec(machine *v1alpha3.Machine) *nodeupdate.NodeUpdateSpec {
	spec := &nodeupdate.NodeUpdateSpec{}

	if machine.Spec.Kubernetes != nil {
		spec.KubernetesVersion = strings.TrimPrefix(machine.Spec.Kubernetes.Version, "v")

		if machine.Spec.Kubernetes.NodeLabels != nil {
			spec.NodeLabels = machine.Spec.Kubernetes.NodeLabels
		}

		if machine.Spec.Kubernetes.RegisterWithTaints != nil {
			spec.RegisterWithTaints = machine.Spec.Kubernetes.RegisterWithTaints
		}
	}

	if machine.Spec.Agent != nil && machine.Spec.Agent.Image != "" {
		spec.OciImage = machine.Spec.Agent.Image
	}

	return spec
}

// hasOperationsDrift returns true if any operation counter in spec exceeds
// the corresponding status counter.
func hasOperationsDrift(machine *v1alpha3.Machine) bool {
	if machine.Spec.Operations == nil {
		return false
	}

	specOps := machine.Spec.Operations
	statusOps := machine.Status.Operations
	if statusOps == nil {
		statusOps = &v1alpha3.OperationsStatus{}
	}

	if specOps.RebootCounter > statusOps.RebootCounter {
		return true
	}

	if specOps.ReimageCounter > statusOps.ReimageCounter {
		return true
	}

	return false
}

// specVersion extracts the kubernetes version from a Machine spec, or returns
// empty string if not set.
func specVersion(machine *v1alpha3.Machine) string {
	if machine.Spec.Kubernetes != nil {
		return machine.Spec.Kubernetes.Version
	}

	return ""
}

// acknowledgeOperations copies spec operation counters to status, marking
// them as acted upon.
func acknowledgeOperations(machine *v1alpha3.Machine) {
	if machine.Spec.Operations == nil {
		return
	}

	if machine.Status.Operations == nil {
		machine.Status.Operations = &v1alpha3.OperationsStatus{}
	}

	machine.Status.Operations.RebootCounter = machine.Spec.Operations.RebootCounter
	machine.Status.Operations.ReimageCounter = machine.Spec.Operations.ReimageCounter
}

// updateMachinePhase sets the Machine phase and message via a status update.
func updateMachinePhase(ctx context.Context, c client.Client, machine *v1alpha3.Machine, phase v1alpha3.MachinePhase, message string) error {
	machine.Status.Phase = phase
	machine.Status.Message = message

	return c.Status().Update(ctx, machine)
}

// updateMachineStatus sets the Machine phase, message, operation counters,
// and the NodeUpdated condition via a status update.
func updateMachineStatus(
	ctx context.Context,
	c client.Client,
	machine *v1alpha3.Machine,
	phase v1alpha3.MachinePhase,
	message string,
	success bool,
) error {
	machine.Status.Phase = phase
	machine.Status.Message = message

	condStatus := metav1.ConditionFalse
	condReason := "Failed"
	if success {
		condStatus = metav1.ConditionTrue
		condReason = "Succeeded"
	}

	// Set the NodeUpdated condition.
	now := metav1.Now()
	found := false
	for i := range machine.Status.Conditions {
		if machine.Status.Conditions[i].Type == "NodeUpdated" {
			machine.Status.Conditions[i].Status = condStatus
			machine.Status.Conditions[i].Reason = condReason
			machine.Status.Conditions[i].Message = message
			machine.Status.Conditions[i].LastTransitionTime = now
			machine.Status.Conditions[i].ObservedGeneration = machine.Generation
			found = true

			break
		}
	}

	if !found {
		machine.Status.Conditions = append(machine.Status.Conditions, metav1.Condition{
			Type:               "NodeUpdated",
			Status:             condStatus,
			Reason:             condReason,
			Message:            message,
			LastTransitionTime: now,
			ObservedGeneration: machine.Generation,
		})
	}

	return c.Status().Update(ctx, machine)
}
