// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package commands

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/Azure/unbounded/internal/clusterinfo"
	"github.com/Azure/unbounded/internal/metalman/netboot"
)

const (
	// apiserverURLOverrideEnv, when set, overrides the API server URL
	// discovered from the cluster-info ConfigMap in kube-public. The CA
	// certificate comes from the ConfigMap when available, otherwise from the
	// in-cluster service-account mount. The operator injects this with the
	// endpoint it resolved; it is also useful in test environments (e.g. kind)
	// where the ConfigMap contains a hostname unreachable from provisioned
	// nodes, and on managed control planes (e.g. AKS) that do not publish
	// cluster-info at all.
	apiserverURLOverrideEnv = "METALMAN_APISERVER_URL"
)

// ClusterInfoWatcher watches the cluster-info ConfigMap in the kube-public
// namespace and provides up-to-date API server URL and CA certificate to
// the FileResolver through the ClusterInfoProvider interface.
//
// It uses a shared informer so that changes at runtime (e.g. API server
// URL rotation) are picked up automatically.
type ClusterInfoWatcher struct {
	clientset            kubernetes.Interface
	log                  *slog.Logger
	apiserverURLOverride string

	// inClusterCA reads the fallback CA when cluster-info is unavailable. It is
	// a field so tests can inject a fixture; it defaults to
	// clusterinfo.InClusterCA.
	inClusterCA func() ([]byte, error)

	mu   sync.RWMutex
	info netboot.ClusterInfo
}

// NewClusterInfoWatcher creates a watcher that resolves the API server URL
// and CA certificate from the cluster-info ConfigMap in kube-public. It
// performs an initial synchronous resolve so that values are available
// before the first template render.
//
// If the METALMAN_APISERVER_URL environment variable is set, its value
// overrides the API server URL from the ConfigMap on every refresh. When
// cluster-info is unavailable (e.g. AKS), the API server URL comes from that
// override and the CA from the in-cluster service-account mount.
func NewClusterInfoWatcher(
	ctx context.Context,
	clientset kubernetes.Interface,
	log *slog.Logger,
) (*ClusterInfoWatcher, error) {
	w := &ClusterInfoWatcher{
		clientset:            clientset,
		log:                  log,
		apiserverURLOverride: os.Getenv(apiserverURLOverrideEnv),
		inClusterCA:          clusterinfo.InClusterCA,
	}

	if w.apiserverURLOverride != "" {
		log.Info("using API server URL override", "env", apiserverURLOverrideEnv, "url", w.apiserverURLOverride)
	}

	// Perform initial synchronous resolve so values are available
	// before the manager starts serving requests.
	if err := w.refresh(ctx); err != nil {
		return nil, fmt.Errorf("initial cluster-info resolve: %w", err)
	}

	return w, nil
}

// ClusterInfo returns the current cluster-info snapshot.
// Safe for concurrent use.
func (w *ClusterInfoWatcher) ClusterInfo() netboot.ClusterInfo {
	w.mu.RLock()
	defer w.mu.RUnlock()

	return w.info
}

// Start implements manager.Runnable. It sets up an informer for the
// cluster-info ConfigMap in kube-public and re-resolves the API server
// URL and CA certificate whenever that ConfigMap changes.
func (w *ClusterInfoWatcher) Start(ctx context.Context) error {
	// Watch ConfigMaps in kube-public (for cluster-info).
	pubFactory := informers.NewSharedInformerFactoryWithOptions(
		w.clientset, 5*time.Minute,
		informers.WithNamespace(metav1.NamespacePublic),
	)

	cmInformer := pubFactory.Core().V1().ConfigMaps().Informer()
	if _, err := cmInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { w.refreshQuiet(ctx) },
		UpdateFunc: func(_, _ interface{}) { w.refreshQuiet(ctx) },
	}); err != nil {
		return fmt.Errorf("adding ConfigMap event handler: %w", err)
	}

	pubFactory.Start(ctx.Done())
	pubFactory.WaitForCacheSync(ctx.Done())

	w.log.Info("cluster-info watcher started")
	<-ctx.Done()

	return nil
}

// refresh re-resolves the cluster-info from the Kubernetes API.
//
// The API server URL comes from the METALMAN_APISERVER_URL override when set
// (the operator injects the endpoint it resolved), otherwise from
// kube-public/cluster-info. The CA comes from cluster-info when available and
// otherwise from the in-cluster service-account mount, so metalman still works
// on managed control planes (e.g. AKS) that do not publish cluster-info.
func (w *ClusterInfoWatcher) refresh(ctx context.Context) error {
	var (
		apiserverURL string
		caPEM        []byte
	)

	if resolved, err := clusterinfo.Resolve(ctx, w.clientset); err != nil {
		w.log.Info("cluster-info unavailable; relying on overrides and the in-cluster CA", "error", err)
	} else {
		apiserverURL = resolved.ApiserverURL
		caPEM = resolved.CACertPEM
	}

	if w.apiserverURLOverride != "" {
		apiserverURL = w.apiserverURLOverride
	}

	if len(caPEM) == 0 {
		readCA := w.inClusterCA
		if readCA == nil {
			readCA = clusterinfo.InClusterCA
		}

		ca, err := readCA()
		if err != nil {
			return fmt.Errorf("no CA available: cluster-info absent and reading the in-cluster CA failed: %w", err)
		}

		caPEM = ca
	}

	if apiserverURL == "" {
		return fmt.Errorf("no API server URL available: set %s or publish kube-public/cluster-info", apiserverURLOverrideEnv)
	}

	info := netboot.ClusterInfo{
		ApiserverURL: apiserverURL,
		CACertBase64: base64.StdEncoding.EncodeToString(caPEM),
	}

	w.mu.Lock()
	changed := w.info != info
	w.info = info
	w.mu.Unlock()

	if changed {
		w.log.Info("cluster-info refreshed",
			"apiserverURL", apiserverURL,
		)
	}

	return nil
}

// refreshQuiet re-resolves the cluster-info, logging any errors instead of
// returning them (suitable for informer event handlers).
func (w *ClusterInfoWatcher) refreshQuiet(ctx context.Context) {
	if err := w.refresh(ctx); err != nil {
		w.log.Warn("failed to refresh cluster-info", "error", err)
	}
}
