// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
	"github.com/Azure/unbounded-kube/internal/cloudprovider"
	"github.com/Azure/unbounded-kube/internal/metalman/attestation"
	"github.com/Azure/unbounded-kube/internal/metalman/cloudinit"
	"github.com/Azure/unbounded-kube/internal/metalman/dhcp"
	"github.com/Azure/unbounded-kube/internal/metalman/indexing"
	"github.com/Azure/unbounded-kube/internal/metalman/ipallocator"
	"github.com/Azure/unbounded-kube/internal/metalman/lifecycle"
	"github.com/Azure/unbounded-kube/internal/metalman/netboot"
	"github.com/Azure/unbounded-kube/internal/metalman/redfish"
)

// ServePXECmd returns a cobra.Command that runs PXE servers and the BMC control loop.
func ServePXECmd() *cobra.Command {
	var (
		site              string
		cacheDir          string
		bindAddress       string
		httpPort          int
		healthPort        int
		dhcpInterface     string
		dhcpAutoInterface bool
		dhcpPort          int
		serveURL          string
		leaseDuration     time.Duration
		renewDeadline     time.Duration
		retryPeriod       time.Duration
		bootstrap         bool
		bootstrapImage    string
	)

	cmd := &cobra.Command{
		Use:   "serve-pxe",
		Short: "Run PXE servers and BMC control loop",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := ctrl.SetupSignalHandler()
			cfg := ctrl.GetConfigOrDie()

			if bootstrap && bootstrapImage == "" {
				return fmt.Errorf("--bootstrap-image is required when --bootstrap is enabled")
			}

			selector, err := SiteSelector(site)
			if err != nil {
				return fmt.Errorf("building site selector: %w", err)
			}

			leID := LeaderElectionID(site)

			scheme := BuildScheme()

			mgr, err := ctrl.NewManager(cfg, manager.Options{
				Scheme:                        scheme,
				LeaderElection:                true,
				LeaderElectionID:              leID,
				LeaderElectionNamespace:       "unbounded-kube",
				LeaseDuration:                 &leaseDuration,
				RenewDeadline:                 &renewDeadline,
				RetryPeriod:                   &retryPeriod,
				LeaderElectionReleaseOnCancel: true,
				Metrics:                       metricsserver.Options{BindAddress: "0"},
				HealthProbeBindAddress:        fmt.Sprintf(":%d", healthPort),
				Cache: cache.Options{
					ByObject: map[client.Object]cache.ByObject{
						&v1alpha3.Machine{}: {Label: selector},
					},
				},
			})
			if err != nil {
				return fmt.Errorf("creating manager: %w", err)
			}

			if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
				return fmt.Errorf("adding healthz check: %w", err)
			}

			if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
				return fmt.Errorf("adding readyz check: %w", err)
			}

			if err := mgr.GetFieldIndexer().IndexField(ctx, &v1alpha3.Machine{}, indexing.IndexNodeByMAC,
				indexing.IndexNodeByMACFunc); err != nil {
				return fmt.Errorf("indexing by MAC: %w", err)
			}

			if err := mgr.GetFieldIndexer().IndexField(ctx, &v1alpha3.Machine{}, indexing.IndexNodeByIP,
				indexing.IndexNodeByIPFunc); err != nil {
				return fmt.Errorf("indexing by IP: %w", err)
			}

			if err := os.MkdirAll(filepath.Join(cacheDir, "sha256"), 0o755); err != nil {
				return fmt.Errorf("creating cache dir: %w", err)
			}

			clientset, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return fmt.Errorf("creating clientset: %w", err)
			}

			// Detect cloud provider for default node labels. These are
			// static and resolved once at startup.
			var providerLabels map[string]string

			provider, err := cloudprovider.DetectProvider(ctx, clientset)
			if err != nil {
				return fmt.Errorf("detect provider: %w", err)
			}

			if provider != nil {
				providerLabels = provider.DefaultLabels()
			}

			sv, err := clientset.Discovery().ServerVersion()
			if err != nil {
				return fmt.Errorf("resolving cluster Kubernetes version: %w", err)
			}

			kubeVersion := sv.GitVersion

			// Resolve cluster DNS from the kube-dns Service ClusterIP.
			dnsSvc, err := clientset.CoreV1().Services("kube-system").Get(ctx, "kube-dns", metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("resolving cluster DNS: %w", err)
			}

			clusterDNS := dnsSvc.Spec.ClusterIP
			if clusterDNS == "" {
				return fmt.Errorf("kube-dns Service has no ClusterIP")
			}

			// Watch the cluster-info ConfigMap in kube-public for API
			// server URL and CA certificate. This is the only watched
			// cluster-level resource; DNS and version are resolved once
			// at startup above.
			clusterInfoWatcher, err := NewClusterInfoWatcher(ctx, clientset, slog.Default())
			if err != nil {
				return fmt.Errorf("creating cluster-info watcher: %w", err)
			}

			if err := mgr.Add(clusterInfoWatcher); err != nil {
				return fmt.Errorf("adding cluster-info watcher: %w", err)
			}

			clusterCA := attestation.ClusterCAFromConfig(cfg)

			serverIP := net.ParseIP(bindAddress)
			if serveURL == "" {
				ip := serverIP
				if ip.IsUnspecified() {
					detected, err := OutboundIP()
					if err != nil {
						return fmt.Errorf("detecting outbound IP for --serve-url default: %w", err)
					}

					ip = detected
					serverIP = detected
				}

				serveURL = fmt.Sprintf("http://%s:%d", ip, httpPort)
			}

			ociCache := netboot.NewOCICache(cacheDir)

			if err := (&netboot.OCIReconciler{
				Client: mgr.GetClient(),
				Cache:  ociCache,
			}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("setting up OCI reconciler: %w", err)
			}

			redfishPool := redfish.NewPool()
			defer redfishPool.Close()

			if err := (&redfish.Reconciler{Client: mgr.GetClient(), Pool: redfishPool}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("setting up Redfish reconciler: %w", err)
			}

			if err := (&lifecycle.Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("setting up Lifecycle reconciler: %w", err)
			}

			if err := (&cloudinit.Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("setting up CloudInit reconciler: %w", err)
			}

			if err := (&ipallocator.Reconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("setting up IP allocator reconciler: %w", err)
			}

			resolver := netboot.FileResolver{
				Cache:             ociCache,
				Reader:            mgr.GetClient(),
				Cluster:           clusterInfoWatcher,
				ServeURL:          serveURL,
				KubernetesVersion: kubeVersion,
				ClusterDNS:        clusterDNS,
				ProviderLabels:    providerLabels,
			}

			if dhcpInterface != "" && dhcpAutoInterface {
				return fmt.Errorf("--dhcp-interface and --dhcp-auto-interface are mutually exclusive")
			}

			dhcpServerIP := serverIP

			if dhcpAutoInterface {
				detected, err := InterfaceForIP(serverIP)
				if err != nil {
					return fmt.Errorf("detecting interface for server IP %s: %w", serverIP, err)
				}

				dhcpInterface = detected
			}

			if dhcpInterface != "" {
				ifIP, err := InterfaceIPv4(dhcpInterface)
				if err != nil {
					return fmt.Errorf("detecting IPv4 address of interface %s: %w", dhcpInterface, err)
				}

				dhcpServerIP = ifIP
			}

			dhcpServer := &dhcp.Server{
				Interface: dhcpInterface,
				Port:      dhcpPort,
				Reader:    mgr.GetClient(),
				ServerIP:  dhcpServerIP,
				OCICache:  ociCache,
			}

			if bootstrap {
				dhcpServer.Bootstrap = &dhcp.BootstrapConfig{
					Client:    mgr.GetClient(),
					APIReader: mgr.GetAPIReader(),
					Image:     bootstrapImage,
					Site:      site,
				}
			}

			if err := mgr.Add(dhcpServer); err != nil {
				return fmt.Errorf("adding DHCP server: %w", err)
			}

			tftpServer := &netboot.TFTPServer{
				BindAddr:     bindAddress,
				FileResolver: resolver,
			}
			if err := mgr.Add(tftpServer); err != nil {
				return fmt.Errorf("adding TFTP server: %w", err)
			}

			attestHandler := &attestation.Handler{
				Clientset:      clientset,
				ClusterCA:      clusterCA,
				LookupNodeByIP: resolver.LookupNodeByIP,
				StatusUpdater:  &StatusUpdater{Client: mgr.GetClient()},
			}

			httpMux := http.NewServeMux()
			httpMux.HandleFunc("POST /attest", attestHandler.Attest)

			httpServer := &netboot.HTTPServer{
				BindAddr:     bindAddress,
				Port:         httpPort,
				Client:       mgr.GetClient(),
				Mux:          httpMux,
				FileResolver: resolver,
			}
			if err := mgr.Add(httpServer); err != nil {
				return fmt.Errorf("adding HTTP server: %w", err)
			}

			siteDisplay := site
			if siteDisplay == "" {
				siteDisplay = "(unlabeled nodes)"
			}

			PrintConfig("site", siteDisplay)
			PrintConfig("leader-election", leID)
			PrintConfig("serve-url", serveURL)
			PrintConfig("cache-dir", cacheDir)
			PrintConfig("dhcp-interface", dhcpInterface)
			PrintConfig("dhcp-port", fmt.Sprintf("%d", dhcpPort))
			PrintConfig("bootstrap", fmt.Sprintf("%t", bootstrap))
			if bootstrap {
				PrintConfig("bootstrap-image", bootstrapImage)
			}
			fmt.Println()

			if dhcpInterface != "" {
				PrintService("DHCP", fmt.Sprintf("%s:%d", dhcpInterface, dhcpPort))
			} else {
				PrintService("DHCP", fmt.Sprintf("0.0.0.0:%d (relay)", dhcpPort))
			}

			PrintService("TFTP", fmt.Sprintf("%s:69", bindAddress))
			PrintService("HTTP", fmt.Sprintf("%s:%d", bindAddress, httpPort))
			PrintService("Redfish", "reconciler")
			PrintReady()

			return mgr.Start(ctx)
		},
	}

	cmd.Flags().StringVar(&site, "site", "", "Site label value to select Machines")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", DefaultCacheDir(), "Local directory for cached image artifacts")
	cmd.Flags().StringVar(&bindAddress, "bind-address", "0.0.0.0", "IP address to bind servers")
	cmd.Flags().IntVar(&httpPort, "http-port", 8880, "Port for the HTTP artifact server")
	cmd.Flags().IntVar(&healthPort, "health-port", 8081, "Port for the health/readiness probe server")
	cmd.Flags().StringVar(&dhcpInterface, "dhcp-interface", "", "Network interface for broadcast DHCP (omit for relay/unicast mode)")
	cmd.Flags().BoolVar(&dhcpAutoInterface, "dhcp-auto-interface", false, "Auto-detect the DHCP interface from the server IP")
	cmd.Flags().IntVar(&dhcpPort, "dhcp-port", 67, "UDP port for the DHCP server")
	cmd.Flags().StringVar(&serveURL, "serve-url", "", "External URL of this serve instance")
	cmd.Flags().DurationVar(&leaseDuration, "leader-elect-lease-duration", 15*time.Second, "Duration that non-leader candidates will wait before attempting to acquire leadership")
	cmd.Flags().DurationVar(&renewDeadline, "leader-elect-renew-deadline", 10*time.Second, "Duration the acting leader will retry refreshing leadership before giving up")
	cmd.Flags().DurationVar(&retryPeriod, "leader-elect-retry-period", 2*time.Second, "Duration between leader election retries")
	cmd.Flags().BoolVar(&bootstrap, "bootstrap", false, "Automatically create Machine objects for unknown MAC addresses during DHCP")
	cmd.Flags().StringVar(&bootstrapImage, "bootstrap-image", "", "OCI image reference for bootstrapped Machines (required when --bootstrap is set)")

	return cmd
}
