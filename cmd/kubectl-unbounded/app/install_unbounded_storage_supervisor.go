// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
	storagesupervisordeploy "github.com/Azure/unbounded/deploy/unbounded-storage-supervisor"
	"github.com/Azure/unbounded/internal/kube"
)

const (
	unboundedStorageSupervisorNamespace     = "unbounded-kube"
	unboundedStorageSupervisorDaemonSetName = "unbounded-storage-supervisor"
)

// installUnboundedStorageSupervisor installs the unbounded-storage supervisor
// manifests and waits for the supervisor DaemonSet to roll out on all nodes.
type installUnboundedStorageSupervisor struct {
	*kubeComponentInstaller

	daemonSetName string
	siteName      string
}

func newInstallUnboundedStorageSupervisor(fileOrURL, siteName string, httpClient *http.Client, logger *slog.Logger, kubeResourcesCli client.Client, kubeCli kubernetes.Interface) *installUnboundedStorageSupervisor {
	inst := &kubeComponentInstaller{
		fileOrURL:        fileOrURL,
		httpClient:       httpClient,
		logger:           logger,
		kubeResourcesCli: kubeResourcesCli,
		kubeCli:          kubeCli,
		namespace:        unboundedStorageSupervisorNamespace,
		controllerName:   unboundedStorageSupervisorDaemonSetName,
		waitTimeout:      5 * time.Minute,
		pollInterval:     5 * time.Second,
		tempPrefix:       "unbounded-storage-supervisor",
	}

	// When no explicit manifests path/URL is provided, fall back to the
	// manifests embedded in the binary from deploy/unbounded-storage-supervisor/.
	if fileOrURL == "" {
		inst.embeddedFS = storagesupervisordeploy.Manifests
	}

	return &installUnboundedStorageSupervisor{
		kubeComponentInstaller: inst,
		daemonSetName:          unboundedStorageSupervisorDaemonSetName,
		siteName:               siteName,
	}
}

func (i *installUnboundedStorageSupervisor) run(ctx context.Context) error {
	manifestDir, err := i.resolveManifests()
	if err != nil {
		return fmt.Errorf("resolving manifests: %w", err)
	}

	defer func() {
		if err := os.RemoveAll(manifestDir); err != nil {
			i.logger.Warn("failed to clean up temp manifest directory", "path", manifestDir, "error", err)
		}
	}()

	if err := i.applySiteNodeSelector(manifestDir); err != nil {
		return fmt.Errorf("setting storage supervisor node selector: %w", err)
	}

	if err := kube.ApplyManifestsInDirectory(ctx, i.logger, i.kubeResourcesCli, fieldManagerID, manifestDir, i.skipPaths); err != nil {
		return fmt.Errorf("applying manifests: %w", err)
	}

	if err := i.waitForDaemonSetRollout(ctx); err != nil {
		return fmt.Errorf("waiting for %s DaemonSet to roll out: %w", i.daemonSetName, err)
	}

	return nil
}

func (i *installUnboundedStorageSupervisor) applySiteNodeSelector(manifestDir string) error {
	if i.siteName == "" {
		return nil
	}

	patched := false

	err := filepath.WalkDir(manifestDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || !isYAMLManifest(path) {
			return nil
		}

		filePatched, err := setDaemonSetSiteNodeSelector(path, i.daemonSetName, i.siteName)
		if err != nil {
			return err
		}

		patched = patched || filePatched

		return nil
	})
	if err != nil {
		return err
	}

	if !patched {
		return fmt.Errorf("storage supervisor DaemonSet %q not found in manifests", i.daemonSetName)
	}

	return nil
}

func setDaemonSetSiteNodeSelector(path, daemonSetName, siteName string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", path, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))

	var docs []map[string]any

	patched := false

	for {
		var doc map[string]any

		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return false, fmt.Errorf("decoding %s: %w", path, err)
		}

		if doc == nil {
			continue
		}

		if isDaemonSetManifest(doc, daemonSetName) {
			if err := setPodTemplateNodeSelector(doc, netv1alpha1.SiteLabelKey, siteName); err != nil {
				return false, fmt.Errorf("patching %s: %w", path, err)
			}

			patched = true
		}

		docs = append(docs, doc)
	}

	if !patched {
		return false, nil
	}

	var buf bytes.Buffer

	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)

	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return false, fmt.Errorf("encoding %s: %w", path, err)
		}
	}

	if err := enc.Close(); err != nil {
		return false, fmt.Errorf("closing encoder for %s: %w", path, err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}

	return true, nil
}

func isYAMLManifest(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

func isDaemonSetManifest(doc map[string]any, name string) bool {
	if doc["kind"] != "DaemonSet" {
		return false
	}

	metadata, ok := doc["metadata"].(map[string]any)
	if !ok {
		return false
	}

	return metadata["name"] == name
}

func setPodTemplateNodeSelector(doc map[string]any, key, value string) error {
	spec, err := ensureStringMap(doc, "spec")
	if err != nil {
		return err
	}

	template, err := ensureStringMap(spec, "template")
	if err != nil {
		return err
	}

	templateSpec, err := ensureStringMap(template, "spec")
	if err != nil {
		return err
	}

	nodeSelector, err := ensureStringMap(templateSpec, "nodeSelector")
	if err != nil {
		return err
	}

	nodeSelector[key] = value

	return nil
}

func ensureStringMap(parent map[string]any, key string) (map[string]any, error) {
	value, ok := parent[key]
	if !ok || value == nil {
		m := map[string]any{}
		parent[key] = m

		return m, nil
	}

	m, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a map", key)
	}

	return m, nil
}

func (i *installUnboundedStorageSupervisor) waitForDaemonSetRollout(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, i.timeout())
	defer cancel()

	ticker := time.NewTicker(i.interval())
	defer ticker.Stop()

	var lastStatus appsv1.DaemonSetStatus

	for {
		ds, err := i.kubeCli.AppsV1().DaemonSets(i.namespace).Get(ctx, i.daemonSetName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("getting DaemonSet %s/%s: %w", i.namespace, i.daemonSetName, err)
		}

		if err == nil {
			lastStatus = ds.Status
			if daemonSetRolledOut(ds) {
				i.logger.Info("DaemonSet is rolled out", "daemonset", i.daemonSetName, "namespace", i.namespace)
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf(
				"timed out waiting for DaemonSet %s/%s rollout: ready %d/%d, updated %d/%d",
				i.namespace,
				i.daemonSetName,
				lastStatus.NumberReady,
				lastStatus.DesiredNumberScheduled,
				lastStatus.UpdatedNumberScheduled,
				lastStatus.DesiredNumberScheduled,
			)
		case <-ticker.C:
		}
	}
}

func daemonSetRolledOut(ds *appsv1.DaemonSet) bool {
	if ds.Status.ObservedGeneration < ds.Generation {
		return false
	}

	if ds.Status.DesiredNumberScheduled == 0 {
		return false
	}

	return ds.Status.UpdatedNumberScheduled == ds.Status.DesiredNumberScheduled &&
		ds.Status.NumberReady == ds.Status.DesiredNumberScheduled
}
