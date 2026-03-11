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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	machinav1alpha2 "github.com/project-unbounded/unbounded-kube/cmd/machina/machina/api/v1alpha2"
)

const (
	// RequeueAfterReady is the requeue interval for machines in Ready phase.
	RequeueAfterReady = 5 * time.Minute

	// RequeueAfterPending is the requeue interval for machines in Pending phase.
	RequeueAfterPending = 30 * time.Second

	// RequeueAfterFailed is the requeue interval for machines in Failed phase.
	RequeueAfterFailed = 1 * time.Minute

	// RequeueAfterProvisioned is the requeue interval for machines in
	// Provisioned phase. Short interval because we are waiting for the
	// Node to appear with the matching label.
	RequeueAfterProvisioned = 30 * time.Second

	// RequeueAfterJoined is the requeue interval for machines in Joined phase.
	RequeueAfterJoined = 5 * time.Minute

	// RequeueAfterOrphaned is the requeue interval for machines in Orphaned phase.
	RequeueAfterOrphaned = 1 * time.Minute

	// TCPProbeTimeout is the timeout for TCP connection probes.
	TCPProbeTimeout = 10 * time.Second

	// SSHConnectTimeout is the timeout for SSH connections.
	SSHConnectTimeout = 30 * time.Second

	// remoteScriptPath is the path where the agent install script is
	// copied to on the remote machine.
	remoteScriptPath = "/tmp/machina-agent-install.sh"

	// SecretNamespaceMachinaSystem is the namespace where SSH key secrets
	// must reside. Machine and MachineModel are cluster-scoped, so we use
	// a fixed namespace for secret lookup.
	SecretNamespaceMachinaSystem = "machina-system"

	// MachinaNodeLabel is the label key applied to Nodes that correspond
	// to a Machine. The value is the Machine name.
	MachinaNodeLabel = "machina.project-unbounded.io/machine"
)

// ReachabilityChecker checks if a machine is reachable via TCP.
// Returns nil if reachable, or an error describing why the machine is unreachable.
type ReachabilityChecker interface {
	CheckReachable(ctx context.Context, host string, port int32) error
}

// DefaultReachabilityChecker implements ReachabilityChecker using TCP dial.
type DefaultReachabilityChecker struct {
	Timeout time.Duration
}

// CheckReachable checks if the machine is reachable via TCP on the specified port.
// Returns nil if reachable, or the dial error if not.
func (c *DefaultReachabilityChecker) CheckReachable(ctx context.Context, host string, port int32) error {
	address := fmt.Sprintf("%s:%d", host, port)

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

// MachineProvisioner handles the actual provisioning of a machine via SSH.
type MachineProvisioner interface {
	ProvisionMachine(ctx context.Context, machine *machinav1alpha2.Machine, model *machinav1alpha2.MachineModel, sshConfig *ssh.ClientConfig, bootstrapToken string, clusterInfo *ClusterInfo) error
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

// +kubebuilder:rbac:groups=machina.unboundedkube.io,resources=machines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=machina.unboundedkube.io,resources=machines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=machina.unboundedkube.io,resources=machines/finalizers,verbs=update
// +kubebuilder:rbac:groups=machina.unboundedkube.io,resources=machinemodels,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile handles Machine reconciliation: reachability checks and provisioning.
func (r *MachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Machine.
	var machine machinav1alpha2.Machine
	if err := r.Get(ctx, req.NamespacedName, &machine); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		logger.Error(err, "Failed to get Machine")

		return ctrl.Result{}, err
	}

	logger.Info("Reconciling Machine", "name", machine.Name, "host", machine.Spec.SSH.Host, "port", machine.Spec.SSH.Port)

	// Set default port if not specified.
	port := machine.Spec.SSH.Port
	if port == 0 {
		port = 22
	}

	// Use injected checker or default.
	checker := r.ReachabilityChecker
	if checker == nil {
		checker = &DefaultReachabilityChecker{}
	}

	// Check if we can reach the machine via TCP.
	if err := checker.CheckReachable(ctx, machine.Spec.SSH.Host, port); err != nil {
		logger.Info("Machine is not reachable", "name", machine.Name, "host", machine.Spec.SSH.Host, "port", port, "error", err)

		return r.updateStatus(ctx, &machine, machinav1alpha2.MachinePhasePending,
			fmt.Sprintf("Machine is not reachable: %v", err), 0)
	}

	// Machine is reachable. If there is no modelRef we just mark it Ready.
	if machine.Spec.ModelRef == nil {
		return r.updateStatus(ctx, &machine, machinav1alpha2.MachinePhaseReady, "Machine is reachable", 0)
	}

	// If the machine is in a Node-lifecycle phase, handle Node join/orphan.
	switch machine.Status.Phase {
	case machinav1alpha2.MachinePhaseProvisioned,
		machinav1alpha2.MachinePhaseJoined,
		machinav1alpha2.MachinePhaseOrphaned:
		return r.reconcileNodeJoin(ctx, &machine)
	}

	// Machine has a modelRef — determine if provisioning is needed.
	return r.reconcileProvisioning(ctx, &machine)
}

// reconcileProvisioning handles the provisioning lifecycle for a machine that
// is reachable and has a modelRef.
func (r *MachineReconciler) reconcileProvisioning(ctx context.Context, machine *machinav1alpha2.Machine) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the referenced MachineModel.
	var model machinav1alpha2.MachineModel

	modelKey := client.ObjectKey{
		Name: machine.Spec.ModelRef.Name,
	}
	if err := r.Get(ctx, modelKey, &model); err != nil {
		if errors.IsNotFound(err) {
			return r.updateStatus(ctx, machine, machinav1alpha2.MachinePhaseFailed,
				fmt.Sprintf("MachineModel %q not found", machine.Spec.ModelRef.Name), 0)
		}

		logger.Error(err, "Failed to get MachineModel")

		return ctrl.Result{}, err
	}

	// Check if provisioning is needed.
	// Only provision from Ready, Pending, Failed, or initial empty phases.
	// Provisioned, Joined, and Orphaned are handled by reconcileNodeJoin.
	switch machine.Status.Phase {
	case machinav1alpha2.MachinePhaseReady,
		machinav1alpha2.MachinePhasePending,
		machinav1alpha2.MachinePhaseFailed,
		"": // initial empty phase
		// proceed
	default:
		// If already Provisioning, just requeue.
		return ctrl.Result{RequeueAfter: RequeueAfterPending}, nil
	}

	// Build SSH config.
	sshConfig, err := r.buildSSHConfig(ctx, &model)
	if err != nil {
		return r.updateStatus(ctx, machine, machinav1alpha2.MachinePhaseFailed,
			fmt.Sprintf("Failed to build SSH config: %v", err), 0)
	}

	// Get bootstrap token if kubernetes profile is configured.
	var bootstrapToken string
	if model.Spec.KubernetesProfile != nil {
		bootstrapToken, err = r.getBootstrapToken(ctx, model.Spec.KubernetesProfile.BootstrapTokenRef.Name)
		if err != nil {
			return r.updateStatus(ctx, machine, machinav1alpha2.MachinePhaseFailed,
				fmt.Sprintf("Failed to get bootstrap token: %v", err), 0)
		}
	}

	// Set phase to Provisioning.
	if _, err := r.updateStatus(ctx, machine, machinav1alpha2.MachinePhaseProvisioning,
		"Provisioning machine", 0); err != nil {
		return ctrl.Result{}, err
	}

	// Provision the machine.
	var provisionErr error
	if r.Provisioner != nil {
		provisionErr = r.Provisioner.ProvisionMachine(ctx, machine, &model, sshConfig, bootstrapToken, r.ClusterInfo)
	} else {
		provisionErr = r.provisionMachine(ctx, machine, &model, sshConfig, bootstrapToken)
	}

	if provisionErr != nil {
		logger.Error(provisionErr, "Failed to provision machine", "machine", machine.Name)

		// Re-fetch to avoid conflict after the Provisioning status update above.
		if err := r.Get(ctx, client.ObjectKeyFromObject(machine), machine); err != nil {
			return ctrl.Result{}, err
		}

		return r.updateStatus(ctx, machine, machinav1alpha2.MachinePhaseFailed,
			fmt.Sprintf("Provisioning failed: %v", provisionErr), 0)
	}

	logger.Info("Machine provisioned successfully", "machine", machine.Name)

	// Re-fetch to avoid conflict after the Provisioning status update above.
	if err := r.Get(ctx, client.ObjectKeyFromObject(machine), machine); err != nil {
		return ctrl.Result{}, err
	}

	return r.updateStatus(ctx, machine, machinav1alpha2.MachinePhaseProvisioned,
		"Machine provisioned successfully", model.Generation)
}

// buildSSHConfig creates SSH client configuration from the model.
func (r *MachineReconciler) buildSSHConfig(ctx context.Context, model *machinav1alpha2.MachineModel) (*ssh.ClientConfig, error) {
	privateKey, err := r.getSecretValue(ctx, &model.Spec.SSHPrivateKeyRef)
	if err != nil {
		return nil, fmt.Errorf("get SSH private key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return nil, fmt.Errorf("parse SSH private key: %w", err)
	}

	username := model.Spec.SSHUsername
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
func (r *MachineReconciler) getSecretValue(ctx context.Context, ref *machinav1alpha2.SecretKeySelector) (string, error) {
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
	machine *machinav1alpha2.Machine,
	model *machinav1alpha2.MachineModel,
	sshConfig *ssh.ClientConfig,
	bootstrapToken string,
) error {
	logger := log.FromContext(ctx)

	port := machine.Spec.SSH.Port
	if port == 0 {
		port = 22
	}

	address := fmt.Sprintf("%s:%d", machine.Spec.SSH.Host, port)

	var (
		sshClient *ssh.Client
		err       error
	)

	// Handle jumpbox if configured.
	if model.Spec.Jumpbox != nil {
		sshClient, err = r.dialViaJumpHost(ctx, model, address, sshConfig)
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

	copySession.Stdin = bytes.NewBufferString(model.Spec.AgentInstallScript)

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

	if model.Spec.KubernetesProfile != nil && model.Spec.KubernetesProfile.Version != "" {
		k8sVersion = model.Spec.KubernetesProfile.Version
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
			`sudo -E bash %s`,
		apiServer,
		bootstrapToken,
		caCertBase64,
		k8sVersion,
		clusterDNS,
		clusterRG,
		machine.Name,
		remoteScriptPath,
	)

	if err := execSession.Run(cmd); err != nil {
		return fmt.Errorf("run agent install script: %w (output: %s)", err, output.String())
	}

	logger.Info("Agent install script completed", "machine", machine.Name, "output", output.String())

	return nil
}

// dialViaJumpHost establishes SSH connection through a jumpbox.
func (r *MachineReconciler) dialViaJumpHost(
	ctx context.Context,
	model *machinav1alpha2.MachineModel,
	targetAddress string,
	targetConfig *ssh.ClientConfig,
) (*ssh.Client, error) {
	jumpbox := model.Spec.Jumpbox

	jumpPort := jumpbox.Port
	if jumpPort == 0 {
		jumpPort = 22
	}

	jumpUsername := jumpbox.SSHUsername
	if jumpUsername == "" {
		jumpUsername = "azureuser"
	}

	// Get jumpbox key — fall back to model's key if not specified.
	var jumpKeyRef *machinav1alpha2.SecretKeySelector
	if jumpbox.SSHPrivateKeyRef != nil {
		jumpKeyRef = jumpbox.SSHPrivateKeyRef
	} else {
		jumpKeyRef = &model.Spec.SSHPrivateKeyRef
	}

	jumpPrivateKey, err := r.getSecretValue(ctx, jumpKeyRef)
	if err != nil {
		return nil, fmt.Errorf("get jumpbox SSH private key: %w", err)
	}

	jumpSigner, err := ssh.ParsePrivateKey([]byte(jumpPrivateKey))
	if err != nil {
		return nil, fmt.Errorf("parse jumpbox SSH private key: %w", err)
	}

	jumpConfig := &ssh.ClientConfig{
		User: jumpUsername,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(jumpSigner),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         SSHConnectTimeout,
	}

	jumpAddress := fmt.Sprintf("%s:%d", jumpbox.Host, jumpPort)

	jumpClient, err := ssh.Dial("tcp", jumpAddress, jumpConfig)
	if err != nil {
		return nil, fmt.Errorf("dial jumpbox: %w", err)
	}

	conn, err := jumpClient.Dial("tcp", targetAddress)
	if err != nil {
		jumpClient.Close() //nolint:errcheck // Best-effort close after dial failure.
		return nil, fmt.Errorf("dial target through jumpbox: %w", err)
	}

	ncc, chans, reqs, err := ssh.NewClientConn(conn, targetAddress, targetConfig)
	if err != nil {
		conn.Close()       //nolint:errcheck // Best-effort close after handshake failure.
		jumpClient.Close() //nolint:errcheck // Best-effort close after handshake failure.

		return nil, fmt.Errorf("create client connection through jumpbox: %w", err)
	}

	return ssh.NewClient(ncc, chans, reqs), nil
}

// updateStatus updates the machine status and returns the appropriate result.
func (r *MachineReconciler) updateStatus(
	ctx context.Context,
	machine *machinav1alpha2.Machine,
	phase machinav1alpha2.MachinePhase,
	message string,
	modelGeneration int64,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	machine.Status.Phase = phase
	machine.Status.Message = message
	machine.Status.LastProbeTime = &metav1.Time{Time: time.Now()}

	if phase == machinav1alpha2.MachinePhaseProvisioned && modelGeneration > 0 {
		machine.Status.ProvisionedModelGeneration = modelGeneration
	}

	// Update condition.
	condition := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             string(phase),
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	if phase == machinav1alpha2.MachinePhaseReady || phase == machinav1alpha2.MachinePhaseProvisioned || phase == machinav1alpha2.MachinePhaseJoined {
		condition.Status = metav1.ConditionTrue
	}

	// Update or add the condition.
	found := false

	for i, c := range machine.Status.Conditions {
		if c.Type == condition.Type {
			machine.Status.Conditions[i] = condition
			found = true

			break
		}
	}

	if !found {
		machine.Status.Conditions = append(machine.Status.Conditions, condition)
	}

	if err := r.Status().Update(ctx, machine); err != nil {
		logger.Error(err, "Failed to update Machine status")
		return ctrl.Result{RequeueAfter: RequeueAfterPending}, err
	}

	logger.Info("Updated Machine status", "name", machine.Name, "phase", phase)

	// Determine requeue interval based on phase.
	switch phase {
	case machinav1alpha2.MachinePhaseReady:
		return ctrl.Result{RequeueAfter: RequeueAfterReady}, nil
	case machinav1alpha2.MachinePhaseProvisioned:
		return ctrl.Result{RequeueAfter: RequeueAfterProvisioned}, nil
	case machinav1alpha2.MachinePhaseJoined:
		return ctrl.Result{RequeueAfter: RequeueAfterJoined}, nil
	case machinav1alpha2.MachinePhaseOrphaned:
		return ctrl.Result{RequeueAfter: RequeueAfterOrphaned}, nil
	case machinav1alpha2.MachinePhaseFailed:
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
		For(&machinav1alpha2.Machine{}).
		Watches(
			&machinav1alpha2.MachineModel{},
			handler.EnqueueRequestsFromMapFunc(r.findMachinesForModel),
		).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.findMachineForNode),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}

// findMachinesForModel finds all machines that reference the given model.
func (r *MachineReconciler) findMachinesForModel(ctx context.Context, obj client.Object) []ctrl.Request {
	model, ok := obj.(*machinav1alpha2.MachineModel)
	if !ok {
		return nil
	}

	var machines machinav1alpha2.MachineList
	if err := r.List(ctx, &machines); err != nil {
		return nil
	}

	var requests []ctrl.Request

	for _, machine := range machines.Items {
		if machine.Spec.ModelRef != nil && machine.Spec.ModelRef.Name == model.Name {
			requests = append(requests, ctrl.Request{
				NamespacedName: client.ObjectKey{
					Name: machine.Name,
				},
			})
		}
	}

	return requests
}

// findMachineForNode maps a Node event to the Machine that owns it (if any)
// by looking at the MachinaNodeLabel label on the Node.
func (r *MachineReconciler) findMachineForNode(_ context.Context, obj client.Object) []ctrl.Request {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil
	}

	machineName, found := node.Labels[MachinaNodeLabel]
	if !found || machineName == "" {
		return nil
	}

	return []ctrl.Request{
		{NamespacedName: client.ObjectKey{Name: machineName}},
	}
}

// reconcileNodeJoin handles the Node lifecycle for a provisioned Machine.
// It looks for a Node with the matching label and transitions the Machine
// between Provisioned, Joined, and Orphaned phases.
func (r *MachineReconciler) reconcileNodeJoin(ctx context.Context, machine *machinav1alpha2.Machine) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Look for a Node with the matching label.
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList, client.MatchingLabels{MachinaNodeLabel: machine.Name}); err != nil {
		logger.Error(err, "Failed to list Nodes for Machine", "machine", machine.Name)
		return ctrl.Result{}, err
	}

	switch machine.Status.Phase {
	case machinav1alpha2.MachinePhaseProvisioned:
		if len(nodeList.Items) == 0 {
			// Still waiting for Node to appear.
			return ctrl.Result{RequeueAfter: RequeueAfterProvisioned}, nil
		}

		// Node found — transition to Joined.
		node := &nodeList.Items[0]

		logger.Info("Node found for Machine, transitioning to Joined",
			"machine", machine.Name, "node", node.Name)

		machine.Status.NodeRef = &machinav1alpha2.NodeReference{Name: node.Name}

		return r.updateStatus(ctx, machine, machinav1alpha2.MachinePhaseJoined,
			fmt.Sprintf("Node %s joined", node.Name), 0)

	case machinav1alpha2.MachinePhaseJoined:
		if len(nodeList.Items) > 0 {
			// Node still exists — stay Joined.
			return ctrl.Result{RequeueAfter: RequeueAfterJoined}, nil
		}

		// Node disappeared — transition to Orphaned.
		logger.Info("Node disappeared for Machine, transitioning to Orphaned",
			"machine", machine.Name)

		return r.updateStatus(ctx, machine, machinav1alpha2.MachinePhaseOrphaned,
			"Node disappeared", 0)

	case machinav1alpha2.MachinePhaseOrphaned:
		if len(nodeList.Items) == 0 {
			// Still orphaned.
			return ctrl.Result{RequeueAfter: RequeueAfterOrphaned}, nil
		}

		// Node reappeared — transition back to Joined.
		node := &nodeList.Items[0]

		logger.Info("Node reappeared for Machine, transitioning to Joined",
			"machine", machine.Name, "node", node.Name)

		machine.Status.NodeRef = &machinav1alpha2.NodeReference{Name: node.Name}

		return r.updateStatus(ctx, machine, machinav1alpha2.MachinePhaseJoined,
			fmt.Sprintf("Node %s rejoined", node.Name), 0)

	default:
		// Should not be called for other phases, but handle gracefully.
		return ctrl.Result{}, nil
	}
}
