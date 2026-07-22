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
	"github.com/Azure/unbounded/internal/unbounded"
)

// DefaultNetbootImage is the default netboot OCI image used when a Machine
// omits spec.host.netboot.netbootImage. It is set at build time via -ldflags.
var DefaultNetbootImage = "netboot:latest"

// ServePXECmd returns the legacy monolithic command. Production wiring uses
// ControllerCmd, ServerCmd, and EdgeCmd instead.
func ServePXECmd() *cobra.Command {
	return newMetalmanRoleCmd(metalmanRoleLegacy, "Run PXE servers and BMC control loop")
}

func newMetalmanRoleCmd(role metalmanRole, short string) *cobra.Command {
	var (
		site                           string
		cacheDir                       string
		bindAddress                    string
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
		defaultNetbootImage            string
		defaultNetbootPullSecret       string
		capabilityKeyFile              string
		capabilityKeyID                string
		edgeServiceAccount             string
	)
	components := componentsForRole(role)

	cmd := &cobra.Command{
		Use:   string(role),
		Short: short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := ctrl.SetupSignalHandler()
			cfg := ctrl.GetConfigOrDie()

			selector, err := SiteSelector(site)
			if err != nil {
				return fmt.Errorf("building site selector: %w", err)
			}

			leID := LeaderElectionID(site)

			// Leader-election lease lives in the namespace metalman is
			// deployed into. unbounded.SystemNamespace() sources it from the
			// Downward API POD_NAMESPACE env (set by the deployment) so the
			// lease and its RBAC stay co-located even when the install
			// namespace is overridden, falling back to the default when unset.
			leaderElectionNamespace := unbounded.SystemNamespace()

			scheme := BuildScheme()

			mgr, err := ctrl.NewManager(cfg, manager.Options{
				Scheme:                        scheme,
				LeaderElection:                components.leaderElection,
				LeaderElectionID:              leID,
				LeaderElectionNamespace:       leaderElectionNamespace,
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

			defaultNetbootImage = strings.TrimSpace(defaultNetbootImage)
			defaultNetbootPullSecret = strings.TrimSpace(defaultNetbootPullSecret)

			defaultNetbootPullSecretRef, err := parseNamespacedSecretReference(defaultNetbootPullSecret)
			if err != nil {
				return err
			}

			ociCache := netboot.NewOCICache(cacheDir)

			if components.ociReconciler {
				if err := (&netboot.OCIReconciler{
					Client:                      mgr.GetClient(),
					Cache:                       ociCache,
					DefaultNetbootRef:           defaultNetbootImage,
					DefaultNetbootPullSecretRef: defaultNetbootPullSecretRef,
				}).SetupWithManager(mgr); err != nil {
					return fmt.Errorf("setting up OCI reconciler: %w", err)
				}
			}

			redfishPool := redfish.NewPool()
			defer redfishPool.Close()

			sessionManager := &metalmachineops.KubernetesSessionManager{
				Client:                   mgr.GetClient(),
				Cache:                    ociCache,
				DefaultNetbootRef:        defaultNetbootImage,
				DefaultNetbootPullSecret: defaultNetbootPullSecretRef,
				Cluster:                  clusterInfoWatcher,
				KubernetesVersion:        kubeVersion,
				ClusterDNS:               clusterDNS,
				ProviderLabels:           providerLabels,
			}

			var statusQueue *metalmachineops.StatusQueue
			if components.statusUpdates {
				statusQueue = &metalmachineops.StatusQueue{Client: mgr.GetClient()}
			}

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

			if components.redfish {
				if err := (&redfish.Reconciler{Client: mgr.GetClient(), Pool: redfishPool, FileResolver: &resolver}).SetupWithManager(mgr); err != nil {
					return fmt.Errorf("setting up Redfish reconciler: %w", err)
				}
			}

			if components.machineOps {
				var sessionHTTPBootURL func(*v1alpha3.NetbootSession) (string, error)
				if components.sessionManager {
					capabilityKey, err := os.ReadFile(capabilityKeyFile)
					if err != nil {
						return fmt.Errorf("reading capability key: %w", err)
					}
					capabilities, err := netboot.NewCapabilitySigner(capabilityKey, capabilityKeyID, nil)
					if err != nil {
						return fmt.Errorf("creating capability signer: %w", err)
					}
					sessionHTTPBootURL = func(session *v1alpha3.NetbootSession) (string, error) {
						return netboot.SessionArtifactURL(capabilities, session, session.Spec.Boot.FirmwareArtifact)
					}
				}
				if err := (&metalmachineops.Reconciler{
					Client:                mgr.GetClient(),
					APIReader:             mgr.GetAPIReader(),
					Site:                  site,
					PowerClients:          &metalmachineops.RedfishPowerClientFactory{Reader: mgr.GetClient(), Pool: redfishPool},
					Sessions:              sessionManager,
					HTTPBootURL:           resolver.HTTPBootURL,
					SessionHTTPBootURL:    sessionHTTPBootURL,
					MaxConcurrentMachines: operationMaxConcurrentMachines,
					MaxAttempts:           operationMaxAttempts,
					PollInterval:          operationPollInterval,
				}).SetupWithManager(mgr); err != nil {
					return fmt.Errorf("setting up MachineOperation reconciler: %w", err)
				}
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

			if components.dhcp {
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
			}

			if statusQueue != nil {
				if err := mgr.Add(statusQueue); err != nil {
					return fmt.Errorf("adding status queue: %w", err)
				}
			}

			if components.tftp {
				tftpServer := &netboot.TFTPServer{
					BindAddr:       bindAddress,
					FileResolver:   resolver,
					StatusRecorder: statusQueue,
				}
				if err := mgr.Add(tftpServer); err != nil {
					return fmt.Errorf("adding TFTP server: %w", err)
				}
			}

			if components.http {
				httpMux := http.NewServeMux()
				var attestHandler *attestation.Handler
				if components.attestation {
					attestHandler = &attestation.Handler{
						Clientset:      clientset,
						ClusterCA:      clusterCA,
						LookupNodeByIP: resolver.LookupNodeByIP,
						StatusUpdater:  &StatusUpdater{Client: mgr.GetClient()},
					}
				}
				if components.sessionHTTP {
					capabilityKey, err := os.ReadFile(capabilityKeyFile)
					if err != nil {
						return fmt.Errorf("reading capability key: %w", err)
					}
					capabilities, err := netboot.NewCapabilitySigner(capabilityKey, capabilityKeyID, nil)
					if err != nil {
						return fmt.Errorf("creating capability signer: %w", err)
					}
					(&netboot.SessionHTTPServer{
						Client:         mgr.GetClient(),
						Cache:          ociCache,
						Capabilities:   capabilities,
						StatusRecorder: &metalmachineops.SessionStatusRecorder{Client: mgr.GetClient()},
						Attestation:    attestHandler,
						EdgeAuthenticator: &netboot.TokenReviewEdgeAuthenticator{
							Client:             clientset.AuthenticationV1(),
							ServiceAccountName: edgeServiceAccount,
						},
					}).RegisterHandlers(httpMux)
				}
				if attestHandler != nil && !components.sessionHTTP {
					httpMux.HandleFunc("POST /attest", attestHandler.Attest)
				}

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
			}

			siteDisplay := site
			if siteDisplay == "" {
				siteDisplay = "(unlabeled nodes)"
			}

			PrintConfig("site", siteDisplay)
			PrintConfig("role", string(role))
			PrintConfig("leader-election", fmt.Sprintf("%t", components.leaderElection))
			PrintConfig("serve-url", serveURL)
			PrintConfig("default-netboot-image", defaultNetbootImage)
			PrintConfig("cache-dir", cacheDir)
			PrintConfig("dhcp-interface", dhcpInterface)
			PrintConfig("dhcp-port", fmt.Sprintf("%d", dhcpPort))
			fmt.Println()

			if components.dhcp {
				if dhcpInterface != "" {
					PrintService("DHCP", fmt.Sprintf("%s:%d", dhcpInterface, dhcpPort))
				} else {
					PrintService("DHCP", fmt.Sprintf("0.0.0.0:%d (relay)", dhcpPort))
				}
			}

			if components.tftp {
				PrintService("TFTP", fmt.Sprintf("%s:69", bindAddress))
			}
			if components.http {
				PrintService("HTTP", fmt.Sprintf("%s:%d", bindAddress, httpPort))
			}
			if components.redfish {
				PrintService("Redfish", "reconciler")
			}
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
	cmd.Flags().IntVar(&operationMaxConcurrentMachines, "operation-max-concurrent-machines", 10, "Maximum target Machines advanced concurrently within one MachineOperation")
	cmd.Flags().Int32Var(&operationMaxAttempts, "operation-max-attempts", 3, "Maximum Redfish action attempts per target Machine")
	cmd.Flags().DurationVar(&operationPollInterval, "operation-poll-interval", 5*time.Second, "Poll interval for in-progress MachineOperations")
	cmd.Flags().StringVar(&defaultNetbootImage, "default-netboot-image", DefaultNetbootImage, "Default OCI image containing PXE netboot artifacts")
	cmd.Flags().StringVar(&defaultNetbootPullSecret, "default-netboot-pull-secret", "", "Namespaced Secret reference (namespace/name) for pulling the default netboot OCI image")
	if components.sessionHTTP || components.sessionManager {
		cmd.Flags().StringVar(&capabilityKeyFile, "capability-key-file", "/var/run/secrets/metalman/capability.key", "File containing the shared capability HMAC key")
		cmd.Flags().StringVar(&capabilityKeyID, "capability-key-id", "v1", "Identifier for the active capability HMAC key")
	}
	if components.sessionHTTP {
		cmd.Flags().StringVar(&edgeServiceAccount, "edge-service-account", "metalman-edge", "ServiceAccount name accepted by internal edge APIs")
	}

	return cmd
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
