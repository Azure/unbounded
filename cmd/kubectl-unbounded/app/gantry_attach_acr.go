// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/kube"
	"github.com/Azure/unbounded/internal/registryauth"
	"github.com/Azure/unbounded/internal/unbounded"
)

const acrAttachManager = "unbounded-acr-attach"

// registryAuthConfigHashKey is the annotation the operator stamps on the
// identity-use grant with the hash of the policy it was published for, so the
// CLI can confirm the grant backs the current policy before acknowledging it.
// It must match the operator's registryAuthHash key.
const registryAuthConfigHashKey = "unbounded-cloud.io/registry-auth-config-hash"

var (
	errACRProxyNotReady = errors.New("AKS admission did not expose a supported identity-binding token proxy; verify feature availability")
	// errUnownedGrant marks a terminal acknowledgment failure: the identity-use
	// grant exists but is not owned by this installation, so it will not become
	// owned by a re-publish and the caller must not keep polling.
	errUnownedGrant = errors.New("identity-use grant is not owned by this installation")
)

type attachACRHandler struct {
	acrID, clusterID, authMode, namespace, kubeconfig string
	timeout                                           time.Duration
	client                                            kubernetes.Interface
	restConfig                                        *rest.Config
	machineClient                                     machineStatusReader
	runAzure                                          func(context.Context, string, ...string) ([]byte, error)
	out                                               io.Writer
}

// flexNodeStatus is the structured, per-node registry-auth convergence state the
// CLI waits on. It carries enough for a caller to tell a verified node apart
// from one that is still converging or reporting a different policy, instead of
// a mode-blind bool.
type flexNodeStatus struct {
	// name is the node name.
	name string
	// hasMachine is false when the node has no agent-backed Machine yet; the
	// operator delivers the policy to the Machine as it appears.
	hasMachine bool
	// ready is true when the Machine reports the expected healthy condition for
	// the desired policy: the reason matches the mode (Configured for managed,
	// External for external), the condition observed at or after the current
	// generation, and the delivered Machine.spec.registryAuth matches the desired
	// policy.
	ready bool
	// terminal is true when the Machine reports a condition that will not resolve
	// by re-converging (a False condition): the wait surfaces it immediately
	// instead of polling to the timeout. A not-ready node that is still
	// converging (no Machine yet, no condition yet, a stale or mismatched
	// reason, an undelivered policy) is not terminal and is retried.
	terminal bool
	// detail is a human-readable explanation when not ready.
	detail string
}

// machineStatusReader reports whether the registry-auth credential provider has
// converged on the cluster's Flex nodes by reading the node-reported
// Machine.Status conditions the agent writes. It is injected in tests; the
// default reads Machines through a controller-runtime client.
type machineStatusReader interface {
	// flexMachines returns, for each eligible Linux AKS Flex node, a structured
	// status against the desired policy: whether it has an agent-backed Machine,
	// and whether that Machine reports the expected healthy condition for the
	// policy being delivered.
	flexMachines(ctx context.Context, policy *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error)
}

type machineStatusClient struct{ client client.Client }

// flexMachines reads the node-reported convergence state for the desired policy.
// It associates each eligible Flex node to its Machine via Machine.NodeName()
// (the field the operator's delivery plan indexes on, not the Machine name) and
// batches the observation: one bounded List of Machines plus an in-memory map,
// not a direct GET per node on every poll.
func (m machineStatusClient) flexMachines(ctx context.Context, policy *v1alpha3.RegistryAuthPolicy) (map[string]flexNodeStatus, error) {
	nodes := &corev1.NodeList{}
	if err := m.client.List(ctx, nodes, client.MatchingLabels{
		"kubernetes.io/os": "linux", registryauth.FlexNodeLabel: registryauth.FlexNodeValue,
	}); err != nil {
		return nil, err
	}

	machines := &v1alpha3.MachineList{}
	if err := m.client.List(ctx, machines); err != nil {
		return nil, err
	}

	byNode := make(map[string]*v1alpha3.Machine, len(machines.Items))
	for i := range machines.Items {
		byNode[machines.Items[i].NodeName()] = &machines.Items[i]
	}

	wantReason := "External"
	if policy != nil {
		wantReason = "Configured"
	}

	statuses := make(map[string]flexNodeStatus, len(nodes.Items))
	for i := range nodes.Items {
		name := nodes.Items[i].Name

		machine, ok := byNode[name]

		if !ok {
			// No agent-backed Machine yet: the operator delivers the policy to
			// the Machine as it appears. The node is not yet verifiable.
			statuses[name] = flexNodeStatus{name: name, hasMachine: false, detail: "no agent-backed Machine registered yet"}

			continue
		}

		statuses[name] = flexMachineStatus(machine, policy, wantReason)
	}

	return statuses, nil
}

// flexMachineStatus evaluates one Machine's node-reported RegistryAuth condition
// against the desired policy, returning a structured status. The expected
// healthy state requires the reason to match the mode, the condition to have
// observed at or after the current generation, and the delivered
// Machine.spec.registryAuth to equal the desired policy (a True from a prior
// policy, or a condition observed before the current policy was written, is not
// readiness).
func flexMachineStatus(machine *v1alpha3.Machine, policy *v1alpha3.RegistryAuthPolicy, wantReason string) flexNodeStatus {
	status := flexNodeStatus{name: machine.Name, hasMachine: true}

	cond := apimeta.FindStatusCondition(machine.Status.Conditions, v1alpha3.MachineConditionRegistryAuth)
	if cond == nil {
		status.detail = "no registry-auth condition reported yet"

		return status
	}

	if cond.Status != metav1.ConditionTrue {
		// A False condition is not automatically terminal. The agent reports a
		// False reason while it is scheduling a bounded retry (RestartBackoff,
		// RestartFailed, ApplyFailed, VerifyFailed, VerificationFailed,
		// LedgerWriteFailed, BudgetResetFailed); the wait must keep polling for
		// that to converge. Only RestartBudgetExhausted means no retry is left
		// for the CURRENT policy generation, and only a verdict observed at or
		// after the current generation applies: a stale failure from a prior
		// generation is not terminal, because a new policy or detach resets the
		// restart budget. Additionally, the verdict must be for the policy
		// actually being delivered: a failure current for the Machine's OLD
		// spec is not authority for a different requested policy that has not
		// yet been patched onto the spec. Surface a current terminal
		// exhaustion immediately only when the delivered spec matches the
		// desired policy.
		if cond.Reason == registryauth.ConditionReasonRestartBudgetExhausted &&
			cond.ObservedGeneration >= machine.Generation &&
			registryAuthSpecEquals(machine.Spec.RegistryAuth, policy) {
			status.terminal = true
		}

		status.detail = fmt.Sprintf("condition %s: %s (%s)", cond.Status, cond.Reason, cond.Message)

		return status
	}

	if cond.ObservedGeneration < machine.Generation {
		status.detail = fmt.Sprintf("condition observed generation %d lags machine generation %d", cond.ObservedGeneration, machine.Generation)

		return status
	}

	if cond.Reason != wantReason {
		status.detail = fmt.Sprintf("condition reports %s, expected %s for this policy", cond.Reason, wantReason)

		return status
	}

	if !registryAuthSpecEquals(machine.Spec.RegistryAuth, policy) {
		status.detail = "delivered policy does not match the desired policy"

		return status
	}

	status.ready = true

	return status
}

// registryAuthSpecEquals reports whether two Machine registry-auth policies are
// equal (both nil counts as a match).
func registryAuthSpecEquals(a, b *v1alpha3.RegistryAuthPolicy) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	return *a == *b
}

// registryAuthPolicyFromConfig maps a Gantry node-auth config to the
// Machine.spec.registryAuth field the operator delivers. A non-managed mode maps
// to nil: the agent then leaves the kubelet's authentication untouched.
func registryAuthPolicyFromConfig(cfg registryauth.Config) *v1alpha3.RegistryAuthPolicy {
	if cfg.Mode != registryauth.IdentityBindingMode {
		return nil
	}

	return &v1alpha3.RegistryAuthPolicy{
		Mode:         cfg.Mode,
		Registry:     cfg.Registry,
		ClientID:     cfg.ClientID,
		TenantID:     cfg.TenantID,
		SNIName:      cfg.SNIName,
		ProxyAddress: cfg.ProxyAddress,
		Audience:     cfg.Audience,
		CACert:       cfg.CACert,
	}
}

func gantryCommandGroup() *cobra.Command {
	group := &cobra.Command{Use: "gantry", Short: "Configure Gantry image acceleration"}
	h := &attachACRHandler{}
	cmd := &cobra.Command{
		Use: "attach-acr", Short: "Attach an existing ACR without changing authentication by default",
		Long: "Add an ACR upstream without replacing unrelated Gantry configuration. Authentication is externally managed by default and remains untouched. Opt in with --auth-mode aks-identity-binding to enable the stock ACR credential provider on current and future Linux Flex nodes, regardless of compute location. ACR and AKS may be in different subscriptions within the same tenant; the Azure CLI caller needs permissions in both subscriptions.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { h.out = cmd.OutOrStdout(); return h.execute(cmd.Context()) },
	}
	cmd.Flags().StringVar(&h.acrID, "acr-resource-id", "", "Existing ACR Azure resource ID")
	cmd.Flags().StringVar(&h.authMode, "auth-mode", registryauth.ExternalMode, "external preserves authentication; aks-identity-binding explicitly enables managed authentication")
	cmd.Flags().StringVar(&h.clusterID, "cluster-resource-id", "", "AKS resource ID (required for aks-identity-binding)")
	cmd.Flags().StringVar(&h.namespace, "namespace", unbounded.SystemNamespace(), "Operator and Gantry namespace")
	cmd.Flags().StringVar(&h.kubeconfig, "kubeconfig", "", "Kubeconfig path")
	cmd.Flags().DurationVar(&h.timeout, "timeout", 10*time.Minute, "Setup and node configuration timeout")

	if err := cmd.MarkFlagRequired("acr-resource-id"); err != nil {
		panic(err)
	}

	group.AddCommand(cmd)
	group.AddCommand(gantryDetachACRCommand())

	return group
}

// gantryDetachACRCommand reverts managed ACR authentication to external mode.
// It flips the node-auth policy to mode: external so the operator clears
// Machine.spec.registryAuth and the agent removes the managed credential
// provider from each node's kubelet, then waits for the fleet to report
// convergence. Azure-side identities and grants are retained by default;
// --remove-azure deletes the operator-owned pull grant for the cluster.
func gantryDetachACRCommand() *cobra.Command {
	h := &detachACRHandler{}

	cmd := &cobra.Command{
		Use:   "detach-acr",
		Short: "Turn off managed ACR authentication and let nodes converge to external",
		Long:  "Revert managed ACR authentication (attach-acr --auth-mode aks-identity-binding) to external. The node-auth policy is set to mode: external, the operator clears Machine.spec.registryAuth on the affected Flex nodes, and each node's agent removes the managed credential provider and reports convergence. Azure identities and the cluster pull grant are retained by default; use --remove-azure to delete the operator-owned pull grant. With --remove-azure, both --cluster-resource-id and --acr-resource-id are required (the ACR grant may live in a different subscription than the AKS cluster) and are validated before the policy is changed.",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { h.out = cmd.OutOrStdout(); return h.execute(cmd.Context()) },
	}
	cmd.Flags().StringVar(&h.namespace, "namespace", unbounded.SystemNamespace(), "Operator and Gantry namespace")
	cmd.Flags().StringVar(&h.kubeconfig, "kubeconfig", "", "Kubeconfig path")
	cmd.Flags().StringVar(&h.clusterID, "cluster-resource-id", "", "AKS resource ID (required with --remove-azure)")
	cmd.Flags().StringVar(&h.acrID, "acr-resource-id", "", "ACR Azure resource ID whose pull grant to reverse (required with --remove-azure); the grant may live in a different subscription than the AKS cluster")
	cmd.Flags().DurationVar(&h.timeout, "timeout", 10*time.Minute, "Convergence timeout")
	cmd.Flags().BoolVar(&h.removeAzure, "remove-azure", false, "Delete the operator-owned cluster pull grant and Azure identity")

	return cmd
}

// defaultAzureCLI runs the Azure CLI for a subscription, returning the JSON
// output. Both the attach and detach handlers use it; tests inject a fake.
func defaultAzureCLI(ctx context.Context, subscriptionID string, args ...string) ([]byte, error) {
	args = append(args, "--subscription", subscriptionID, "--only-show-errors", "--output", "json")
	cmd := exec.CommandContext(ctx, "az", args...)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("az %s: %w: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(stderr.String()))
	}

	return output, nil
}

func azureResourceID(id, resourceType string) (*arm.ResourceID, error) {
	parsed, err := arm.ParseResourceID(id)
	if err != nil {
		return nil, fmt.Errorf("invalid Azure resource ID: %w", err)
	}

	sub, err := uuid.Parse(parsed.SubscriptionID)
	if err != nil || sub == uuid.Nil || parsed.ResourceGroupName == "" ||
		!strings.EqualFold(parsed.ResourceType.String(), resourceType) {
		return nil, fmt.Errorf("expected a subscription-scoped %s resource ID", resourceType)
	}

	return parsed, nil
}

func (h *attachACRHandler) execute(ctx context.Context) error {
	if h.authMode == "" {
		h.authMode = registryauth.ExternalMode
	}

	if h.authMode != registryauth.ExternalMode && h.authMode != registryauth.IdentityBindingMode {
		return errors.New("--auth-mode must be external or aks-identity-binding")
	}

	acrID, err := azureResourceID(h.acrID, "Microsoft.ContainerRegistry/registries")
	if err != nil {
		return err
	}

	if h.timeout <= 0 {
		return errors.New("--timeout must be positive")
	}

	if h.authMode == registryauth.IdentityBindingMode {
		if _, err := azureResourceID(h.clusterID, "Microsoft.ContainerService/managedClusters"); err != nil {
			return fmt.Errorf("--cluster-resource-id is required for managed authentication: %w", err)
		}

		if _, err := fmt.Fprintln(h.out, "WARNING: managed ACR authentication may restart kubelets on all targeted Linux Flex nodes concurrently. Nodes are not drained automatically; you are responsible for the impact on existing workloads."); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	if h.client == nil {
		h.client, h.restConfig, err = kube.ClientAndConfigFromFile(getKubeconfigPath(h.kubeconfig))
		if err != nil {
			return err
		}
	}

	// The Machine client is only needed in managed mode, where waitForACRInstallation
	// polls node-reported readiness. In external/default mode it is unused;
	// initializing it unconditionally would require a valid kubeconfig even for
	// the external path, which should not depend on Machine CRD access.
	if h.authMode == registryauth.IdentityBindingMode && h.machineClient == nil {
		c, err := newMachineClientWithKubeconfig(h.kubeconfig)
		if err != nil {
			return fmt.Errorf("create Machine client: %w", err)
		}

		h.machineClient = machineStatusClient{client: c}
	}

	if h.runAzure == nil {
		h.runAzure = defaultAzureCLI
	}

	source, err := h.client.CoreV1().ConfigMaps(h.namespace).Get(ctx, "gantry-config", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read gantry-config; install operator-managed Gantry first: %w", err)
	}

	var registry struct{ ID, LoginServer, RoleAssignmentMode string }
	if err := h.azureJSON(ctx, acrID.SubscriptionID, &registry, "acr", "show", "--name", acrID.Name, "--resource-group", acrID.ResourceGroupName); err != nil {
		return err
	}

	if !strings.EqualFold(registry.ID, h.acrID) || !registryauth.IsACR(registry.LoginServer) {
		return errors.New("ACR response did not identify the requested public-cloud registry")
	}

	cfg := registryauth.Config{Mode: h.authMode, Registry: registry.LoginServer}

	policy, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	attached, _, err := registryauth.EnsureUpstream(source.Data["config.yaml"], string(policy))
	if err != nil {
		return err
	}

	originalPolicy := source.Data[registryauth.ConfigKey]
	if h.authMode == registryauth.IdentityBindingMode {
		if originalPolicy != "" {
			current, err := registryauth.Derive(attached, originalPolicy)
			if err != nil {
				return fmt.Errorf("existing authentication policy must be resolved explicitly: %w", err)
			}

			// Managed opt-in is permitted from a known external policy (one the
			// attach/detach flow wrote, i.e. mode: external with no managed
			// identity) so attach -> detach -> attach works, and from the same
			// managed policy. A conflicting or customer-owned policy of any
			// other shape is refused rather than silently replaced.
			switch current.Mode {
			case registryauth.ExternalMode:
				// Known external: no managed identity to preserve.
			case registryauth.IdentityBindingMode:
				if current.Registry != cfg.Registry {
					return errors.New("refusing to replace an existing authentication policy for another registry; resolve it explicitly before opting in")
				}

				cfg.ClientID, cfg.TenantID = current.ClientID, current.TenantID
			default:
				return errors.New("refusing to replace an existing authentication policy; resolve it explicitly before opting in")
			}
		}

		if err := h.configureIdentityBinding(ctx, acrID, registry.RoleAssignmentMode, &cfg); err != nil {
			return err
		}

		policy, err = yaml.Marshal(cfg)
		if err != nil {
			return err
		}
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := h.client.CoreV1().ConfigMaps(h.namespace).Get(ctx, source.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if h.authMode == registryauth.IdentityBindingMode && current.Data[registryauth.ConfigKey] != originalPolicy {
			return errors.New("authentication policy changed concurrently; refusing to overwrite it")
		}

		data, _, err := registryauth.EnsureUpstream(current.Data["config.yaml"], string(policy))
		if err != nil {
			return err
		}

		if h.authMode == registryauth.IdentityBindingMode {
			if _, err := registryauth.Derive(data, string(policy)); err != nil {
				return fmt.Errorf("validate Gantry attachment policy: %w", err)
			}
		}

		current.Data["config.yaml"] = data
		if h.authMode == registryauth.IdentityBindingMode {
			current.Data[registryauth.ConfigKey] = string(policy)
		}

		_, err = h.client.CoreV1().ConfigMaps(h.namespace).Update(ctx, current, metav1.UpdateOptions{})

		return err
	}); err != nil {
		return fmt.Errorf("save Gantry attachment policy: %w", err)
	}

	if h.authMode == registryauth.IdentityBindingMode {
		if err := h.acknowledgeIdentityUseGrant(ctx, &cfg); err != nil {
			return fmt.Errorf("policy saved, but the identity-use grant was not acknowledged: %w", err)
		}

		if err := h.waitForACRInstallation(ctx, &cfg); err != nil {
			return fmt.Errorf("policy saved, but stock provider installation is not ready: %w", err)
		}
	}

	if h.out != nil {
		status := "existing authentication unchanged"
		if h.authMode == registryauth.IdentityBindingMode {
			status = "stock ACR credential provider configured"
		}

		_, err = fmt.Fprintf(h.out, "Attached %s to Gantry (%s). Verify a private-image pull and Gantry acceleration separately.\n", cfg.Registry, status)
	}

	return err
}

func (h *attachACRHandler) azureJSON(ctx context.Context, subscriptionID string, output any, args ...string) error {
	data, err := h.runAzure(ctx, subscriptionID, args...)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("invalid Azure response for %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}

	return nil
}

func (h *attachACRHandler) waitForACRProxy(ctx context.Context, cfg *registryauth.Config) error {
	var lastErr error

	err := wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		candidate := *cfg
		candidate.SNIName, candidate.ProxyAddress, candidate.CACert, candidate.Audience = "", "", "", ""

		lastErr = h.discoverACRProxy(ctx, &candidate)
		if lastErr == nil {
			*cfg = candidate

			return true, nil
		}

		if errors.Is(lastErr, errACRProxyNotReady) || apierrors.IsServiceUnavailable(lastErr) ||
			apierrors.IsTimeout(lastErr) || apierrors.IsServerTimeout(lastErr) ||
			apierrors.IsTooManyRequests(lastErr) || apierrors.IsInternalError(lastErr) || apierrors.IsNotFound(lastErr) {
			return false, nil
		}

		return false, lastErr
	})
	if err != nil {
		return fmt.Errorf("wait for AKS identity-binding admission: %w", errors.Join(err, lastErr))
	}

	return nil
}

func (h *attachACRHandler) discoverACRProxy(ctx context.Context, cfg *registryauth.Config) error {
	accounts := h.client.CoreV1().ServiceAccounts(h.namespace)
	name := "gantry-acr-discovery"

	account, err := accounts.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		account = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: h.namespace,
			Labels:      map[string]string{"app.kubernetes.io/managed-by": acrAttachManager},
			Annotations: map[string]string{"azure.workload.identity/client-id": cfg.ClientID, "azure.workload.identity/tenant-id": cfg.TenantID},
		}}
		if _, err := accounts.Create(ctx, account, metav1.CreateOptions{}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if account.Labels["app.kubernetes.io/managed-by"] != acrAttachManager {
		// A foreign (unowned) discovery ServiceAccount is refused, never
		// changed.
		return errors.New("refusing to change an unowned ACR discovery ServiceAccount")
	} else if account.Annotations["azure.workload.identity/client-id"] != cfg.ClientID ||
		account.Annotations["azure.workload.identity/tenant-id"] != cfg.TenantID {
		// An owned discovery ServiceAccount carrying a different identity is
		// the reattach-with-a-fresh-identity case: --remove-azure deleted the
		// old UAMI, so a re-attach mints a new client ID. Safely update the
		// owned account's identity annotations in place rather than requiring
		// the user to delete it first. A truly foreign account is refused above.
		if account.Annotations == nil {
			account.Annotations = map[string]string{}
		}

		account.Annotations["azure.workload.identity/client-id"] = cfg.ClientID
		account.Annotations["azure.workload.identity/tenant-id"] = cfg.TenantID

		if _, err := accounts.Update(ctx, account, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update owned ACR discovery ServiceAccount to the new identity: %w", err)
		}
	}

	pod, err := h.client.CoreV1().Pods(h.namespace).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "gantry-acr-discovery-", Labels: map[string]string{"azure.workload.identity/use": "true"},
			Annotations: map[string]string{"azure.workload.identity/use-identity-binding": "true"},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: name, RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{Name: "discover", Image: "registry.k8s.io/pause:3.10"}},
		},
	}, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		return fmt.Errorf("discover AKS-managed token proxy through admission dry-run: %w", err)
	}

	values := map[string]string{}

	for _, container := range pod.Spec.Containers {
		if container.Name == "discover" {
			for _, env := range container.Env {
				values[env.Name] = env.Value
			}
		}
	}

	cfg.SNIName = values["AZURE_KUBERNETES_SNI_NAME"]
	if data := values["AZURE_KUBERNETES_CA_DATA"]; data != "" {
		if strings.Contains(data, "-----BEGIN CERTIFICATE-----") {
			cfg.CACert = data
		} else {
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				return fmt.Errorf("decode AKS-injected token proxy CA: %w", err)
			}

			cfg.CACert = string(decoded)
		}
	}

	if values["AZURE_KUBERNETES_TOKEN_PROXY"] == "" {
		return errACRProxyNotReady
	}

	proxy, err := url.Parse(values["AZURE_KUBERNETES_TOKEN_PROXY"])
	if err != nil || proxy.Scheme != "https" || proxy.Hostname() == "" || proxy.User != nil ||
		(proxy.Port() != "" && proxy.Port() != "443") || (proxy.Path != "" && proxy.Path != "/") || proxy.RawQuery != "" || proxy.Fragment != "" {
		return errors.New("AKS admission did not expose a supported identity-binding token proxy; verify feature availability")
	}

	cfg.ProxyAddress = proxy.Hostname()
	tokenPath := values["AZURE_FEDERATED_TOKEN_FILE"]
	caPath := values["AZURE_KUBERNETES_CA_FILE"]

	for _, container := range pod.Spec.Containers {
		if container.Name != "discover" {
			continue
		}

		for _, mount := range container.VolumeMounts {
			for _, volume := range pod.Spec.Volumes {
				if volume.Name != mount.Name {
					continue
				}

				if volume.Projected != nil {
					for _, source := range volume.Projected.Sources {
						if source.ServiceAccountToken != nil && mount.MountPath+"/"+source.ServiceAccountToken.Path == tokenPath {
							cfg.Audience = source.ServiceAccountToken.Audience
						}

						if source.ConfigMap != nil {
							for _, item := range source.ConfigMap.Items {
								if mount.MountPath+"/"+item.Path == caPath {
									cm, err := h.client.CoreV1().ConfigMaps(h.namespace).Get(ctx, source.ConfigMap.Name, metav1.GetOptions{})
									if err != nil {
										return err
									}

									cfg.CACert = cm.Data[item.Key]
								}
							}
						}

						if source.ClusterTrustBundle != nil && mount.MountPath+"/"+source.ClusterTrustBundle.Path == caPath {
							selector, err := metav1.LabelSelectorAsSelector(source.ClusterTrustBundle.LabelSelector)
							if err != nil {
								return err
							}

							bundles, err := h.client.CertificatesV1beta1().ClusterTrustBundles().List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
							if err != nil {
								return err
							}

							var certificates []string

							for _, bundle := range bundles.Items {
								projection := source.ClusterTrustBundle
								if projection.Name != nil && bundle.Name == *projection.Name ||
									projection.SignerName != nil && bundle.Spec.SignerName == *projection.SignerName {
									certificates = append(certificates, bundle.Spec.TrustBundle)
								}
							}

							cfg.CACert = strings.Join(certificates, "\n")
						}
					}
				}

				if volume.ConfigMap != nil {
					for _, item := range volume.ConfigMap.Items {
						if mount.MountPath+"/"+item.Path == caPath {
							cm, err := h.client.CoreV1().ConfigMaps(h.namespace).Get(ctx, volume.ConfigMap.Name, metav1.GetOptions{})
							if err != nil {
								return err
							}

							cfg.CACert = cm.Data[item.Key]
						}
					}
				}
			}
		}
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("incomplete AKS identity-binding admission metadata: %w", err)
	}

	return nil
}

// waitForACRInstallation blocks until every agent-backed Flex node reports the
// managed credential provider configured and verified on its kubelet, as
// reflected in the node-reported Machine.Status registry-auth condition the
// operator delivers policy to. A node still converging (no Machine yet, no
// condition yet, a stale or mismatched reason, or an undelivered policy) is
// retried until the timeout; a node reporting a terminal failure surfaces that
// condition's message. A node reporting the external policy (or a stale
// Configured from a prior revision) is not counted as converged for this
// managed attach.
func (h *attachACRHandler) waitForACRInstallation(ctx context.Context, cfg *registryauth.Config) error {
	if h.machineClient == nil {
		return errors.New("machine status reader is not configured")
	}

	policy := registryAuthPolicyFromConfig(*cfg)

	var lastError error

	err := wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		statuses, err := h.machineClient.flexMachines(ctx, policy)
		if err != nil {
			return false, err
		}

		// A genuinely empty eligible fleet is the only no-node case: the policy
		// is delivered to Machines as they appear, so there is nothing to wait
		// on. Every eligible node that is present but not converged is checked,
		// including a node with no Machine yet (the reader deliberately retains
		// it as an eligible unverified node) and a node reporting a terminal
		// failure.
		for name, st := range statuses {
			if st.terminal {
				// A terminal failure is surfaced immediately, not polled to the
				// timeout: the node reported the policy will not converge.
				return false, fmt.Errorf("flex node %s reports a terminal failure: %s", name, st.detail)
			}

			if !st.ready {
				// Not converged: a node with no Machine is still installing, and
				// a node with a Machine that reports a stale or mismatched
				// condition is still converging. Both are retried until the
				// timeout.
				lastError = fmt.Errorf("flex node %s has not converged to the managed credential provider: %s", name, st.detail)

				return false, nil
			}
		}

		return true, nil
	})
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("timed out waiting for Flex nodes to verify the managed credential provider; last: %v", lastError)
		}

		return errors.Join(err, lastError)
	}

	return nil
}

// acknowledgeIdentityUseGrant waits for the operator's audience reconciler to
// publish the identity-use grant (ClusterRole and ClusterRoleBinding the nodes
// use to use the managed identity) for the current policy, and confirms it is
// owned by this installation and matches the current policy hash. This
// restores the managed-attach acknowledgment that the grant actually backs the
// policy that was just written: a grant that is unowned or stamped with a
// different policy hash is a race or a customer-owned grant and is not
// acknowledged as ready. Absence is retried (the operator publishes it); a
// present-but-mismatched grant is an error, not a silent success.
func (h *attachACRHandler) acknowledgeIdentityUseGrant(ctx context.Context, cfg *registryauth.Config) error {
	payload, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])

	var lastError error

	err = wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		binding, err := h.client.RbacV1().ClusterRoleBindings().Get(ctx, registryauth.IdentityRoleName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			lastError = fmt.Errorf("the identity-use grant %q is not yet published", registryauth.IdentityRoleName)

			return false, nil
		}

		if err != nil {
			return false, err
		}

		if binding.Labels["app.kubernetes.io/managed-by"] != acrAttachManager || binding.Labels["unbounded-cloud.io/namespace"] != h.namespace {
			lastError = fmt.Errorf("%w: %q", errUnownedGrant, registryauth.IdentityRoleName)

			// An unowned grant will not become owned by a re-publish; report it
			// as a terminal failure rather than polling until the deadline.
			return false, lastError
		}

		// The grant is owned and present. The policy hash is acknowledged when
		// it matches; a mismatch is reported (a stale grant from a prior
		// policy, e.g. during attach -> detach -> attach) and retried, while a
		// missing hash is accepted because the operator stamps it on re-publish.
		switch binding.Annotations[registryAuthConfigHashKey] {
		case "":
			// Not yet stamped; the operator will (re)publish it.
			lastError = fmt.Errorf("the identity-use grant %q is not yet stamped with the policy hash", registryauth.IdentityRoleName)

			return false, nil
		case hash:
			return true, nil
		default:
			lastError = fmt.Errorf("the identity-use grant %q carries a stale policy hash; waiting for the operator to reconcile the current policy", registryauth.IdentityRoleName)

			return false, nil
		}
	})
	if err != nil {
		if errors.Is(lastError, errUnownedGrant) {
			return lastError
		}

		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("timed out waiting for the operator to acknowledge the identity-use grant; last: %v", lastError)
		}

		return errors.Join(err, lastError)
	}

	return nil
}

func (h *attachACRHandler) checkFlexVersions(ctx context.Context) error {
	nodes, err := h.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: registryauth.FlexNodeLabel + "=" + registryauth.FlexNodeValue})
	if err != nil {
		return err
	}

	for _, node := range nodes.Items {
		if node.Labels["kubernetes.io/os"] != "linux" {
			return fmt.Errorf("managed ACR authentication currently supports Linux Flex nodes only: %s", node.Name)
		}

		v, err := version.ParseGeneric(node.Status.NodeInfo.KubeletVersion)
		if err != nil || !v.AtLeast(version.MustParseGeneric("1.34.0")) {
			return fmt.Errorf("flex node %s requires kubelet 1.34 or later for identity-binding image pulls", node.Name)
		}
	}

	return nil
}
