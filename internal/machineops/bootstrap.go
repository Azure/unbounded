// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/cloudprovider"
	"github.com/Azure/unbounded/internal/provision"
)

// ClusterInfo holds the cluster-level data needed to build host replacement cloud-init.
type ClusterInfo struct {
	APIServer      string
	CACertBase64   string
	ClusterDNS     string
	KubeVersion    string
	ProviderLabels map[string]string
}

// ResolveClusterInfo resolves cluster bootstrap details for HostReplace.
func ResolveClusterInfo(ctx context.Context, apiServerEndpoint string, k kubernetes.Interface) (*ClusterInfo, error) {
	apiServerEndpoint = strings.TrimSpace(apiServerEndpoint)
	if apiServerEndpoint == "" {
		return nil, fmt.Errorf("API server endpoint is required")
	}

	cm, err := k.CoreV1().ConfigMaps(metav1.NamespacePublic).Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get kube-root-ca.crt ConfigMap from kube-public: %w", err)
	}

	caCert, ok := cm.Data["ca.crt"]
	if !ok {
		return nil, fmt.Errorf("ca.crt key not found in kube-root-ca.crt ConfigMap")
	}

	apiServerEndpoint = strings.TrimPrefix(apiServerEndpoint, "https://")

	svc, err := k.CoreV1().Services(metav1.NamespaceSystem).Get(ctx, "kube-dns", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get kube-dns Service from kube-system: %w", err)
	}

	if svc.Spec.ClusterIP == "" {
		return nil, fmt.Errorf("kube-dns Service has no ClusterIP")
	}

	provider, err := cloudprovider.DetectProvider(ctx, k)
	if err != nil {
		return nil, fmt.Errorf("detect provider: %w", err)
	}

	sv, err := k.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("get server version: %w", err)
	}

	info := &ClusterInfo{
		APIServer:    apiServerEndpoint,
		CACertBase64: base64.StdEncoding.EncodeToString([]byte(caCert)),
		ClusterDNS:   svc.Spec.ClusterIP,
		KubeVersion:  sv.GitVersion,
	}
	if provider != nil {
		info.ProviderLabels = provider.DefaultLabels()
	}

	return info, nil
}

func (r *MachineOperationReconciler) buildReplaceUserData(ctx context.Context, machine *unboundedv1alpha3.Machine) (string, error) {
	agentConfig, err := r.buildReplaceAgentConfig(ctx, machine)
	if err != nil {
		return "", err
	}

	userData, err := provision.ReplaceCloudInit(agentConfig, provision.AgentInstallEnv(machine.Spec.Agent))
	if err != nil {
		return "", err
	}

	return userData, nil
}

func (r *MachineOperationReconciler) buildReplaceAgentConfig(ctx context.Context, machine *unboundedv1alpha3.Machine) (provision.UnboundedAgentConfig, error) {
	if machine.Spec.Kubernetes == nil {
		return provision.UnboundedAgentConfig{}, fmt.Errorf("machine spec.kubernetes is required for HostReplace")
	}

	clusterInfo, err := r.clusterInfo(ctx)
	if err != nil {
		return provision.UnboundedAgentConfig{}, err
	}

	bootstrapToken, err := r.getBootstrapToken(ctx, machine.Spec.Kubernetes.BootstrapTokenRef.Name)
	if err != nil {
		return provision.UnboundedAgentConfig{}, err
	}

	return provision.BuildAgentConfig(provision.BuildAgentConfigParams{
		Machine: machine,
		Cluster: provision.ClusterEndpoint{
			APIServer:    clusterInfo.APIServer,
			CACertBase64: clusterInfo.CACertBase64,
			ClusterDNS:   clusterInfo.ClusterDNS,
			KubeVersion:  clusterInfo.KubeVersion,
		},
		ProviderLabels: clusterInfo.ProviderLabels,
		BootstrapToken: bootstrapToken,
		NodeName:       machine.Name,
	}), nil
}

func (r *MachineOperationReconciler) clusterInfo(ctx context.Context) (*ClusterInfo, error) {
	if r.ClusterInfo != nil {
		return r.ClusterInfo, nil
	}

	if r.KubeClient == nil {
		return nil, fmt.Errorf("kubernetes client is required for HostReplace")
	}

	clusterInfo, err := ResolveClusterInfo(ctx, r.APIServerEndpoint, r.KubeClient)
	if err != nil {
		return nil, err
	}

	return clusterInfo, nil
}

func (r *MachineOperationReconciler) getBootstrapToken(ctx context.Context, secretName string) (string, error) {
	if secretName == "" {
		return "", fmt.Errorf("machine spec.kubernetes.bootstrapTokenRef.name is required")
	}

	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: secretName}, &secret); err != nil {
		return "", fmt.Errorf("get bootstrap token secret %s in kube-system: %w", secretName, err)
	}

	tokenID, ok := secret.Data["token-id"]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %s", "token-id", secretName)
	}

	tokenSecret, ok := secret.Data["token-secret"]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %s", "token-secret", secretName)
	}

	return fmt.Sprintf("%s.%s", string(tokenID), string(tokenSecret)), nil
}
