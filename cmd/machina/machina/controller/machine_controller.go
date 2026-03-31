package controller

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	stderrs "errors"

	unboundedv1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
	"github.com/project-unbounded/unbounded-kube/internal/provision"
)

const (
	// RequeueAfterReady is the requeue interval for machines in Ready phase.
	RequeueAfterReady = 5 * time.Minute

	// RequeueAfterPending is the requeue interval for machines in Pending phase.
	RequeueAfterPending = 30 * time.Second

	// RequeueAfterFailed is the requeue interval for machines in Failed phase.
	RequeueAfterFailed = 1 * time.Minute

	// RequeueAfterJoining is the requeue interval for machines in
	// Joining phase. Short interval because we are waiting for the
	// Node to appear with the matching label.
	RequeueAfterJoining = 30 * time.Second

	// TCPProbeTimeout is the timeout for TCP connection probes.
	TCPProbeTimeout = 10 * time.Second

	// SSHConnectTimeout is the timeout for SSH connections.
	SSHConnectTimeout = 30 * time.Second

	// remoteScriptPath is the path where the agent install script is
	// copied to on the remote machine.
	remoteScriptPath = "/tmp/machina-agent-install.sh"

	// SecretNamespaceMachinaSystem is the namespace where SSH key secrets
	// must reside. Machine is cluster-scoped, so we use a fixed namespace
	// for secret lookup.
	SecretNamespaceMachinaSystem = "machina-system"

	// MachineNodeLabel is the label key applied to Nodes that correspond
	// to a Machine. The value is the Machine name.
	MachineNodeLabel = "unbounded-kube.io/machine"
)

// ReachabilityChecker checks if a machine is reachable via TCP.
// The host string may include a port (e.g. "1.2.3.4:2222"); when no port
// is present, the default SSH port 22 is assumed.
type ReachabilityChecker interface {
	CheckReachable(ctx context.Context, host string) error
}

// DefaultReachabilityChecker implements ReachabilityChecker using TCP dial.
type DefaultReachabilityChecker struct {
	Timeout time.Duration
}

// CheckReachable checks if the machine is reachable via TCP.
// The host string may contain a port; if not, port 22 is used.
func (c *DefaultReachabilityChecker) CheckReachable(ctx context.Context, host string) error {
	address := hostPort(host)

	timeout := c.Timeout
	if timeout == 0 {
		timeout = TCPProbeTimeout
	}

	dialer := net.Dialer{Timeout: timeout}

	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("TCP dial %s: %w", address, err)
	}

	defer conn.Close() //nolint:errcheck // Best-effort close of probe connection.

	return nil
}

// hostPort ensures the host string contains a port. If no port is present,
// ":22" is appended.
func hostPort(host string) string {
	// If SplitHostPort succeeds, the host already includes a port.
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	} else {
		var addrErr *net.AddrError
		// For errors other than "missing port in address", return the host unchanged.
		if !stderrs.As(err, &addrErr) || addrErr == nil || addrErr.Err != "missing port in address" {
			return host
		}
	}

	// At this point, the address is missing a port. Normalize bracketed IPv6
	// literals like "[2001:db8::1]" by trimming the outer brackets before
	// calling JoinHostPort to avoid double-bracketing.
	if len(host) > 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}

	return net.JoinHostPort(host, "22")
}

// MachineProvisioner handles the actual provisioning of a machine via SSH.
type MachineProvisioner interface {
	ProvisionMachine(ctx context.Context, machine *unboundedv1alpha3.Machine, sshConfig *ssh.ClientConfig, bootstrapToken string, clusterInfo *ClusterInfo) error
}

// MachineReconciler reconciles a Machine object.
type MachineReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	ReachabilityChecker     ReachabilityChecker
	Provisioner             MachineProvisioner
	ClusterInfo             *ClusterInfo
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=unbounded-kube.io,resources=machines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=unbounded-kube.io,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=unbounded-kube.io,resources=machines/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile handles Machine reconciliation: reachability checks and provisioning.
func (r *MachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Machine.
	var machine unboundedv1alpha3.Machine
	if err := r.Get(ctx, req.NamespacedName, &machine); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		logger.Error(err, "Failed to get Machine")

		return ctrl.Result{}, err
	}

	// Machines without SSH configuration are not managed by this controller.
	if machine.Spec.SSH == nil {
		logger.Info("Machine has no SSH config, skipping", "name", machine.Name)
		return ctrl.Result{}, nil
	}

	logger.Info("Reconciling Machine", "name", machine.Name, "host", machine.Spec.SSH.Host)

	// Use injected checker or default.
	checker := r.ReachabilityChecker
	if checker == nil {
		checker = &DefaultReachabilityChecker{}
	}

	// Check if we can reach the machine via TCP.
	if err := checker.CheckReachable(ctx, machine.Spec.SSH.Host); err != nil {
		logger.Info("Machine is not reachable", "name", machine.Name, "host", machine.Spec.SSH.Host, "error", err)

		r.setSSHReachableCondition(&machine, metav1.ConditionFalse, "Unreachable", fmt.Sprintf("Machine is not reachable: %v", err))

		return r.updateStatus(ctx, &machine, unboundedv1alpha3.MachinePhasePending,
			fmt.Sprintf("Machine is not reachable: %v", err))
	}

	r.setSSHReachableCondition(&machine, metav1.ConditionTrue, "Reachable", "Machine is reachable via SSH")

	// If there is no kubernetes configuration we just mark it Ready.
	if machine.Spec.Kubernetes == nil {
		return r.updateStatus(ctx, &machine, unboundedv1alpha3.MachinePhaseReady, "Machine is reachable")
	}

	// If the machine is in a Node-lifecycle phase and was previously
	// provisioned, handle Node join. A machine that is Ready but was never
	// provisioned (e.g. it was reachable with no kubernetes config, then
	// kubernetes config was added) needs to go through provisioning first.
	switch machine.Status.Phase {
	case unboundedv1alpha3.MachinePhaseJoining:
		return r.reconcileNodeJoin(ctx, &machine)
	case unboundedv1alpha3.MachinePhaseReady:
		if wasProvisioned(&machine) {
			return r.reconcileNodeJoin(ctx, &machine)
		}
	}

	// Machine has kubernetes config — determine if provisioning is needed.
	return r.reconcileProvisioning(ctx, &machine)
}

// reconcileProvisioning handles the provisioning lifecycle for a machine that
// is reachable and has kubernetes configuration.
func (r *MachineReconciler) reconcileProvisioning(ctx context.Context, machine *unboundedv1alpha3.Machine) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if provisioning is needed.
	// Only provision from Pending, Failed, Ready (never provisioned), or
	// initial empty phases. Joining is handled by reconcileNodeJoin.
	switch machine.Status.Phase {
	case unboundedv1alpha3.MachinePhasePending,
		unboundedv1alpha3.MachinePhaseFailed,
		unboundedv1alpha3.MachinePhaseReady,
		"": // initial empty phase
		// proceed
	default:
		// If already Provisioning, just requeue.
		return ctrl.Result{RequeueAfter: RequeueAfterPending}, nil
	}

	// Build SSH config from the Machine's spec.
	sshConfig, err := r.buildSSHConfig(ctx, machine)
	if err != nil {
		return r.updateStatus(ctx, machine, unboundedv1alpha3.MachinePhaseFailed,
			fmt.Sprintf("Failed to build SSH config: %v", err))
	}

	// Get bootstrap token.
	bootstrapToken, err := r.getBootstrapToken(ctx, machine.Spec.Kubernetes.BootstrapTokenRef.Name)
	if err != nil {
		return r.updateStatus(ctx, machine, unboundedv1alpha3.MachinePhaseFailed,
			fmt.Sprintf("Failed to get bootstrap token: %v", err))
	}

	// Set phase to Provisioning.
	if _, err := r.updateStatus(ctx, machine, unboundedv1alpha3.MachinePhaseProvisioning,
		"Provisioning machine"); err != nil {
		return ctrl.Result{}, err
	}

	// Provision the machine.
	var provisionErr error
	if r.Provisioner != nil {
		provisionErr = r.Provisioner.ProvisionMachine(ctx, machine, sshConfig, bootstrapToken, r.ClusterInfo)
	} else {
		provisionErr = r.provisionMachine(ctx, machine, sshConfig, bootstrapToken)
	}

	if provisionErr != nil {
		logger.Error(provisionErr, "Failed to provision machine", "machine", machine.Name)

		// Re-fetch to avoid conflict after the Provisioning status update above.
		if err := r.Get(ctx, client.ObjectKeyFromObject(machine), machine); err != nil {
			return ctrl.Result{}, err
		}

		return r.updateStatus(ctx, machine, unboundedv1alpha3.MachinePhaseFailed,
			fmt.Sprintf("Provisioning failed: %v", provisionErr))
	}

	logger.Info("Machine provisioned successfully", "machine", machine.Name)

	// Re-fetch to avoid conflict after the Provisioning status update above.
	if err := r.Get(ctx, client.ObjectKeyFromObject(machine), machine); err != nil {
		return ctrl.Result{}, err
	}

	// Set the Provisioned condition with the current generation.
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:               unboundedv1alpha3.MachineConditionProvisioned,
		Status:             metav1.ConditionTrue,
		Reason:             "Provisioned",
		Message:            "Machine provisioned successfully",
		ObservedGeneration: machine.Generation,
	})

	return r.updateStatus(ctx, machine, unboundedv1alpha3.MachinePhaseJoining,
		"Machine provisioned successfully, waiting for Node to join")
}

// buildSSHConfig creates SSH client configuration from the Machine's SSH spec.
func (r *MachineReconciler) buildSSHConfig(ctx context.Context, machine *unboundedv1alpha3.Machine) (*ssh.ClientConfig, error) {
	privateKey, err := r.getSecretValue(ctx, &machine.Spec.SSH.PrivateKeyRef)
	if err != nil {
		return nil, fmt.Errorf("get SSH private key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return nil, fmt.Errorf("parse SSH private key: %w", err)
	}

	username := machine.Spec.SSH.Username
	if username == "" {
		username = "azureuser"
	}

	return &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         SSHConnectTimeout,
	}, nil
}

// getSecretValue retrieves a value from a secret in the machina-system namespace.
func (r *MachineReconciler) getSecretValue(ctx context.Context, ref *unboundedv1alpha3.SecretKeySelector) (string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: SecretNamespaceMachinaSystem, Name: ref.Name}, &secret); err != nil {
		return "", fmt.Errorf("get secret %s: %w", ref.Name, err)
	}

	key := ref.Key
	if key == "" {
		key = "ssh-privatekey"
	}

	data, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("key %s not found in secret %s", key, ref.Name)
	}

	return string(data), nil
}

// getBootstrapToken reads a Kubernetes bootstrap token secret from kube-system
// and returns the token in the standard "<token-id>.<token-secret>" format.
func (r *MachineReconciler) getBootstrapToken(ctx context.Context, secretName string) (string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: secretName}, &secret); err != nil {
		return "", fmt.Errorf("get bootstrap token secret %s in kube-system: %w", secretName, err)
	}

	tokenID, ok := secret.Data["token-id"]
	if !ok {
		return "", fmt.Errorf("key \"token-id\" not found in secret %s", secretName)
	}

	tokenSecret, ok := secret.Data["token-secret"]
	if !ok {
		return "", fmt.Errorf("key \"token-secret\" not found in secret %s", secretName)
	}

	return fmt.Sprintf("%s.%s", string(tokenID), string(tokenSecret)), nil
}

// provisionMachine provisions a single machine via SSH.
func (r *MachineReconciler) provisionMachine(
	ctx context.Context,
	machine *unboundedv1alpha3.Machine,
	sshConfig *ssh.ClientConfig,
	bootstrapToken string,
) error {
	logger := log.FromContext(ctx)

	address := hostPort(machine.Spec.SSH.Host)

	var (
		sshClient *ssh.Client
		err       error
	)

	// Handle bastion if configured.
	if machine.Spec.SSH.Bastion != nil {
		sshClient, err = r.dialViaBastion(ctx, machine, address, sshConfig)
	} else {
		sshClient, err = ssh.Dial("tcp", address, sshConfig)
	}

	if err != nil {
		return fmt.Errorf("SSH dial: %w", err)
	}

	defer sshClient.Close() //nolint:errcheck // Best-effort close of SSH connection.

	// Step 1: Copy the agent install script to the remote machine.
	logger.Info("Copying agent install script to remote machine", "machine", machine.Name)

	copySession, err := sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("create SSH session for script copy: %w", err)
	}

	agentInstallScript := provision.UnboundedAgentInstallScript()

	copySession.Stdin = bytes.NewBufferString(agentInstallScript)

	if runErr := copySession.Run(fmt.Sprintf("cat > %s && chmod +x %s", remoteScriptPath, remoteScriptPath)); runErr != nil {
		copySession.Close() //nolint:errcheck // Best-effort close after failed run.
		return fmt.Errorf("copy agent install script to remote: %w", runErr)
	}

	copySession.Close() //nolint:errcheck // Session is done; close error is not actionable.

	// Step 2: Always clean up the script when we are done, regardless of
	// whether execution succeeds or fails.
	defer func() {
		cleanupSession, cleanupErr := sshClient.NewSession()
		if cleanupErr != nil {
			logger.Error(cleanupErr, "Failed to create SSH session for script cleanup", "machine", machine.Name)
			return
		}

		defer cleanupSession.Close() //nolint:errcheck // Best-effort close of cleanup session.

		if cleanupErr = cleanupSession.Run(fmt.Sprintf("rm -f %s", remoteScriptPath)); cleanupErr != nil {
			logger.Error(cleanupErr, "Failed to clean up agent install script", "machine", machine.Name)
		}
	}()

	// Step 3: Build environment variables and execute the script.
	k8sVersion := ""
	if r.ClusterInfo != nil {
		k8sVersion = r.ClusterInfo.KubeVersion
	}

	if machine.Spec.Kubernetes != nil && machine.Spec.Kubernetes.Version != "" {
		k8sVersion = machine.Spec.Kubernetes.Version
	}

	if k8sVersion != "" && !strings.HasPrefix(k8sVersion, "v") {
		k8sVersion = "v" + k8sVersion
	}

	apiServer := ""
	caCertBase64 := ""
	clusterDNS := ""
	clusterRG := ""

	if r.ClusterInfo != nil {
		apiServer = r.ClusterInfo.APIServer
		caCertBase64 = r.ClusterInfo.CACertBase64
		clusterDNS = r.ClusterInfo.ClusterDNS
		clusterRG = r.ClusterInfo.ClusterRG
	}

	// Node labels are currently not propagated to the install script.
	// The NODE_LABELS env var is intentionally left empty until the
	// embedded AKS Flex Node install script supports consuming it.
	nodeLabels := ""

	logger.Info("Executing agent install script", "machine", machine.Name)

	execSession, err := sshClient.NewSession()
	if err != nil {
		return fmt.Errorf("create SSH session for script execution: %w", err)
	}

	defer execSession.Close() //nolint:errcheck // Best-effort close of exec session.

	var output bytes.Buffer

	execSession.Stdout = &output
	execSession.Stderr = &output

	cmd := fmt.Sprintf(
		`export API_SERVER=%q; `+
			`export BOOTSTRAP_TOKEN=%q; `+
			`export CA_CERT_BASE64=%q; `+
			`export KUBE_VERSION=%q; `+
			`export CLUSTER_DNS=%q; `+
			`export CLUSTER_RG=%q; `+
			`export MACHINA_MACHINE_NAME=%q; `+
			`export NODE_LABELS=%q; `+
			`sudo -E bash %s`,
		apiServer,
		bootstrapToken,
		caCertBase64,
		k8sVersion,
		clusterDNS,
		clusterRG,
		machine.Name,
		nodeLabels,
		remoteScriptPath,
	)

	if err := execSession.Run(cmd); err != nil {
		return fmt.Errorf("run agent install script: %w (output: %s)", err, output.String())
	}

	logger.Info("Agent install script completed", "machine", machine.Name, "output", output.String())

	return nil
}

// dialViaBastion establishes an SSH connection through a bastion host.
func (r *MachineReconciler) dialViaBastion(
	ctx context.Context,
	machine *unboundedv1alpha3.Machine,
	targetAddress string,
	targetConfig *ssh.ClientConfig,
) (*ssh.Client, error) {
	bastion := machine.Spec.SSH.Bastion

	bastionAddress := hostPort(bastion.Host)

	bastionUsername := bastion.Username
	if bastionUsername == "" {
		bastionUsername = "azureuser"
	}

	// Get bastion key — fall back to machine's key if not specified.
	var bastionKeyRef *unboundedv1alpha3.SecretKeySelector
	if bastion.PrivateKeyRef != nil {
		bastionKeyRef = bastion.PrivateKeyRef
	} else {
		bastionKeyRef = &machine.Spec.SSH.PrivateKeyRef
	}

	bastionPrivateKey, err := r.getSecretValue(ctx, bastionKeyRef)
	if err != nil {
		return nil, fmt.Errorf("get bastion SSH private key: %w", err)
	}

	bastionSigner, err := ssh.ParsePrivateKey([]byte(bastionPrivateKey))
	if err != nil {
		return nil, fmt.Errorf("parse bastion SSH private key: %w", err)
	}

	bastionConfig := &ssh.ClientConfig{
		User: bastionUsername,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(bastionSigner),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         SSHConnectTimeout,
	}

	bastionClient, err := ssh.Dial("tcp", bastionAddress, bastionConfig)
	if err != nil {
		return nil, fmt.Errorf("dial bastion: %w", err)
	}

	conn, err := bastionClient.Dial("tcp", targetAddress)
	if err != nil {
		bastionClient.Close() //nolint:errcheck // Best-effort close after dial failure.
		return nil, fmt.Errorf("dial target through bastion: %w", err)
	}

	ncc, chans, reqs, err := ssh.NewClientConn(conn, targetAddress, targetConfig)
	if err != nil {
		conn.Close()          //nolint:errcheck // Best-effort close after handshake failure.
		bastionClient.Close() //nolint:errcheck // Best-effort close after handshake failure.

		return nil, fmt.Errorf("create client connection through bastion: %w", err)
	}

	return ssh.NewClient(ncc, chans, reqs), nil
}

// setSSHReachableCondition updates the SSHReachable condition on the machine.
func (r *MachineReconciler) setSSHReachableCondition(machine *unboundedv1alpha3.Machine, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type:    unboundedv1alpha3.MachineConditionSSHReachable,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
}

// updateStatus updates the machine status and returns the appropriate result.
func (r *MachineReconciler) updateStatus(
	ctx context.Context,
	machine *unboundedv1alpha3.Machine,
	phase unboundedv1alpha3.MachinePhase,
	message string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	machine.Status.Phase = phase
	machine.Status.Message = message

	if err := r.Status().Update(ctx, machine); err != nil {
		logger.Error(err, "Failed to update Machine status")
		return ctrl.Result{RequeueAfter: RequeueAfterPending}, err
	}

	logger.Info("Updated Machine status", "name", machine.Name, "phase", phase)

	// Determine requeue interval based on phase.
	switch phase {
	case unboundedv1alpha3.MachinePhaseReady:
		return ctrl.Result{RequeueAfter: RequeueAfterReady}, nil
	case unboundedv1alpha3.MachinePhaseJoining:
		return ctrl.Result{RequeueAfter: RequeueAfterJoining}, nil
	case unboundedv1alpha3.MachinePhaseFailed:
		return ctrl.Result{RequeueAfter: RequeueAfterFailed}, nil
	default:
		return ctrl.Result{RequeueAfter: RequeueAfterPending}, nil
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *MachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&unboundedv1alpha3.Machine{}).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.findMachineForNode),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}

// findMachineForNode maps a Node event to the Machine that owns it (if any)
// by looking at the MachineNodeLabel label on the Node.
func (r *MachineReconciler) findMachineForNode(_ context.Context, obj client.Object) []ctrl.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}

	machineName, found := node.Labels[MachineNodeLabel]
	if !found || machineName == "" {
		return nil
	}

	return []ctrl.Request{
		{NamespacedName: client.ObjectKey{Name: machineName}},
	}
}

// reconcileNodeJoin handles the Node lifecycle for a provisioned Machine.
// It looks for a Node with the matching label and transitions the Machine
// between Joining and Ready phases.
func (r *MachineReconciler) reconcileNodeJoin(ctx context.Context, machine *unboundedv1alpha3.Machine) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Determine the node name to look for. If the machine has a nodeRef in
	// its kubernetes spec, use that; otherwise look up by label.
	var nodeList corev1.NodeList

	if machine.Spec.Kubernetes != nil && machine.Spec.Kubernetes.NodeRef != nil {
		// Look for the specific node by name.
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: machine.Spec.Kubernetes.NodeRef.Name}, &node); err != nil {
			if errors.IsNotFound(err) {
				// Node doesn't exist yet; handled below.
			} else {
				logger.Error(err, "Failed to get Node for Machine", "machine", machine.Name)
				return ctrl.Result{}, err
			}
		} else {
			nodeList.Items = append(nodeList.Items, node)
		}
	} else {
		// Look for a Node with the matching label.
		if err := r.List(ctx, &nodeList, client.MatchingLabels{MachineNodeLabel: machine.Name}); err != nil {
			logger.Error(err, "Failed to list Nodes for Machine", "machine", machine.Name)
			return ctrl.Result{}, err
		}
	}

	switch machine.Status.Phase {
	case unboundedv1alpha3.MachinePhaseJoining:
		if len(nodeList.Items) == 0 {
			// Still waiting for Node to appear.
			return ctrl.Result{RequeueAfter: RequeueAfterJoining}, nil
		}

		// Node found — transition to Ready.
		node := &nodeList.Items[0]

		logger.Info("Node found for Machine, transitioning to Ready",
			"machine", machine.Name, "node", node.Name)

		// Update nodeRef in the kubernetes spec if not already set.
		if machine.Spec.Kubernetes != nil && machine.Spec.Kubernetes.NodeRef == nil {
			machine.Spec.Kubernetes.NodeRef = &unboundedv1alpha3.LocalObjectReference{Name: node.Name}

			if err := r.Update(ctx, machine); err != nil {
				logger.Error(err, "Failed to update Machine spec with nodeRef", "machine", machine.Name)
				return ctrl.Result{}, err
			}
		}

		return r.updateStatus(ctx, machine, unboundedv1alpha3.MachinePhaseReady,
			fmt.Sprintf("Node %s joined", node.Name))

	case unboundedv1alpha3.MachinePhaseReady:
		if len(nodeList.Items) > 0 {
			// Node still exists — stay Ready.
			return ctrl.Result{RequeueAfter: RequeueAfterReady}, nil
		}

		// Node disappeared — transition back to Joining so we wait for
		// it to come back (or re-provision on the next cycle).
		logger.Info("Node disappeared for Machine, transitioning to Joining",
			"machine", machine.Name)

		return r.updateStatus(ctx, machine, unboundedv1alpha3.MachinePhaseJoining,
			"Node disappeared, waiting for Node to rejoin")

	default:
		// Should not be called for other phases, but handle gracefully.
		return ctrl.Result{}, nil
	}
}

// wasProvisioned returns true if the machine has a Provisioned condition set to True.
func wasProvisioned(machine *unboundedv1alpha3.Machine) bool {
	cond := apimeta.FindStatusCondition(machine.Status.Conditions, unboundedv1alpha3.MachineConditionProvisioned)
	return cond != nil && cond.Status == metav1.ConditionTrue
}
