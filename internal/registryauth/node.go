// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package registryauth

import (
	"context"
	"crypto/sha256"
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

// ErrUnsupportedKubeletLayout reports that there is no agent-managed kubelet
// unit at the installer's node root, so the installer has nothing to configure.
// The installer is designed for a node whose kubelet is the agent-managed host
// kubelet (unit at <root>/etc/systemd/system/kubelet.service, restartable via
// `chroot <root> systemctl`). When that unit is absent the kubelet is not where
// the installer can reach it - for example, it may run inside an isolated node
// layout such as a systemd-nspawn machine, in a separate PID namespace under a
// different systemd manager - and no retry on this node root can succeed.
//
// Unlike ErrKubeletNotRunning (a transient "the kubelet is down, keep waiting"),
// this is a terminal condition for the managed path: the installer reports it as
// a failure instead of waiting forever. The probe logs the exact proc contents
// and unit state so the operator can confirm whether the node root is correct.
var ErrUnsupportedKubeletLayout = errors.New("no agent-managed kubelet unit at the node root; the kubelet is not where the managed installer can reach it; inspect the node root and /proc or use external authentication")

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

	// The revision keys the persisted restart budget to this policy, so a config
	// change resets the budget while a pod recreation on the same policy does not.
	revision := sha256.Sum256([]byte(opts.ConfigJSON))

	installer := &Installer{Root: opts.Root, Executable: opts.ProviderPath, Revision: fmt.Sprintf("%x", revision[:])}
	installer.HostCommand = func(ctx context.Context, args ...string) error {
		commandCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
		defer cancel()

		if err := exec.CommandContext(commandCtx, "chroot", append([]string{opts.Root, "/usr/bin/systemctl"}, args...)...).Run(); err != nil {
			return fmt.Errorf("node systemctl %s: %w", args[0], err)
		}

		return nil
	}
	installer.KubeletArgs = func() ([]string, error) { return RunningKubeletArgs(opts.Root) }

	// The layout probe gates installation only. External (teardown) mode is
	// ownership-aware and is a no-op on a node with no managed files, so it must
	// not be blocked by an unsupported layout: the supported detach operation has
	// to be able to converge on the very unsupported nodes this probe introduces.
	if cfg.Mode == IdentityBindingMode {
		if err := probeNodeLayout(ctx, opts.Root); err != nil {
			return err
		}
	}

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

// probeNodeLayout runs once at startup, before the reconcile loop, to turn the
// two failure shapes that used to be indistinguishable "no running kubelet
// found" into actionable ones:
//
//   - a missing agent kubelet unit means this node layout is not the one the
//     installer supports (the nspawn case), so it returns
//     ErrUnsupportedKubeletLayout, a terminal failure rather than an infinite
//     wait;
//   - a present unit with no running kubelet is logged with diagnostics (proc
//     mount state, candidate PID namespace, unit shape) so a real "kubelet is
//     down" can be distinguished from a wrong-root or namespace mismatch.
//
// It never mutates the node; it only reads and logs.
func probeNodeLayout(ctx context.Context, root string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	unit := filepath.Join(root, unitPath)

	data, err := os.ReadFile(unit)
	if errors.Is(err, os.ErrNotExist) {
		slog.Error("managed ACR authentication cannot operate this node: agent kubelet unit is absent",
			"root", root, "unit", unit,
			"hint", "kubelet is likely isolated (e.g. systemd-nspawn); use external authentication or run the node with a host kubelet")

		return fmt.Errorf("probe node layout: %w", ErrUnsupportedKubeletLayout)
	}

	if err != nil {
		return fmt.Errorf("probe node layout: read kubelet unit: %w", err)
	}

	args, err := RunningKubeletArgs(root)
	if errors.Is(err, ErrKubeletNotRunning) {
		slog.Warn("managed ACR authentication waiting for the kubelet to appear",
			"root", root, "unit", unit,
			"proc", probeProc(root),
			"unitShape", unitShape(data),
		)

		return nil
	}

	if err != nil {
		return fmt.Errorf("probe node layout: %w", err)
	}

	slog.Info("managed ACR authentication node layout OK", "root", root, "unit", unit, "kubeletArgs", len(args))

	return nil
}

// probeProc reports the procfs the installer scans, distinguishing "no kubelet
// running" from "wrong root / PID namespace" from "proc unreadable". It lists
// every candidate process name it could read and counts the read failures, so a
// log line shows whether the scanned namespace is empty (wrong root), populated
// but lacking a kubelet (down), or only partially readable (mount problem).
func probeProc(root string) string {
	proc := filepath.Join(root, "proc")

	entries, err := os.ReadDir(proc)
	if err != nil {
		return fmt.Sprintf("proc unreadable: %v", err)
	}

	kubelet, readable, unreadable := 0, 0, 0
	names := []string{}

	for _, entry := range entries {
		comm, err := os.ReadFile(filepath.Join(proc, entry.Name(), "comm"))
		if err != nil {
			unreadable++

			continue
		}

		// /proc/self, /proc/thread-self and the like are symlinks; only numeric
		// PIDs are real processes.
		first := entry.Name()[0]
		if first < '0' || first > '9' {
			readable++

			continue
		}

		name := strings.TrimSpace(string(comm))
		readable++

		if name == "kubelet" {
			kubelet++

			continue
		}

		if len(names) < 5 {
			names = append(names, name)
		}
	}

	return fmt.Sprintf("proc=%q readable=%d unreadable=%d kubelet=%d sample=%v", proc, readable, unreadable, kubelet, names)
}

// unitShape reports the ExecStart line of the kubelet unit (truncated) so a log
// line shows whether it is the agent-managed unit the installer expects.
func unitShape(unit []byte) string {
	for _, line := range strings.Split(string(unit), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ExecStart=") {
			if len(line) > 120 {
				return line[:120] + "..."
			}

			return line
		}
	}

	return "no-ExecStart"
}
