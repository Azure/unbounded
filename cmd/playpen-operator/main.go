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
		Short: "Aggregated Kubernetes API allocator for playpen runner pods",
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
	flags.StringVar(&cfg.RunnerNamespace, "runner-namespace", cfg.RunnerNamespace, "namespace containing playpen-runner pods")
	flags.StringVar(&cfg.RunnerLabelSelector, "runner-label-selector", cfg.RunnerLabelSelector, "label selector for playpen-runner pods")
	flags.StringVar(&cfg.RunnerImage, "runner-image", cfg.RunnerImage, "container image used for operator-managed playpen-runner pods")
	flags.StringVar(&cfg.RunnerImagePullPolicy, "runner-image-pull-policy", cfg.RunnerImagePullPolicy, "imagePullPolicy used for operator-managed playpen-runner pods")
	flags.StringVar(&cfg.RunnerServiceAccountName, "runner-service-account-name", cfg.RunnerServiceAccountName, "ServiceAccount used for operator-managed playpen-runner pods")
	flags.IntVar(&cfg.RunnerAMD64Count, "runner-amd64-count", cfg.RunnerAMD64Count, "desired number of idle amd64 playpen-runner pods")
	flags.IntVar(&cfg.RunnerARM64Count, "runner-arm64-count", cfg.RunnerARM64Count, "desired number of idle arm64 playpen-runner pods")
	flags.BoolVar(&cfg.RunnerRequireKVM, "runner-require-kvm", cfg.RunnerRequireKVM, "mount /dev/kvm into operator-managed playpen-runner pods")
	flags.BoolVar(&cfg.RunnerControlPlaneToleration, "runner-control-plane-tolerations", cfg.RunnerControlPlaneToleration, "allow operator-managed playpen-runner pods to schedule on control-plane nodes")
	flags.StringVar(&cfg.ExternalClientSite, "external-client-site", cfg.ExternalClientSite, "unbounded-net site label assigned to synthetic playpen client Nodes")
	flags.IntVar(&cfg.ControlPlaneCount, "control-plane-count", cfg.ControlPlaneCount, "desired number of idle k3s control-plane pods per Kubernetes version")
	flags.StringSliceVar(&cfg.ControlPlaneVersions, "control-plane-versions", cfg.ControlPlaneVersions, "Kubernetes versions available for k3s control-plane allocations")
	flags.StringVar(&cfg.ControlPlaneImage, "control-plane-image", cfg.ControlPlaneImage, "k3s image template for control-plane pods; {version} is replaced with the Kubernetes version")
	flags.StringVar(&cfg.ControlPlaneServiceAccountName, "control-plane-service-account-name", cfg.ControlPlaneServiceAccountName, "ServiceAccount used for operator-managed control-plane pods")
	flags.Int32Var(&cfg.ControlPlaneAPIServerHostPortStart, "control-plane-apiserver-host-port-start", cfg.ControlPlaneAPIServerHostPortStart, "first TCP hostPort available for control-plane API servers")
	flags.Int32Var(&cfg.ControlPlaneAPIServerHostPortEnd, "control-plane-apiserver-host-port-end", cfg.ControlPlaneAPIServerHostPortEnd, "last TCP hostPort available for control-plane API servers")
	flags.DurationVar(&cfg.PlaypenTTL, "playpen-ttl", cfg.PlaypenTTL, "maximum lifetime for an allocated playpen pod; disabled when non-positive")
	flags.DurationVar(&cfg.ReconcileInterval, "reconcile-interval", cfg.ReconcileInterval, "runner cleanup reconciliation interval")
	flags.StringVar(&cfg.Runner.ListenAddr, "runner-listen-addr", cfg.Runner.ListenAddr, "runner HTTPS listen address returned in allocs")
	flags.StringVar(&cfg.Runner.PublicRedfishURL, "runner-public-redfish-url", cfg.Runner.PublicRedfishURL, "runner Redfish URL returned in allocs; defaults to the runner pod IP")
	flags.StringVar(&cfg.Runner.VXLAN.Interface, "runner-vxlan-interface", cfg.Runner.VXLAN.Interface, "runner VXLAN interface")
	flags.IntVar(&cfg.Runner.VXLAN.VNI, "runner-vxlan-vni", cfg.Runner.VXLAN.VNI, "runner VXLAN network identifier")
	flags.IntVar(&cfg.Runner.VXLAN.Port, "runner-vxlan-port", cfg.Runner.VXLAN.Port, "runner VXLAN UDP destination port")
	flags.StringVar(&cfg.Runner.Guest.MAC, "runner-guest-mac", cfg.Runner.Guest.MAC, "guest MAC returned in allocs")
	flags.StringVar(&cfg.Runner.Guest.IPv4, "runner-guest-ipv4", cfg.Runner.Guest.IPv4, "guest IPv4 returned in allocs")
	flags.StringVar(&cfg.Runner.Guest.SubnetMask, "runner-guest-subnet-mask", cfg.Runner.Guest.SubnetMask, "guest subnet mask returned in allocs")
	flags.StringVar(&cfg.Runner.Guest.Gateway, "runner-guest-gateway", cfg.Runner.Guest.Gateway, "guest gateway returned in allocs")
	flags.StringSliceVar(&cfg.Runner.Guest.DNS, "runner-guest-dns", cfg.Runner.Guest.DNS, "guest DNS servers returned in allocs")
	flags.StringVar(&cfg.Runner.Redfish.Username, "runner-redfish-username", cfg.Runner.Redfish.Username, "Redfish username returned in allocs")
	flags.StringVar(&cfg.Runner.Redfish.Password, "runner-redfish-password", cfg.Runner.Redfish.Password, "Redfish password returned in allocs")
	flags.StringVar(&cfg.Runner.Redfish.DeviceID, "runner-redfish-device-id", cfg.Runner.Redfish.DeviceID, "Redfish device ID returned in allocs")

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

	op := &operator.Operator{Client: kubeClient, KubeClient: kubeClientset, APIRegClient: apiRegClient, Config: cfg, Scheme: scheme}

	return op.Run(ctx)
}

func runtimeScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))

	return scheme
}
