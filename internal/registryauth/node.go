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

	"k8s.io/apimachinery/pkg/util/version"
)

var ErrKubeletNotRunning = errors.New("no running kubelet found; waiting before configuring node authentication")

type NodeOptions struct {
	Root         string
	ConfigJSON   string
	Listen       string
	ProviderPath string
}

// RunNode installs configuration and upstream's executable. It never handles credentials.
func RunNode(ctx context.Context, opts NodeOptions) error {
	var cfg Config
	if err := json.Unmarshal([]byte(opts.ConfigJSON), &cfg); err != nil {
		return fmt.Errorf("invalid inline node-auth configuration: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	installer := &Installer{Root: opts.Root, Executable: opts.ProviderPath}
	installer.HostCommand = func(ctx context.Context, args ...string) error {
		commandCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()

		if err := exec.CommandContext(commandCtx, "chroot", append([]string{opts.Root, "/usr/bin/systemctl"}, args...)...).Run(); err != nil {
			return fmt.Errorf("node systemctl %s: %w", args[0], err)
		}

		return nil
	}
	installer.KubeletArgs = func() ([]string, error) { return RunningKubeletArgs(opts.Root) }

	return runNode(ctx, opts.Listen, cfg.Mode, func(reconcileCtx context.Context) error {
		if cfg.Mode == IdentityBindingMode {
			output, err := exec.CommandContext(reconcileCtx, "chroot", opts.Root, "/usr/local/bin/kubelet", "--version").Output()
			if err != nil {
				return fmt.Errorf("check kubelet version: %w", err)
			}

			fields := strings.Fields(string(output))
			if len(fields) == 0 {
				return errors.New("kubelet returned no version")
			}

			v, err := version.ParseGeneric(fields[len(fields)-1])
			if err != nil || !v.AtLeast(version.MustParseGeneric("1.34.0")) {
				return errors.New("AKS identity bindings require kubelet 1.34 or later with ServiceAccount tokens for credential providers enabled")
			}
		}

		return installer.Reconcile(reconcileCtx, cfg)
	})
}

func runNode(ctx context.Context, listen, mode string, reconcileNode func(context.Context) error) error {
	if ctx.Err() != nil {
		return nil
	}

	var health struct {
		sync.RWMutex
		ready   bool
		message string
	}

	health.message = "waiting for stock ACR provider installation"
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		health.RLock()
		defer health.RUnlock()

		if !health.ready {
			http.Error(w, health.message, http.StatusServiceUnavailable)
			return
		}

		_, _ = fmt.Fprintln(w, health.message) //nolint:errcheck // Probe responses are best effort.
	})
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }) //nolint:errcheck // Probe response.

	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer closeQuietly(server)

	errCh := make(chan error, 1)
	go func() { errCh <- server.Serve(listener) }()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	var lastMessage string

	for {
		if ctx.Err() != nil {
			return nil
		}

		message := "stock ACR credential provider configured; private pulls and Gantry acceleration are not yet verified"

		err := func() error {
			reconcileCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()

			if mode != IdentityBindingMode {
				message = "operator-owned provider removed; customer authentication is unchanged"
			}

			return reconcileNode(reconcileCtx)
		}()
		if err != nil {
			message = err.Error()
		}

		health.Lock()
		health.ready, health.message = err == nil, message
		health.Unlock()

		if message != lastMessage {
			if err != nil {
				slog.Error("stock ACR provider installation is not ready", "reason", message)
			} else {
				slog.Info("stock ACR provider reconciled", "status", message)
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
			return nil, fmt.Errorf("read running kubelet arguments: %w", err)
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
