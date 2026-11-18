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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/internal/kube"
	"github.com/Azure/unbounded/internal/registryauth"
	"github.com/Azure/unbounded/internal/unbounded"
)

const acrAttachManager = "unbounded-acr-attach"

var errACRProxyNotReady = errors.New("AKS admission did not expose a supported identity-binding token proxy; verify feature availability")

type attachACRHandler struct {
	acrID, clusterID, authMode, namespace, kubeconfig string
	timeout                                           time.Duration
	client                                            kubernetes.Interface
	restConfig                                        *rest.Config
	runAzure                                          func(context.Context, string, ...string) ([]byte, error)
	out                                               io.Writer
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

	return group
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

	if h.runAzure == nil {
		h.runAzure = func(ctx context.Context, subscriptionID string, args ...string) ([]byte, error) {
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

			if current.Mode != registryauth.IdentityBindingMode || current.Registry != cfg.Registry {
				return errors.New("refusing to replace an existing authentication policy; resolve it explicitly before opting in")
			}

			cfg.ClientID, cfg.TenantID = current.ClientID, current.TenantID
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
		if err := h.waitForACRInstallation(ctx, cfg); err != nil {
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
	} else if account.Labels["app.kubernetes.io/managed-by"] != acrAttachManager ||
		account.Annotations["azure.workload.identity/client-id"] != cfg.ClientID ||
		account.Annotations["azure.workload.identity/tenant-id"] != cfg.TenantID {
		return errors.New("refusing to change an unowned or conflicting ACR discovery ServiceAccount")
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

func (h *attachACRHandler) waitForACRInstallation(ctx context.Context, cfg registryauth.Config) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}

	hash := sha256.Sum256(data)
	expected := hex.EncodeToString(hash[:])

	for {
		ds, err := h.client.AppsV1().DaemonSets(h.namespace).Get(ctx, "gantry-acr-auth", metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}

		if err == nil && ds.Spec.Template.Annotations["unbounded-cloud.io/registry-auth-config-hash"] == expected && acrInstallerReady(ds) {
			binding, err := h.client.RbacV1().ClusterRoleBindings().Get(ctx, registryauth.IdentityRoleName, metav1.GetOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}

			if err == nil && binding.Annotations["unbounded-cloud.io/registry-auth-config-hash"] == expected &&
				binding.Labels["app.kubernetes.io/managed-by"] == acrAttachManager &&
				binding.Labels["unbounded-cloud.io/namespace"] == h.namespace {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func acrInstallerReady(ds *appsv1.DaemonSet) bool {
	return ds.Status.ObservedGeneration >= ds.Generation &&
		ds.Status.UpdatedNumberScheduled == ds.Status.DesiredNumberScheduled &&
		ds.Status.NumberAvailable == ds.Status.DesiredNumberScheduled
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
