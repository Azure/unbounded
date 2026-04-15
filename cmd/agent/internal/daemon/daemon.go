// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/provision"
)

// NewKubeClient is the constructor for the Kubernetes watch client. It is
// overridden in tests to inject a fake client.
var NewKubeClient = defaultNewKubeClient

const (
	// watchRetryInterval is the delay between watch re-establishment attempts.
	watchRetryInterval = 10 * time.Second
)

// Run is the main daemon loop. It discovers the active nspawn machine,
// builds a Kubernetes client from the applied config, and watches the
// Machine CR for changes.
func Run(ctx context.Context, log *slog.Logger) error {
	// Find the active machine and its applied config.
	active, err := findActiveMachine()
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

	// Ensure a Machine CR exists before entering the watch loop. In
	// dynamic environments (manual-bootstrap, cloud-init) a Machine CR
	// may not have been pre-created by machina.
	if err := registerMachine(ctx, log, kubeClient, active.Config); err != nil {
		return fmt.Errorf("register machine: %w", err)
	}

	// reconcileCh is a buffered channel (capacity 1) that signals the
	// worker goroutine that reconciliation may be needed. The watch loop
	// performs a non-blocking send on every relevant event; the buffer
	// naturally coalesces bursts so the worker processes at most one
	// reconciliation at a time. This keeps the watch loop free to drain
	// events without backpressuring the API server's HTTP/2 stream.
	reconcileCh := make(chan struct{}, 1)

	// Worker goroutine: runs handleMachineEvent whenever signalled.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reconcileCh:
				if err := handleMachineEvent(ctx, log, kubeClient, machineName); err != nil {
					log.Error("reconciliation failed", "error", err)
				}
			}
		}
	}()

	// Enter the watch loop.
	for {
		if err := watchMachine(ctx, log, kubeClient, machineName, reconcileCh); err != nil {
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
// the cluster CA certificate embedded in the config.
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

	return NewKubeClient(restCfg, client.Options{Scheme: s})
}

// defaultNewKubeClient is the production constructor for the Kubernetes
// WithWatch client.
func defaultNewKubeClient(cfg *rest.Config, opts client.Options) (client.WithWatch, error) {
	return client.NewWithWatch(cfg, opts)
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

// watchMachine establishes a watch on the named Machine CR and signals the
// reconcile channel on relevant events. It returns when the watch closes or
// the context is cancelled. The actual reconciliation happens asynchronously
// in the worker goroutine, keeping this loop free to drain the watch stream.
func watchMachine(ctx context.Context, log *slog.Logger, wc client.WithWatch, machineName string, reconcileCh chan<- struct{}) error {
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

			// Signal the worker goroutine. Non-blocking: if there
			// is already a pending signal the buffer coalesces it.
			select {
			case reconcileCh <- struct{}{}:
			default:
			}
		}
	}
}

// handleMachineEvent processes a single reconciliation cycle. It reads the
// current Machine CR from the API server, checks for drift, and performs
// reconciliation if needed.
func handleMachineEvent(ctx context.Context, log *slog.Logger, c client.WithWatch, machineName string) error {
	// Read the Machine CR from the API server to get the latest state.
	machine := &v1alpha3.Machine{}
	if err := c.Get(ctx, client.ObjectKey{Name: machineName}, machine); err != nil {
		return fmt.Errorf("get Machine %q: %w", machineName, err)
	}

	// Re-read the active machine state on each event, because a previous
	// reconciliation may have changed the active nspawn machine name.
	active, err := findActiveMachine()
	if err != nil {
		return fmt.Errorf("find active machine: %w", err)
	}

	// Build the desired config from the Machine CR overlaid on applied config.
	desired := desiredConfigFromMachine(active.Config, machine)

	// Check for operation counter drift. Counter bumps are the only
	// trigger for reconciliation; actual config drift (version, image,
	// etc.) is checked inside UpdateNode to decide whether the
	// expensive rootfs reprovision is needed.
	if !hasOperationsDrift(machine) {
		log.Debug("no operation counter drift")
		return nil
	}

	log.Info("operation counter drift detected",
		"current_version", active.Config.Cluster.Version,
		"desired_version", specVersion(machine),
	)

	// Set status to Provisioning before starting work.
	if err := updateMachinePhase(ctx, c, machine, v1alpha3.MachinePhaseProvisioning, "agent daemon reconciling"); err != nil {
		log.Warn("failed to update phase to Provisioning", "error", err)
		// Continue with reconciliation even if status update fails.
	}

	// Execute the node update with the desired config.
	if err := updateNode(ctx, log, active, desired); err != nil {
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
		"new_version", desired.Cluster.Version,
	)

	return nil
}

// desiredConfigFromMachine builds the desired AgentConfig by overlaying
// fields from the Machine CR onto the applied config. Fields not present in
// the CR (API server, CA cert, cluster DNS, bootstrap token, etc.) are
// preserved from the applied config.
func desiredConfigFromMachine(applied *provision.AgentConfig, machine *v1alpha3.Machine) *provision.AgentConfig {
	// Deep copy the applied config as the base.
	desired := *applied
	desired.Cluster = applied.Cluster
	desired.Kubelet = applied.Kubelet

	// Copy labels map to avoid aliasing.
	if applied.Kubelet.Labels != nil {
		desired.Kubelet.Labels = make(map[string]string, len(applied.Kubelet.Labels))
		for k, v := range applied.Kubelet.Labels {
			desired.Kubelet.Labels[k] = v
		}
	}

	// Copy taints slice.
	if applied.Kubelet.RegisterWithTaints != nil {
		desired.Kubelet.RegisterWithTaints = make([]string, len(applied.Kubelet.RegisterWithTaints))
		copy(desired.Kubelet.RegisterWithTaints, applied.Kubelet.RegisterWithTaints)
	}

	// Preserve Attest pointer.
	if applied.Attest != nil {
		a := *applied.Attest
		desired.Attest = &a
	}

	// Overlay Machine CR fields.
	if machine.Spec.Kubernetes != nil {
		if v := machine.Spec.Kubernetes.Version; v != "" {
			desired.Cluster.Version = strings.TrimPrefix(v, "v")
		}

		if labels := machine.Spec.Kubernetes.NodeLabels; len(labels) > 0 {
			desired.Kubelet.Labels = make(map[string]string, len(labels))
			for k, v := range labels {
				desired.Kubelet.Labels[k] = v
			}
		}

		if taints := machine.Spec.Kubernetes.RegisterWithTaints; len(taints) > 0 {
			desired.Kubelet.RegisterWithTaints = make([]string, len(taints))
			copy(desired.Kubelet.RegisterWithTaints, taints)
		}
	}

	if machine.Spec.Agent != nil && machine.Spec.Agent.Image != "" {
		desired.OCIImage = machine.Spec.Agent.Image
	}

	return &desired
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

// acknowledgeOperations copies the spec reimage counter to status, marking
// it as acted upon. The reboot counter is not acknowledged here because
// reboots are handled separately.
func acknowledgeOperations(machine *v1alpha3.Machine) {
	if machine.Spec.Operations == nil {
		return
	}

	if machine.Status.Operations == nil {
		machine.Status.Operations = &v1alpha3.OperationsStatus{}
	}

	machine.Status.Operations.ReimageCounter = machine.Spec.Operations.ReimageCounter
}

// updateMachinePhase sets the Machine phase, message, and a corresponding
// NodeUpdated condition via a status update. The condition tracks the
// in-progress state so that phase transitions are always backed by
// observable conditions.
func updateMachinePhase(ctx context.Context, c client.Client, machine *v1alpha3.Machine, phase v1alpha3.MachinePhase, message string) error {
	machine.Status.Phase = phase
	machine.Status.Message = message

	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineConditionNodeUpdated,
		Status:             metav1.ConditionFalse,
		Reason:             "InProgress",
		Message:            message,
		ObservedGeneration: machine.Generation,
	})

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

	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               v1alpha3.MachineConditionNodeUpdated,
		Status:             condStatus,
		Reason:             condReason,
		Message:            message,
		ObservedGeneration: machine.Generation,
	})

	return c.Status().Update(ctx, machine)
}
