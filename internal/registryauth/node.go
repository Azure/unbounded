// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// ErrKubeletNotRunning distinguishes a stopped service from an ambiguous process
// or an unreadable proc filesystem, allowing owned-installation recovery only.
var ErrKubeletNotRunning = errors.New("no running kubelet found; waiting before configuring node authentication")

// NodeOptions selects the node-local mount, projected config, and health listener.
type NodeOptions struct {
	Root       string
	ConfigPath string
	Listen     string
	Namespace  string
	Identity   string
}

// RunNode continuously repairs owned files. Pod termination never removes host
// configuration: explicit external/empty desired state performs safe removal.
func RunNode(ctx context.Context, opts NodeOptions) error {
	if opts.Identity == "" || opts.Namespace == "" {
		return errors.New("node-auth requires a pod identity and namespace")
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
		ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()

		command := exec.CommandContext(ctx, "chroot", append([]string{opts.Root, "/usr/bin/systemctl"}, args...)...)
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

	health.message = "waiting for node authentication reconciliation"
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
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok")) //nolint:errcheck // Probe responses are best effort after headers are sent.
	})
	server := &http.Server{Addr: opts.Listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()

	defer closeQuietly(server)

	var (
		provider                       *Provider
		providerClientID, lastVerified string
		verifiedAt                     time.Time
		lastMessage                    string
	)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		message := "credential provider active; private repository pull permission is unverified (set node-auth probeImage)"

		err := func() error {
			reconcileCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()

			data, err := os.ReadFile(opts.ConfigPath)
			if err != nil {
				return err
			}

			var cfg Config
			if err := json.Unmarshal(data, &cfg); err != nil {
				return errors.New("invalid projected node-auth configuration")
			}

			if err := withRestartLease(reconcileCtx, client, opts, func(ctx context.Context) error { return installer.Reconcile(ctx, cfg) }); err != nil {
				return err
			}

			if cfg.Mode == "external" || len(cfg.Registries) == 0 {
				message = "operator-owned credential provider removed; authentication is externally managed"
				return nil
			}

			if provider == nil || providerClientID != cfg.ClientID {
				provider, err = NewProvider(cfg.ClientID)
				if err != nil {
					return err
				}

				providerClientID = cfg.ClientID
			}

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

			if cfg.ProbeImage != "" {
				message = "credential provider active; private-image pull authorization verified by manifest HEAD"
			}

			return nil
		}()
		if err != nil {
			message = err.Error()
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

// RunningKubeletArgs reads only kubelet's arguments, never its credentials or environment.
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
		// Do not begin another pass while a canceled host operation is still
		// unwinding. The host command receives the leadership context.
		return <-result
	default:
		return errors.New("node-auth restart coordination interrupted; waiting for the shared restart Lease")
	}
}
