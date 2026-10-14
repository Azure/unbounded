// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/ipam"
	ipamproto "github.com/Azure/unbounded/internal/net/ipam/proto"
)

func registerIPAMFlags(flags *pflag.FlagSet, cfg *config) {
	flags.StringVar(&cfg.IPAMSocketPath, "ipam-socket-path", ipam.DefaultSocketPath, "Unix socket shared with unbounded-ipam on the host")
	flags.BoolVar(&cfg.InstallIPAMPlugin, "install-ipam-plugin", true, "Install or upgrade the bundled unbounded-ipam binary")
	flags.StringVar(&cfg.IPAMBinDir, "ipam-bin-dir", "/host/opt/cni/bin", "Mounted host CNI binary directory for unbounded-ipam installation")
}

func validateIPAMConfig(cfg *config) error {
	if cfg.IPAMSocketPath == "" {
		cfg.IPAMSocketPath = ipam.DefaultSocketPath
	}

	if err := ipam.ValidateSocketPath(cfg.IPAMSocketPath); err != nil {
		return err
	}

	if cfg.InstallIPAMPlugin && (!filepath.IsAbs(cfg.IPAMBinDir) || filepath.Clean(cfg.IPAMBinDir) != cfg.IPAMBinDir) {
		return fmt.Errorf("IPAM binary directory must be a clean absolute path: %q", cfg.IPAMBinDir)
	}

	return nil
}

func (h *nodeHealthState) setVerifiedIPAMAssignment(cidrs []string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.verifiedIPAMCIDRs = slices.Clone(cidrs)
}

func (h *nodeHealthState) getIPAMConfig() (*ipamproto.Config, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.cniManaged && !h.cniReady {
		return nil, fmt.Errorf("managed CNI is not ready: %s", h.cniReason)
	}

	return ipam.ConfigFromCIDRs(h.verifiedIPAMCIDRs)
}

func acquireDelegatedIPAMAssignment(ctx context.Context, client kubernetes.Interface, cfg *config, health *nodeHealthState) {
	for {
		node, err := client.CoreV1().Nodes().Get(ctx, cfg.NodeName, metav1.GetOptions{})
		if err == nil {
			_, err = ipam.ConfigFromCIDRs(node.Spec.PodCIDRs)
			if err == nil {
				health.setVerifiedIPAMAssignment(node.Spec.PodCIDRs)
				klog.Infof("Verified delegated IPAM PodCIDRs for node %s: %v", cfg.NodeName, node.Spec.PodCIDRs)

				return
			}
		}

		if ctx.Err() != nil {
			return
		}

		klog.Warningf("Delegated IPAM assignment pending for node %s: %v", cfg.NodeName, err)

		if err := waitForCNIRetry(ctx, cfg.cniRetryInterval); err != nil {
			return
		}
	}
}

func installIPAMBinary(source, directory string) (returnErr error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return fmt.Errorf("invalid IPAM install directory %q", directory)
	}

	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect mounted IPAM binary directory: %w", err)
	}

	if !info.IsDir() {
		return fmt.Errorf("IPAM binary destination %s is not a directory", directory)
	}

	destination := filepath.Join(directory, "unbounded-ipam")
	if targetInfo, err := os.Lstat(destination); err == nil {
		if !targetInfo.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular IPAM binary %s", destination)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, input.Close()) }()

	info, err = input.Stat()
	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("bundled IPAM binary %s is not executable", source)
	}

	output, err := os.CreateTemp(directory, ".unbounded-ipam-*")
	if err != nil {
		return err
	}

	closed := false
	defer func() {
		if !closed {
			returnErr = errors.Join(returnErr, output.Close())
		}

		if err := os.Remove(output.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			returnErr = errors.Join(returnErr, err)
		}
	}()

	if _, err := io.Copy(output, input); err != nil {
		return err
	}

	if err := output.Chmod(0o755); err != nil {
		return err
	}

	if err := output.Sync(); err != nil {
		return err
	}

	err = output.Close()
	closed = true

	if err != nil {
		return err
	}

	if err := os.Rename(output.Name(), destination); err != nil {
		return err
	}

	directoryFile, err := os.Open(directory)
	if err != nil {
		return err
	}

	defer func() { returnErr = errors.Join(returnErr, directoryFile.Close()) }()

	return directoryFile.Sync()
}
