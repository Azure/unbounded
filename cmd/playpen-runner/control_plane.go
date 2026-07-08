// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/playpen/meta"
)

type controlPlaneConfig struct {
	kubeconfig        string
	guestServer       string
	kubernetesVersion string
	podName           string
	podNamespace      string
}

func newControlPlaneCommand() *cobra.Command {
	cfg := controlPlaneConfig{podName: os.Getenv("POD_NAME"), podNamespace: os.Getenv("POD_NAMESPACE")}

	cmd := &cobra.Command{
		Use:   "control-plane",
		Short: "Publish pooled k3s control-plane allocation metadata",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runControlPlane(cmd.Context(), cfg)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&cfg.kubeconfig, "kubeconfig", "/etc/rancher/k3s/k3s.yaml", "k3s admin kubeconfig path")
	flags.StringVar(&cfg.guestServer, "guest-server", "", "API server URL reachable from allocated playpen guests")
	flags.StringVar(&cfg.kubernetesVersion, "kubernetes-version", "", "Kubernetes version served by this control plane")

	return cmd
}

func runControlPlane(ctx context.Context, cfg controlPlaneConfig) error {
	if strings.TrimSpace(cfg.podName) == "" || strings.TrimSpace(cfg.podNamespace) == "" {
		return fmt.Errorf("POD_NAME and POD_NAMESPACE are required")
	}

	if strings.TrimSpace(cfg.guestServer) == "" {
		return fmt.Errorf("guest server is required")
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))

	parentClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create parent Kubernetes client: %w", err)
	}

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		if err := publishControlPlaneOnce(ctx, parentClient, cfg); err != nil {
			slog.Warn("control-plane metadata publish failed", "error", err)
		} else {
			slog.Info("control-plane metadata published")
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func publishControlPlaneOnce(ctx context.Context, parentClient client.Client, cfg controlPlaneConfig) error {
	rawKubeconfig, err := os.ReadFile(cfg.kubeconfig)
	if err != nil {
		return fmt.Errorf("read k3s kubeconfig: %w", err)
	}

	childConfig, err := clientcmd.RESTConfigFromKubeConfig(rawKubeconfig)
	if err != nil {
		return fmt.Errorf("build child REST config: %w", err)
	}

	childClient, err := kubernetes.NewForConfig(childConfig)
	if err != nil {
		return fmt.Errorf("create child Kubernetes client: %w", err)
	}

	if _, err := childClient.Discovery().ServerVersion(); err != nil {
		return fmt.Errorf("child API server is not ready: %w", err)
	}

	guestKubeconfig, err := rewriteKubeconfigServer(rawKubeconfig, cfg.guestServer, "")
	if err != nil {
		return err
	}

	if err := ensureKubeDNS(ctx, childClient); err != nil {
		return err
	}

	if err := ensureClusterInfo(ctx, childClient, guestKubeconfig); err != nil {
		return err
	}

	pod := &corev1.Pod{}

	key := types.NamespacedName{Namespace: cfg.podNamespace, Name: cfg.podName}
	if err := parentClient.Get(ctx, key, pod); err != nil {
		return fmt.Errorf("get parent pod: %w", err)
	}

	base := pod.DeepCopy()
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}

	pod.Annotations[meta.AnnotationControlPlaneKubeconfig] = string(rawKubeconfig)
	pod.Annotations[meta.AnnotationControlPlaneGuestServer] = cfg.guestServer

	if err := parentClient.Patch(ctx, pod, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("patch parent pod annotations: %w", err)
	}

	return nil
}

func ensureKubeDNS(ctx context.Context, childClient kubernetes.Interface) error {
	services := childClient.CoreV1().Services("kube-system")

	_, err := services.Get(ctx, "kube-dns", metav1.GetOptions{})
	if err == nil {
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get kube-dns service: %w", err)
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system"},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{Name: "dns", Protocol: corev1.ProtocolUDP, Port: 53},
				{Name: "dns-tcp", Protocol: corev1.ProtocolTCP, Port: 53},
			},
			Selector: map[string]string{"k8s-app": "kube-dns"},
		},
	}

	if _, err := services.Create(ctx, service, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create kube-dns service: %w", err)
	}

	return nil
}

func ensureClusterInfo(ctx context.Context, childClient kubernetes.Interface, kubeconfig string) error {
	configMaps := childClient.CoreV1().ConfigMaps("kube-public")

	current, err := configMaps.Get(ctx, "cluster-info", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = configMaps.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-info", Namespace: "kube-public"},
			Data:       map[string]string{"kubeconfig": kubeconfig},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create cluster-info ConfigMap: %w", err)
		}

		return nil
	}

	if err != nil {
		return fmt.Errorf("get cluster-info ConfigMap: %w", err)
	}

	if current.Data == nil {
		current.Data = map[string]string{}
	}

	if current.Data["kubeconfig"] == kubeconfig {
		return nil
	}

	current.Data["kubeconfig"] = kubeconfig
	if _, err := configMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update cluster-info ConfigMap: %w", err)
	}

	return nil
}

func rewriteKubeconfigServer(raw []byte, server, tlsServerName string) (string, error) {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return "", fmt.Errorf("parse kubeconfig: %w", err)
	}

	for _, cluster := range cfg.Clusters {
		cluster.Server = server
		cluster.TLSServerName = tlsServerName
	}

	data, err := clientcmd.Write(*cfg)
	if err != nil {
		return "", fmt.Errorf("write kubeconfig: %w", err)
	}

	return string(data), nil
}
