// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// RestartLeaseName is created by the operator. Node helpers can only get/update it.
const RestartLeaseName = "gantry-acr-auth-restart"

// ErrKubeletNotRunning permits recovery only after an owned installation exists.
var ErrKubeletNotRunning = errors.New("no running kubelet found; waiting before configuring node authentication")

// NodeOptions selects the local mount, projected configuration, and assertion.
type NodeOptions struct {
	Root       string
	ConfigPath string
	Listen     string
	Namespace  string
	Identity   string
	TokenFile  string
}

// RunNode reconciles a node-wide broker and exec adapter. The broker's own
// infrastructure ServiceAccount supplies the assertion. Applications are untouched.
func RunNode(ctx context.Context, opts NodeOptions) error {
	if opts.Identity == "" || opts.Namespace == "" {
		return errors.New("node-auth requires a pod identity and namespace")
	}

	if opts.TokenFile == "" {
		opts.TokenFile = FederatedTokenPath
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	executable, err := os.Executable()
	if err != nil {
		return err
	}

	installer := &Installer{Root: opts.Root, Executable: executable}
	installer.HostCommand = func(ctx context.Context, args ...string) error {
		commandCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()

		command := exec.CommandContext(commandCtx, "chroot", append([]string{opts.Root, "/usr/bin/systemctl"}, args...)...)
		if err := command.Run(); err != nil {
			return fmt.Errorf("host systemctl %s failed: %w", args[0], err)
		}

		return nil
	}
	installer.KubeletArgs = func() ([]string, error) { return RunningKubeletArgs(opts.Root) }

	var health struct {
		sync.RWMutex
		ready   bool
		message string
	}

	health.message = "waiting for federated node authentication reconciliation"
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		health.RLock()
		defer health.RUnlock()

		if !health.ready {
			http.Error(w, health.message, http.StatusServiceUnavailable)
			return
		}

		_, _ = fmt.Fprintln(w, health.message) //nolint:errcheck // Probe responses are best effort after headers are sent.
	})
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }) //nolint:errcheck // Probe responses are best effort.

	listener, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return err
	}

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer closeQuietly(server)

	brokerListener, err := ListenBroker(opts.Root)
	if err != nil {
		closeQuietly(listener)
		return err
	}

	var broker Broker

	brokerServer := &http.Server{Handler: &broker, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 35 * time.Second, MaxHeaderBytes: 8192}
	defer closeQuietly(brokerServer)

	errCh := make(chan error, 2)
	go func() { errCh <- server.Serve(listener) }()
	go func() { errCh <- brokerServer.Serve(brokerListener) }()

	var (
		provider                                    *Provider
		providerIdentity, lastVerified, lastMessage string
		verifiedAt                                  time.Time
	)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		message := "federated node broker and credential adapter active; repository pull permission is unverified (set node-auth probeImage)"

		err := func() error {
			reconcileCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()

			data, err := os.ReadFile(opts.ConfigPath)
			if err != nil {
				return err
			}

			var cfg Config
			if json.Unmarshal(data, &cfg) != nil {
				return errors.New("invalid projected node-auth configuration")
			}

			if cfg.Mode != "external" && cfg.Mode != "workloadIdentity" {
				return errors.New("node-auth requires explicit workloadIdentity or external mode")
			}

			if cfg.Mode == "external" || len(cfg.Registries) == 0 {
				broker.Configure(Config{}, nil)

				message = "operator-owned credential adapter removed; authentication is externally managed"

				return withRestartLease(reconcileCtx, client, opts, func(ctx context.Context) error { return installer.Reconcile(ctx, cfg) })
			}

			identity := cfg.ClientID + "/" + cfg.TenantID
			if provider == nil || providerIdentity != identity {
				provider, err = NewProvider(cfg, opts.TokenFile)
				if err != nil {
					return err
				}

				providerIdentity = identity
			}
			// Validate federation before changing kubelet. There is deliberately
			// no fallback to a VM identity when assertion exchange fails.
			if string(data) != lastVerified || time.Since(verifiedAt) >= 5*time.Minute {
				if cfg.ProbeImage != "" {
					if err := provider.Probe(reconcileCtx, cfg.ProbeImage); err != nil {
						return err
					}
				} else {
					for _, registry := range cfg.Registries {
						if _, err := provider.RefreshToken(reconcileCtx, registry); err != nil {
							return err
						}
					}
				}

				lastVerified, verifiedAt = string(data), time.Now()
			}

			broker.Configure(cfg, provider)

			if err := withRestartLease(reconcileCtx, client, opts, func(ctx context.Context) error { return installer.Reconcile(ctx, cfg) }); err != nil {
				return err
			}

			if cfg.ProbeImage != "" {
				message = "federated node broker and credential adapter active; private-image authorization verified by manifest HEAD"
			}

			return nil
		}()
		if err != nil {
			message = err.Error()

			broker.Configure(Config{}, nil)
		}

		health.Lock()
		health.ready, health.message = err == nil, message
		health.Unlock()

		if message != lastMessage {
			if err != nil {
				slog.Error("registry authentication is not ready", "reason", message)
			} else {
				slog.Info("registry authentication reconciled", "status", message)
			}

			lastMessage = message
		}

		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}

			return nil
		case <-ticker.C:
		}
	}
}

// RunningKubeletArgs reads only arguments, never credentials or environment.
func RunningKubeletArgs(root string) ([]string, error) {
	proc := filepath.Join(root, "proc")

	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}

	var result []string

	for _, entry := range entries {
		comm, err := os.ReadFile(filepath.Join(proc, entry.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "kubelet" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(proc, entry.Name(), "cmdline"))
		if err != nil {
			continue
		}

		if result != nil {
			return nil, errors.New("multiple kubelet processes found; refusing ambiguous node configuration")
		}

		result = strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	}

	if result == nil {
		return nil, ErrKubeletNotRunning
	}

	return result, nil
}

func withRestartLease(ctx context.Context, client kubernetes.Interface, opts NodeOptions, run func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	result := make(chan error, 1)
	started := make(chan struct{})

	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          &resourcelock.LeaseLock{LeaseMeta: metav1.ObjectMeta{Name: RestartLeaseName, Namespace: opts.Namespace}, Client: client.CoordinationV1(), LockConfig: resourcelock.ResourceLockConfig{Identity: opts.Identity}},
		LeaseDuration: 60 * time.Second, RenewDeadline: 40 * time.Second, RetryPeriod: 5 * time.Second, ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				close(started)

				if err := ctx.Err(); err != nil {
					result <- err
				} else {
					result <- run(ctx)
				}

				cancel()
			},
			OnStoppedLeading: func() {},
		},
	})
	if err != nil {
		return err
	}

	elector.Run(ctx)

	select {
	case <-started:
		return <-result
	default:
		return errors.New("node-auth restart coordination interrupted; waiting for the shared restart Lease")
	}
}
