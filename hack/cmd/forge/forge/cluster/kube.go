// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"k8s.io/client-go/kubernetes"

	"github.com/Azure/unbounded/hack/cmd/forge/forge/kube"
)

func applyBootstrapToken(ctx context.Context, logger *slog.Logger, kubeCli kubernetes.Interface, kubectl func(context.Context) *exec.Cmd, forgeName string, dataDir DataDir) (*kube.BootstrapToken, error) {
	// Try to find existing bootstrap token secret with matching site label
	logger.Info("Checking for existing bootstrap token", "forge", forgeName)

	existingToken, err := kube.GetBootstrapToken(ctx, kubeCli)
	if err != nil && !errors.Is(err, kube.ErrBootstrapTokenNotFound) {
		return nil, fmt.Errorf("get existing bootstrap token: %w", err)
	}

	if existingToken != nil {
		logger.Info("Found existing bootstrap token", "tokenID", existingToken.ID)
		return existingToken, nil
	}

	// No existing token found, create a new one
	logger.Info("No existing bootstrap token found, creating new one")

	id, token, err := kube.GenerateBootstrapIDAndToken()
	if err != nil {
		return nil, fmt.Errorf("generate bootstrap token: %w", err)
	}

	// Create the bootstrap token manifest with forge label (using resource group name)
	bootstrapTokenManifest := kube.BootstrapTokenManifest(id, token)

	// Write the manifest to the hack directory
	bootstrapTokenPath := dataDir.UnboundedForgePath("bootstrap-token.yaml")
	if err := os.MkdirAll(filepath.Dir(bootstrapTokenPath), 0o755); err != nil {
		return nil, fmt.Errorf("create directory for bootstrap token manifest: %w", err)
	}

	if err := os.WriteFile(bootstrapTokenPath, []byte(bootstrapTokenManifest), 0o644); err != nil {
		return nil, fmt.Errorf("write bootstrap token manifest: %w", err)
	}

	logger.Info("Installing bootstrap token", "path", bootstrapTokenPath)

	if err := kube.ApplyManifests(ctx, logger, kubectl, bootstrapTokenPath); err != nil {
		return nil, fmt.Errorf("apply bootstrap token: %w", err)
	}

	return &kube.BootstrapToken{ID: id, Secret: token}, nil
}

type DataDir struct {
	Root           string
	UnboundedForge string
	Site           string
}

func (d *DataDir) UnboundedForgePath(extra ...string) string {
	b := d.Root

	if d.UnboundedForge != "" {
		b = filepath.Join(b, d.UnboundedForge)
	}

	for _, ex := range extra {
		b = filepath.Join(b, ex)
	}

	return b
}

func (d *DataDir) SitePath(extra ...string) string {
	return d.UnboundedForgePath(append([]string{d.Site}, extra...)...)
}

func saveKubeconfig(kubeconfigPath string, kubeconfigData []byte) (string, error) {
	dir := filepath.Dir(kubeconfigPath)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create directory: %w", err)
	}

	if err := os.WriteFile(kubeconfigPath, kubeconfigData, 0o600); err != nil {
		return "", fmt.Errorf("write kubeconfig file: %w", err)
	}

	return kubeconfigPath, nil
}
