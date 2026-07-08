// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	apiregclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Azure/unbounded/internal/playpen/operator"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	cfg := operator.DefaultConfig()
	cmd := &cobra.Command{
		Use:   "playpen-operator",
		Short: "Aggregated Kubernetes API allocator for KubeVirt playpen VMs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			return run(ctx, cfg)
		},
		Version: version.Version + " (commit: " + version.GitCommit + ")",
	}

	flags := cmd.Flags()
	flags.StringVar(&cfg.ListenAddr, "listen-addr", cfg.ListenAddr, "HTTPS listen address")
	flags.StringVar(&cfg.Namespace, "namespace", cfg.Namespace, "operator namespace")
	flags.StringVar(&cfg.ServiceName, "service-name", cfg.ServiceName, "operator Service name used by the aggregated APIService")
	flags.StringVar(&cfg.TLSSecretName, "tls-secret-name", cfg.TLSSecretName, "Secret storing the operator HTTPS serving certificate")
	flags.StringVar(&cfg.Image, "image", cfg.Image, "container image used for playpen endpoint pods")
	flags.StringVar(&cfg.ImagePullPolicy, "image-pull-policy", cfg.ImagePullPolicy, "imagePullPolicy used for endpoint pods")
	flags.StringVar(&cfg.ServiceAccount, "service-account", cfg.ServiceAccount, "ServiceAccount used for endpoint pods")
	flags.DurationVar(&cfg.AllocationTTL, "allocation-ttl", cfg.AllocationTTL, "maximum lifetime for an allocation")
	flags.DurationVar(&cfg.ReconcileInterval, "reconcile-interval", cfg.ReconcileInterval, "allocation cleanup reconciliation interval")
	flags.Int32Var(&cfg.WireGuardHostPortStart, "wireguard-host-port-start", cfg.WireGuardHostPortStart, "first UDP hostPort available for allocation WireGuard endpoints")
	flags.Int32Var(&cfg.WireGuardHostPortEnd, "wireguard-host-port-end", cfg.WireGuardHostPortEnd, "last UDP hostPort available for allocation WireGuard endpoints")
	flags.IntVar(&cfg.EndpointListenPort, "endpoint-wireguard-port", cfg.EndpointListenPort, "WireGuard listen port inside endpoint pods")
	flags.IntVar(&cfg.RedfishPort, "redfish-port", cfg.RedfishPort, "Redfish HTTPS listen port on the WireGuard address")
	flags.IntVar(&cfg.VXLANPort, "vxlan-port", cfg.VXLANPort, "VXLAN UDP destination port")
	flags.StringSliceVar(&cfg.GuestDNS, "guest-dns", cfg.GuestDNS, "DNS servers returned for guest DHCP/PXE reservations")
	flags.StringVar(&cfg.DefaultDiskSize, "default-disk-size", cfg.DefaultDiskSize, "default KubeVirt emptyDisk size")
	flags.StringVar(&cfg.DefaultMemory, "default-memory", cfg.DefaultMemory, "default VM memory request")
	flags.IntVar(&cfg.DefaultCPUs, "default-cpus", cfg.DefaultCPUs, "default VM CPU cores")

	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.SetVersionTemplate(`{{printf "%s\n" .Version}}`)

	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg operator.Config) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	scheme := runtimeScheme()
	restConfig := ctrl.GetConfigOrDie()

	kubeClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	kubeClientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	apiRegClient, err := apiregclient.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	op := &operator.Operator{Client: kubeClient, KubeClient: kubeClientset, Dynamic: dynamicClient, APIRegClient: apiRegClient, RESTConfig: restConfig, Config: cfg, Scheme: scheme}

	return op.Run(ctx)
}

func runtimeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))

	return scheme
}
