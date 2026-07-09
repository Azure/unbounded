// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	apiregistrationclientset "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	kubevirtv1 "kubevirt.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	"github.com/Azure/unbounded/internal/net/certmanager"
	webhookpkg "github.com/Azure/unbounded/internal/net/webhook"
	"github.com/Azure/unbounded/internal/playpen/kubevirt"
	"github.com/Azure/unbounded/internal/playpen/network"
	"github.com/Azure/unbounded/internal/playpen/server"
	"github.com/Azure/unbounded/internal/version"
	playpenclient "github.com/Azure/unbounded/pkg/playpen"
)

func main() {
	root := &cobra.Command{
		Use:     "playpen",
		Short:   "Metalman KubeVirt test playpen",
		Version: version.Version + " (commit: " + version.GitCommit + ")",
	}
	root.SetVersionTemplate(`{{printf "%s\n" .Version}}`)
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(operatorCmd())
	root.AddCommand(endpointCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

type operatorConfig struct {
	KubeconfigPath        string
	Namespace             string
	ServiceName           string
	Port                  int
	DefaultSite           string
	DefaultGatewayPool    string
	DefaultPodCIDRBase    string
	DefaultVMImage        string
	DefaultNetwork        string
	DefaultSSHKey         string
	HTTPBootContainerDisk string
}

func operatorCmd() *cobra.Command {
	var cfg operatorConfig

	cmd := &cobra.Command{
		Use:   "operator",
		Short: "Run the playpen aggregated API and Redfish server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return runOperator(ctx, cfg)
		},
	}

	cmd.Flags().StringVar(&cfg.KubeconfigPath, "kubeconfig", "", "Path to kubeconfig, defaults to in-cluster config")
	cmd.Flags().StringVar(&cfg.Namespace, "namespace", envOrDefault("POD_NAMESPACE", "unbounded-kube"), "Namespace for playpen VMs and serving certificates")
	cmd.Flags().StringVar(&cfg.ServiceName, "service-name", "playpen", "Kubernetes Service name for Redfish and aggregated API")
	cmd.Flags().IntVar(&cfg.Port, "port", 9443, "HTTPS listen port")
	cmd.Flags().StringVar(&cfg.DefaultSite, "site", "playpen", "Default unbounded-net site for fake playpen Nodes")
	cmd.Flags().StringVar(&cfg.DefaultGatewayPool, "gateway-pool", "", "Preferred unbounded-net GatewayPool for playpen traffic")
	cmd.Flags().StringVar(&cfg.DefaultPodCIDRBase, "pod-cidr-base", "10.241.0.0/16", "CIDR base used to derive per-allocation fake Node pod CIDRs")
	cmd.Flags().StringVar(&cfg.DefaultVMImage, "vm-image", "quay.io/containerdisks/fedora:latest", "Default KubeVirt containerDisk image")
	cmd.Flags().StringVar(&cfg.DefaultNetwork, "network-attachment", "default/playpen-net", "Multus network attachment for the playpen VM L2 interface")
	cmd.Flags().StringVar(&cfg.DefaultSSHKey, "ssh-authorized-key", "", "Optional SSH authorized key injected into allocated VMs")
	cmd.Flags().StringVar(&cfg.HTTPBootContainerDisk, "http-boot-container-disk", "", "Optional helper containerDisk used for one-shot UEFI HTTP boot")

	return cmd
}

func endpointCmd() *cobra.Command {
	var (
		allocationPath string
		namespace      string
		ipCommand      string
		wgCommand      string
		iptables       string
		sysctl         string
	)

	cmd := &cobra.Command{
		Use:   "endpoint",
		Short: "Run the optional privileged playpen data-plane endpoint",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return runEndpoint(ctx, endpointConfig{
				AllocationPath: allocationPath,
				Namespace:      namespace,
				IPCommand:      ipCommand,
				WGCommand:      wgCommand,
				IPTables:       iptables,
				Sysctl:         sysctl,
			})
		},
	}
	cmd.Flags().StringVar(&allocationPath, "allocation", "-", "Allocation JSON path, or - for stdin")
	cmd.Flags().StringVar(&namespace, "netns", "", "Network namespace name, defaults to playpen-<allocationID>")
	cmd.Flags().StringVar(&ipCommand, "ip-command", "ip", "iproute2 command")
	cmd.Flags().StringVar(&wgCommand, "wg-command", "wg", "WireGuard command")
	cmd.Flags().StringVar(&iptables, "iptables-command", "iptables", "iptables command")
	cmd.Flags().StringVar(&sysctl, "sysctl-command", "sysctl", "sysctl command")

	return cmd
}

type endpointConfig struct {
	AllocationPath string
	Namespace      string
	IPCommand      string
	WGCommand      string
	IPTables       string
	Sysctl         string
}

func runEndpoint(ctx context.Context, cfg endpointConfig) error {
	allocation, err := readAllocation(cfg.AllocationPath)
	if err != nil {
		return err
	}

	tunnel, err := allocation.EstablishTunnel(ctx, playpenclient.TunnelOptions{
		Namespace:       cfg.Namespace,
		Setup:           true,
		IPCommand:       cfg.IPCommand,
		WGCommand:       cfg.WGCommand,
		IPTablesCommand: cfg.IPTables,
		SysctlCommand:   cfg.Sysctl,
		AllowEndpoint:   true,
	})
	if err != nil {
		return err
	}
	defer func() { _ = tunnel.Close(context.Background()) }()

	fmt.Fprintf(os.Stderr, "playpen endpoint ready in netns %s for allocation %s\n", tunnel.Namespace, allocation.AllocationID)
	<-ctx.Done()

	return nil
}

func readAllocation(path string) (*playpenclient.Allocation, error) {
	var data []byte
	var err error
	if path == "" || path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read allocation: %w", err)
	}

	var allocation playpenclient.Allocation
	if err := json.Unmarshal(data, &allocation); err != nil {
		return nil, fmt.Errorf("decode allocation: %w", err)
	}
	if allocation.AllocationID == "" {
		return nil, fmt.Errorf("allocationID is required")
	}

	return &allocation, nil
}

func runOperator(ctx context.Context, cfg operatorConfig) error {
	restConfig, err := kubeConfig(cfg.KubeconfigPath)
	if err != nil {
		return err
	}

	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}
	ctrlClient, err := controllerRuntimeClient(restConfig)
	if err != nil {
		return err
	}
	webhookServer, err := webhookpkg.NewServer(kube, restConfig, cfg.Namespace)
	if err != nil {
		return fmt.Errorf("create front-proxy validator: %w", err)
	}

	certMgr := certmanager.NewCertManager(certmanager.Options{
		Clientset:   kube,
		Namespace:   cfg.Namespace,
		ServiceName: cfg.ServiceName,
		SecretName:  "playpen-serving-cert",
		CAConfigMap: "playpen-serving-ca",
	})
	if err := certMgr.EnsureCertificate(ctx); err != nil {
		return fmt.Errorf("ensure serving certificate: %w", err)
	}
	if err := injectAPIServiceCABundle(ctx, restConfig, certMgr.CABundle()); err != nil {
		return err
	}
	go certMgr.RunRotationMonitor(ctx)

	vmMgr := kubevirt.NewManager(ctrlClient, kubevirt.Config{
		Namespace:             cfg.Namespace,
		ServiceName:           cfg.ServiceName,
		ServicePort:           cfg.Port,
		DefaultVMImage:        cfg.DefaultVMImage,
		DefaultNetwork:        cfg.DefaultNetwork,
		DefaultPodCIDRBase:    cfg.DefaultPodCIDRBase,
		DefaultSite:           cfg.DefaultSite,
		DefaultGatewayPool:    cfg.DefaultGatewayPool,
		DefaultSSHKey:         cfg.DefaultSSHKey,
		HTTPBootContainerDisk: cfg.HTTPBootContainerDisk,
	})
	netMgr := network.NewManager(ctrlClient, network.Config{
		DefaultSite:        cfg.DefaultSite,
		DefaultGatewayPool: cfg.DefaultGatewayPool,
		DefaultPodCIDRBase: cfg.DefaultPodCIDRBase,
	})
	playpenServer := server.New(kube, webhookServer, certMgr, vmMgr, netMgr, server.Config{Port: cfg.Port})

	return playpenServer.Run(ctx)
}

func controllerRuntimeClient(restConfig *rest.Config) (ctrlclient.Client, error) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register core scheme: %w", err)
	}
	if err := netv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register unbounded-net scheme: %w", err)
	}
	if err := kubevirtv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register kubevirt scheme: %w", err)
	}

	ctrlClient, err := ctrlclient.New(restConfig, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create controller-runtime client: %w", err)
	}

	return ctrlClient, nil
}

func injectAPIServiceCABundle(ctx context.Context, restConfig *rest.Config, caBundle []byte) error {
	if len(caBundle) == 0 {
		return fmt.Errorf("playpen serving CA bundle is empty")
	}

	apiRegClient, err := apiregistrationclientset.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create apiregistration client: %w", err)
	}

	apiSvc, err := apiRegClient.ApiregistrationV1().APIServices().Get(ctx, "v1alpha1.playpen.unbounded-cloud.io", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("playpen APIService is not installed; skipping caBundle injection")
			return nil
		}

		return fmt.Errorf("get playpen APIService: %w", err)
	}

	apiSvc.Spec.CABundle = caBundle
	if _, err := apiRegClient.ApiregistrationV1().APIServices().Update(ctx, apiSvc, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update playpen APIService caBundle: %w", err)
	}

	return nil
}

func kubeConfig(path string) (*rest.Config, error) {
	if path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}

	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}
