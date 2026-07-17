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
	"strings"
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

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/cloudprovider"
	"github.com/Azure/unbounded/internal/metalman/attestation"
	"github.com/Azure/unbounded/internal/metalman/dhcp"
	"github.com/Azure/unbounded/internal/metalman/indexing"
	metalmachineops "github.com/Azure/unbounded/internal/metalman/machineops"
	"github.com/Azure/unbounded/internal/metalman/netboot"
	"github.com/Azure/unbounded/internal/metalman/redfish"
)

// DefaultNetbootImage is the default netboot OCI image used when a Machine
// omits spec.pxe.netbootImage. It is set at build time via -ldflags.
var DefaultNetbootImage = "netboot:latest"

// ServePXECmd returns a cobra.Command that runs PXE servers and the BMC control loop.
func ServePXECmd() *cobra.Command {
	var (
		site                           string
		cacheDir                       string
		bindAddress                    string
		advertiseIP                    string
		httpPort                       int
		healthPort                     int
		dhcpInterface                  string
		dhcpAutoInterface              bool
		dhcpPort                       int
		serveURL                       string
		leaseDuration                  time.Duration
		renewDeadline                  time.Duration
		retryPeriod                    time.Duration
		operationMaxConcurrentMachines int
		operationMaxAttempts           int32
		operationPollInterval          time.Duration
		operationPowerActionTimeout    time.Duration
		defaultNetbootImage            string
		defaultNetbootPullSecret       string
	)

	cmd := &cobra.Command{
		Use:   "serve-pxe",
		Short: "Run PXE servers and BMC control loop",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := ctrl.SetupSignalHandler()
			cfg := ctrl.GetConfigOrDie()

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

			// advertisedIP is the address other hosts (the DHCP/TFTP/HTTP
			// clients) use to reach this metalman instance. It defaults to the
			// bind address but can be overridden with --advertise-ip when
			// metalman binds a loopback address yet is reached over a different
			// network (for example when a userspace overlay proxy forwards
			// traffic from an overlay IP to metalman on 127.0.0.1). It is used
			// as the DHCP ServerIP (next-server) in relay mode and as the
			// default --serve-url host, without changing the actual bind
			// address of the sockets.
			advertisedIP, err := resolveAdvertisedIP(bindAddress, advertiseIP, OutboundIP)
			if err != nil {
				return err
			}

			if serveURL == "" {
				serveURL = fmt.Sprintf("http://%s:%d", advertisedIP, httpPort)
			}

			defaultNetbootImage = strings.TrimSpace(defaultNetbootImage)
			defaultNetbootPullSecret = strings.TrimSpace(defaultNetbootPullSecret)

			defaultNetbootPullSecretRef, err := parseNamespacedSecretReference(defaultNetbootPullSecret)
			if err != nil {
				return err
			}

			ociCache := netboot.NewOCICache(cacheDir)

			if err := (&netboot.OCIReconciler{
				Client:                      mgr.GetClient(),
				Cache:                       ociCache,
				DefaultNetbootRef:           defaultNetbootImage,
				DefaultNetbootPullSecretRef: defaultNetbootPullSecretRef,
			}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("setting up OCI reconciler: %w", err)
			}

			redfishPool := redfish.NewPool()
			defer redfishPool.Close()

			statusQueue := &metalmachineops.StatusQueue{Client: mgr.GetClient()}

			resolver := netboot.FileResolver{
				Cache:             ociCache,
				Reader:            mgr.GetClient(),
				Cluster:           clusterInfoWatcher,
				ServeURL:          serveURL,
				DefaultNetbootRef: defaultNetbootImage,
				KubernetesVersion: kubeVersion,
				ClusterDNS:        clusterDNS,
				ProviderLabels:    providerLabels,
			}

			if err := (&redfish.Reconciler{Client: mgr.GetClient(), Pool: redfishPool, FileResolver: &resolver}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("setting up Redfish reconciler: %w", err)
			}

			if err := (&metalmachineops.Reconciler{
				Client:                mgr.GetClient(),
				APIReader:             mgr.GetAPIReader(),
				Site:                  site,
				PowerClients:          &metalmachineops.RedfishPowerClientFactory{Reader: mgr.GetClient(), Pool: redfishPool},
				HTTPBootURL:           resolver.HTTPBootURL,
				MaxConcurrentMachines: operationMaxConcurrentMachines,
				MaxAttempts:           operationMaxAttempts,
				PollInterval:          operationPollInterval,
				PowerActionTimeout:    operationPowerActionTimeout,
			}).SetupWithManager(mgr); err != nil {
				return fmt.Errorf("setting up MachineOperation reconciler: %w", err)
			}

			if dhcpInterface != "" && dhcpAutoInterface {
				return fmt.Errorf("--dhcp-interface and --dhcp-auto-interface are mutually exclusive")
			}

			dhcpServerIP := advertisedIP

			if dhcpAutoInterface {
				interfaceIP, err := resolveDHCPInterfaceIP(serverIP, OutboundIP)
				if err != nil {
					return err
				}

				detected, err := InterfaceForIP(interfaceIP)
				if err != nil {
					return fmt.Errorf("detecting interface for server IP %s: %w", interfaceIP, err)
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
				Interface:         dhcpInterface,
				Port:              dhcpPort,
				Reader:            mgr.GetClient(),
				ServerIP:          dhcpServerIP,
				OCICache:          ociCache,
				ServeURL:          serveURL,
				DefaultNetbootRef: defaultNetbootImage,
			}
			if err := mgr.Add(dhcpServer); err != nil {
				return fmt.Errorf("adding DHCP server: %w", err)
			}

			if err := mgr.Add(statusQueue); err != nil {
				return fmt.Errorf("adding status queue: %w", err)
			}

			tftpServer := &netboot.TFTPServer{
				BindAddr:       bindAddress,
				FileResolver:   resolver,
				StatusRecorder: statusQueue,
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
				BindAddr:       bindAddress,
				Port:           httpPort,
				Client:         mgr.GetClient(),
				Mux:            httpMux,
				FileResolver:   resolver,
				StatusRecorder: statusQueue,
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
			PrintConfig("advertise-ip", advertisedIP.String())
			PrintConfig("default-netboot-image", defaultNetbootImage)
			PrintConfig("cache-dir", cacheDir)
			PrintConfig("dhcp-interface", dhcpInterface)
			PrintConfig("dhcp-port", fmt.Sprintf("%d", dhcpPort))
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
	cmd.Flags().StringVar(&advertiseIP, "advertise-ip", "", "IP address clients use to reach this instance (defaults to --bind-address); used as the DHCP next-server and default --serve-url host when metalman binds a different (for example loopback) address")
	cmd.Flags().IntVar(&httpPort, "http-port", 8880, "Port for the HTTP artifact server")
	cmd.Flags().IntVar(&healthPort, "health-port", 8081, "Port for the health/readiness probe server")
	cmd.Flags().StringVar(&dhcpInterface, "dhcp-interface", "", "Network interface for broadcast DHCP (omit for relay/unicast mode)")
	cmd.Flags().BoolVar(&dhcpAutoInterface, "dhcp-auto-interface", false, "Auto-detect the DHCP interface from the server IP")
	cmd.Flags().IntVar(&dhcpPort, "dhcp-port", 67, "UDP port for the DHCP server")
	cmd.Flags().StringVar(&serveURL, "serve-url", "", "External URL of this serve instance")
	cmd.Flags().DurationVar(&leaseDuration, "leader-elect-lease-duration", 15*time.Second, "Duration that non-leader candidates will wait before attempting to acquire leadership")
	cmd.Flags().DurationVar(&renewDeadline, "leader-elect-renew-deadline", 10*time.Second, "Duration the acting leader will retry refreshing leadership before giving up")
	cmd.Flags().DurationVar(&retryPeriod, "leader-elect-retry-period", 2*time.Second, "Duration between leader election retries")
	cmd.Flags().IntVar(&operationMaxConcurrentMachines, "operation-max-concurrent-machines", 10, "Maximum target Machines advanced concurrently within one MachineOperation")
	cmd.Flags().Int32Var(&operationMaxAttempts, "operation-max-attempts", 3, "Maximum Redfish action attempts per target Machine")
	cmd.Flags().DurationVar(&operationPollInterval, "operation-poll-interval", 5*time.Second, "Poll interval for in-progress MachineOperations")
	cmd.Flags().DurationVar(&operationPowerActionTimeout, "operation-power-action-timeout", 5*time.Minute, "Timeout for a target Machine to complete a power/boot stage (for example writing the boot image during a repave) before metalman re-issues the power action. Increase for slow netboot environments such as high-latency network overlays.")
	cmd.Flags().StringVar(&defaultNetbootImage, "default-netboot-image", DefaultNetbootImage, "Default OCI image containing PXE netboot artifacts")
	cmd.Flags().StringVar(&defaultNetbootPullSecret, "default-netboot-pull-secret", "", "Namespaced Secret reference (namespace/name) for pulling the default netboot OCI image")

	return cmd
}

// resolveAdvertisedIP determines the address clients use to reach this metalman
// instance. advertiseIP takes precedence when set; otherwise the bindAddress is
// used. An unspecified result (for example 0.0.0.0) is resolved to a concrete
// address via outbound. This lets metalman bind one address (such as loopback)
// while advertising another (such as an overlay IP) as the DHCP next-server and
// default serve-url host.
func resolveAdvertisedIP(bindAddress, advertiseIP string, outbound func() (net.IP, error)) (net.IP, error) {
	advertised := net.ParseIP(bindAddress)
	if strings.TrimSpace(advertiseIP) != "" {
		advertised = net.ParseIP(strings.TrimSpace(advertiseIP))
		if advertised == nil {
			return nil, fmt.Errorf("invalid --advertise-ip %q", advertiseIP)
		}
	}

	if advertised == nil {
		return nil, fmt.Errorf("invalid --bind-address %q", bindAddress)
	}

	if advertised.IsUnspecified() {
		detected, err := outbound()
		if err != nil {
			return nil, fmt.Errorf("detecting outbound IP for advertised address: %w", err)
		}

		advertised = detected
	}

	return advertised, nil
}

func resolveDHCPInterfaceIP(serverIP net.IP, outbound func() (net.IP, error)) (net.IP, error) {
	if serverIP == nil {
		return nil, fmt.Errorf("invalid server IP")
	}

	if !serverIP.IsUnspecified() {
		return serverIP, nil
	}

	detected, err := outbound()
	if err != nil {
		return nil, fmt.Errorf("detecting outbound IP for DHCP interface: %w", err)
	}

	return detected, nil
}

func parseNamespacedSecretReference(ref string) (*v1alpha3.NamespacedSecretReference, error) {
	if ref == "" {
		return nil, nil
	}

	namespace, name, ok := strings.Cut(ref, "/")
	if !ok || namespace == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("--default-netboot-pull-secret must use namespace/name")
	}

	return &v1alpha3.NamespacedSecretReference{Namespace: namespace, Name: name}, nil
}
