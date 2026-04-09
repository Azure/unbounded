// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"archive/tar"
	"compress/gzip"
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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded-kube/internal/kube"
)

const (
	defaultWaitTimeout  = 5 * time.Minute
	defaultPollInterval = 5 * time.Second
)

// kubeComponentInstaller is a generic installer for Kubernetes components
// distributed as a directory, tar.gz archive, or remote URL. It resolves
// the manifests, applies them to the cluster, and waits for a controller
// pod to reach the Running phase.
type kubeComponentInstaller struct {
	// fileOrURL is the path to a directory, tar.gz archive, or https:// URL
	// containing the component manifests. When empty, embeddedFS is used
	// instead.
	fileOrURL string
	// embeddedFS is an optional embedded filesystem containing the component
	// manifests. It is used as a fallback when fileOrURL is empty.
	embeddedFS fs.FS
	// httpClient is used when fileOrURL is an https:// URL. If nil,
	// http.DefaultClient is used.
	httpClient *http.Client
	// logger is used for informational and warning messages.
	logger *slog.Logger
	// kubeResourcesCli is the controller-runtime client used for server-side
	// apply of manifests.
	kubeResourcesCli client.Client
	// kubeCli is the kubernetes client interface used to poll for the
	// controller pod.
	kubeCli kubernetes.Interface
	// namespace is the namespace the controller pod runs in.
	namespace string
	// controllerName is the prefix of the controller pod name to wait for.
	controllerName string
	// waitTimeout is the maximum duration to wait for the controller pod.
	// Defaults to defaultWaitTimeout if zero.
	waitTimeout time.Duration
	// pollInterval is the interval between controller pod polls. Defaults to
	// defaultPollInterval if zero.
	pollInterval time.Duration
	// tempPrefix is the prefix used for temp files and directories created
	// during manifest resolution (e.g. "unbounded-net", "machina").
	tempPrefix string

	skipPaths []string
}

func (i *kubeComponentInstaller) client() *http.Client {
	if i.httpClient != nil {
		return i.httpClient
	}

	return http.DefaultClient
}

func (i *kubeComponentInstaller) timeout() time.Duration {
	if i.waitTimeout > 0 {
		return i.waitTimeout
	}

	return defaultWaitTimeout
}

func (i *kubeComponentInstaller) interval() time.Duration {
	if i.pollInterval > 0 {
		return i.pollInterval
	}

	return defaultPollInterval
}

func (i *kubeComponentInstaller) tempPattern() string {
	return i.tempPrefix + "-*"
}

func (i *kubeComponentInstaller) tempArchivePattern() string {
	return i.tempPrefix + "-*.tar.gz"
}

// run resolves fileOrURL to a local directory of manifests, applies them to the
// cluster, and waits for the controller pod to become Running.
func (i *kubeComponentInstaller) run(ctx context.Context) error {
	manifestDir, err := i.resolveManifests()
	if err != nil {
		return fmt.Errorf("resolving manifests: %w", err)
	}

	defer func() {
		if err := os.RemoveAll(manifestDir); err != nil {
			i.logger.Warn("failed to clean up temp manifest directory", "path", manifestDir, "error", err)
		}
	}()

	if err := kube.ApplyManifestsInDirectory(ctx, i.logger, i.kubeResourcesCli, fieldManagerID, manifestDir, i.skipPaths); err != nil {
		return fmt.Errorf("applying manifests: %w", err)
	}

	if err := i.waitForController(ctx); err != nil {
		return fmt.Errorf("waiting for %s to become running: %w", i.controllerName, err)
	}

	return nil
}

// resolveManifests resolves fileOrURL to a local temp directory containing the
// manifest files. When fileOrURL is empty and embeddedFS is set, the embedded
// files are materialized to a temp directory. The caller is responsible for
// cleaning up the returned directory.
func (i *kubeComponentInstaller) resolveManifests() (string, error) {
	if i.fileOrURL == "" && i.embeddedFS != nil {
		return i.materializeEmbeddedFS()
	}

	if strings.HasPrefix(i.fileOrURL, "https://") {
		return i.downloadAndExtract()
	}

	info, err := os.Stat(i.fileOrURL)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", i.fileOrURL, err)
	}

	if info.IsDir() {
		return i.copyDirectory()
	}

	return i.extractArchive(i.fileOrURL)
}

// materializeEmbeddedFS writes the contents of embeddedFS to a temp directory
// and returns its path. The caller is responsible for cleaning up the returned
// directory.
func (i *kubeComponentInstaller) materializeEmbeddedFS() (string, error) {
	destDir, err := os.MkdirTemp("", i.tempPattern())
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}

	err = fs.WalkDir(i.embeddedFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, path)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		content, err := fs.ReadFile(i.embeddedFS, path)
		if err != nil {
			return fmt.Errorf("reading embedded file %s: %w", path, err)
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("creating parent directory for %s: %w", target, err)
		}

		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		if rmErr := os.RemoveAll(destDir); rmErr != nil {
			return "", errors.Join(fmt.Errorf("materializing embedded FS: %w", err), rmErr)
		}

		return "", fmt.Errorf("materializing embedded FS: %w", err)
	}

	return destDir, nil
}

// waitForController polls until a pod whose name starts with controllerName in
// the configured namespace is running with all containers ready, or until the
// context is cancelled or the timeout elapses.
func (i *kubeComponentInstaller) waitForController(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, i.timeout())
	defer cancel()

	ticker := time.NewTicker(i.interval())
	defer ticker.Stop()

	for {
		pods, err := i.kubeCli.CoreV1().Pods(i.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("listing pods in %s: %w", i.namespace, err)
		}

		for idx := range pods.Items {
			pod := &pods.Items[idx]
			if strings.HasPrefix(pod.Name, i.controllerName) && isPodReady(pod) {
				i.logger.Info("controller is running", "pod", pod.Name, "namespace", i.namespace)
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s pod in namespace %s", i.controllerName, i.namespace)
		case <-ticker.C:
		}
	}
}

// isPodReady returns true when the pod is in the Running phase and every
// container reports Ready. This avoids falsely treating a pod that is in
// CrashLoopBackOff (phase is still Running) as healthy.
func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}

	for idx := range pod.Status.ContainerStatuses {
		if !pod.Status.ContainerStatuses[idx].Ready {
			return false
		}
	}

	return true
}

func (i *kubeComponentInstaller) downloadAndExtract() (retDir string, retErr error) {
	resp, err := i.client().Get(i.fileOrURL)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", i.fileOrURL, err)
	}

	defer func() {
		retErr = errors.Join(retErr, resp.Body.Close())
	}()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: HTTP %d", i.fileOrURL, resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", i.tempArchivePattern())
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}

	tmpName := tmp.Name()

	defer func() {
		rmErr := os.Remove(tmpName)
		if rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			retErr = errors.Join(retErr, rmErr)
		}
	}()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		if closeErr := tmp.Close(); closeErr != nil {
			return "", fmt.Errorf("writing download to temp file: %w (close: %w)", err, closeErr)
		}

		return "", fmt.Errorf("writing download to temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing temp file: %w", err)
	}

	return i.extractArchive(tmpName)
}

func (i *kubeComponentInstaller) extractArchive(archivePath string) (retDir string, retErr error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("opening archive %s: %w", archivePath, err)
	}

	defer func() {
		if err := f.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing archive: %w", err))
		}
	}()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("creating gzip reader: %w", err)
	}

	defer func() {
		if err := gr.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing gzip reader: %w", err))
		}
	}()

	destDir, err := os.MkdirTemp("", i.tempPattern())
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}

	cleanupOnErr := func(primary error) error {
		if err := os.RemoveAll(destDir); err != nil {
			return errors.Join(primary, fmt.Errorf("cleaning up %s: %w", destDir, err))
		}

		return primary
	}

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return "", cleanupOnErr(fmt.Errorf("reading tar entry: %w", err))
		}

		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return "", cleanupOnErr(fmt.Errorf("invalid tar entry path: %s", hdr.Name))
		}

		target := filepath.Join(destDir, clean)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", cleanupOnErr(fmt.Errorf("creating directory %s: %w", target, err))
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", cleanupOnErr(fmt.Errorf("creating parent directory for %s: %w", target, err))
			}

			if err := extractFile(target, tr); err != nil {
				return "", cleanupOnErr(err)
			}
		}
	}

	return destDir, nil
}

// extractFile writes the contents of r to the file at path.
func extractFile(path string, r io.Reader) error {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("creating file %s: %w", path, err)
	}

	if _, err := io.Copy(out, r); err != nil {
		if closeErr := out.Close(); closeErr != nil {
			return errors.Join(fmt.Errorf("writing file %s: %w", path, err), closeErr)
		}

		return fmt.Errorf("writing file %s: %w", path, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("closing file %s: %w", path, err)
	}

	return nil
}

func (i *kubeComponentInstaller) copyDirectory() (string, error) {
	destDir, err := os.MkdirTemp("", i.tempPattern())
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}

	err = filepath.WalkDir(i.fileOrURL, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(i.fileOrURL, path)
		if err != nil {
			return fmt.Errorf("computing relative path: %w", err)
		}

		target := filepath.Join(destDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		return copyFile(target, path)
	})
	if err != nil {
		if rmErr := os.RemoveAll(destDir); rmErr != nil {
			return "", errors.Join(fmt.Errorf("copying directory: %w", err), rmErr)
		}

		return "", fmt.Errorf("copying directory: %w", err)
	}

	return destDir, nil
}

// copyFile copies the contents of src to a new file at dst.
func copyFile(dst, src string) (retErr error) {
	sf, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}

	defer func() {
		retErr = errors.Join(retErr, sf.Close())
	}()

	df, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}

	if _, err := io.Copy(df, sf); err != nil {
		if closeErr := df.Close(); closeErr != nil {
			return errors.Join(fmt.Errorf("copying %s: %w", src, err), closeErr)
		}

		return fmt.Errorf("copying %s: %w", src, err)
	}

	if err := df.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", dst, err)
	}

	return nil
}
