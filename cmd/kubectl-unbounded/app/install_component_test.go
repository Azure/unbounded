package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	machinadeploy "github.com/project-unbounded/unbounded-kube/deploy/machina"
)

// discardLogger returns a logger that discards all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeTarGz writes a .tar.gz archive to w from a map of relative paths to
// file contents. Directory entries are inferred automatically from the file
// paths. The tar and gzip writers are closed before returning.
func writeTarGz(t *testing.T, w io.Writer, files map[string]string) {
	t.Helper()

	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)
	// Collect and sort directory entries so archives are deterministic.
	dirs := map[string]bool{}

	for name := range files {
		for d := filepath.Dir(name); d != "."; d = filepath.Dir(d) {
			dirs[d] = true
		}
	}

	sortedDirs := make([]string, 0, len(dirs))
	for d := range dirs {
		sortedDirs = append(sortedDirs, d)
	}

	sort.Strings(sortedDirs)

	for _, d := range sortedDirs {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     d + "/",
			Mode:     0o755,
		}))
	}

	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Size:     int64(len(content)),
			Mode:     0o644,
		}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
}

// createTarGz builds a .tar.gz archive from a map of relative paths to file
// contents. The archive is written to dest/archive.tar.gz and the full path
// is returned.
func createTarGz(t *testing.T, dest string, files map[string]string) string {
	t.Helper()

	archivePath := filepath.Join(dest, "archive.tar.gz")
	f, err := os.Create(archivePath)
	require.NoError(t, err)
	writeTarGz(t, f, files)
	require.NoError(t, f.Close())

	return archivePath
}

// createTarGzBytes builds a .tar.gz archive in memory and returns the raw bytes.
func createTarGzBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	writeTarGz(t, &buf, files)

	return buf.Bytes()
}

// newTestInstaller returns a kubeComponentInstaller suitable for unit tests,
// pre-configured with the given fileOrURL and sensible defaults.
func newTestInstaller(fileOrURL string) *kubeComponentInstaller {
	return &kubeComponentInstaller{
		fileOrURL:  fileOrURL,
		logger:     discardLogger(),
		tempPrefix: "test-component",
	}
}

func TestResolveManifestsWithDirectory(t *testing.T) {
	srcDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "a.yaml"), []byte("apiVersion: v1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "sub", "b.yaml"), []byte("kind: Service"), 0o644))
	inst := newTestInstaller(srcDir)
	result, err := inst.resolveManifests()
	require.NoError(t, err)

	defer func() {
		require.NoError(t, os.RemoveAll(result))
	}()
	// Result should be a different directory than the source.
	require.NotEqual(t, srcDir, result)
	got, err := os.ReadFile(filepath.Join(result, "a.yaml"))
	require.NoError(t, err)
	require.Equal(t, "apiVersion: v1", string(got))
	got, err = os.ReadFile(filepath.Join(result, "sub", "b.yaml"))
	require.NoError(t, err)
	require.Equal(t, "kind: Service", string(got))
}

func TestResolveManifestsWithTarGz(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := createTarGz(t, tmpDir, map[string]string{
		"manifests/deployment.yaml":      "apiVersion: apps/v1",
		"manifests/sub/service.yaml":     "kind: Service",
		"manifests/sub/deep/config.yaml": "data: value",
	})
	inst := newTestInstaller(archivePath)
	result, err := inst.resolveManifests()
	require.NoError(t, err)

	defer func() {
		require.NoError(t, os.RemoveAll(result))
	}()

	got, err := os.ReadFile(filepath.Join(result, "manifests", "deployment.yaml"))
	require.NoError(t, err)
	require.Equal(t, "apiVersion: apps/v1", string(got))
	got, err = os.ReadFile(filepath.Join(result, "manifests", "sub", "service.yaml"))
	require.NoError(t, err)
	require.Equal(t, "kind: Service", string(got))
	got, err = os.ReadFile(filepath.Join(result, "manifests", "sub", "deep", "config.yaml"))
	require.NoError(t, err)
	require.Equal(t, "data: value", string(got))
}

func TestResolveManifestsWithHTTPS(t *testing.T) {
	archiveBytes := createTarGzBytes(t, map[string]string{
		"cni/install.yaml":       "apiVersion: v1",
		"cni/nested/config.yaml": "kind: ConfigMap",
	})

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(archiveBytes)
	}))
	defer ts.Close()

	inst := &kubeComponentInstaller{
		fileOrURL:  ts.URL + "/cni.tar.gz",
		httpClient: ts.Client(),
		logger:     discardLogger(),
		tempPrefix: "test-component",
	}
	result, err := inst.resolveManifests()
	require.NoError(t, err)

	defer func() {
		require.NoError(t, os.RemoveAll(result))
	}()

	got, err := os.ReadFile(filepath.Join(result, "cni", "install.yaml"))
	require.NoError(t, err)
	require.Equal(t, "apiVersion: v1", string(got))
	got, err = os.ReadFile(filepath.Join(result, "cni", "nested", "config.yaml"))
	require.NoError(t, err)
	require.Equal(t, "kind: ConfigMap", string(got))
}

func TestResolveManifestsWithHTTPS_Non200(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	inst := &kubeComponentInstaller{
		fileOrURL:  ts.URL + "/missing.tar.gz",
		httpClient: ts.Client(),
		logger:     discardLogger(),
		tempPrefix: "test-component",
	}
	_, err := inst.resolveManifests()
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 404")
}

func TestResolveManifestsWithInvalidPath(t *testing.T) {
	inst := newTestInstaller("/nonexistent/path/to/nothing")
	_, err := inst.resolveManifests()
	require.Error(t, err)
}

func TestExtractArchivePathTraversal(t *testing.T) {
	// Build a tar.gz with a path traversal entry directly, bypassing the
	// helper since it would normalise paths.
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "evil.tar.gz")
	f, err := os.Create(archivePath)
	require.NoError(t, err)

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "../../etc/passwd",
		Size:     5,
		Mode:     0o644,
	}))
	_, err = tw.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	require.NoError(t, f.Close())

	inst := newTestInstaller(archivePath)
	_, err = inst.resolveManifests()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid tar entry path")
}

func TestWaitForController_PodRunning(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unbounded-net-controller-abc123",
			Namespace: unboundedCNINamespace,
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Ready: true}},
		},
	}
	cli := fake.NewClientset(pod)
	inst := &kubeComponentInstaller{
		kubeCli:        cli,
		logger:         discardLogger(),
		namespace:      unboundedCNINamespace,
		controllerName: unboundedCNIControllerName,
	}
	err := inst.waitForController(context.Background())
	require.NoError(t, err)
}

func TestWaitForController_PodRunningButContainerNotReady(t *testing.T) {
	// Simulates a CrashLoopBackOff scenario: phase is Running but the
	// container is not ready.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unbounded-net-controller-abc123",
			Namespace: unboundedCNINamespace,
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Ready: false}},
		},
	}
	cli := fake.NewClientset(pod)
	inst := &kubeComponentInstaller{
		kubeCli:        cli,
		logger:         discardLogger(),
		namespace:      unboundedCNINamespace,
		controllerName: unboundedCNIControllerName,
		waitTimeout:    200 * time.Millisecond,
		pollInterval:   50 * time.Millisecond,
	}
	err := inst.waitForController(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out")
}

func TestWaitForController_PodPending(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unbounded-net-controller-abc123",
			Namespace: unboundedCNINamespace,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}
	cli := fake.NewClientset(pod)
	inst := &kubeComponentInstaller{
		kubeCli:        cli,
		logger:         discardLogger(),
		namespace:      unboundedCNINamespace,
		controllerName: unboundedCNIControllerName,
		waitTimeout:    200 * time.Millisecond,
		pollInterval:   50 * time.Millisecond,
	}
	err := inst.waitForController(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out")
}

func TestWaitForController_NoPod(t *testing.T) {
	cli := fake.NewClientset()
	inst := &kubeComponentInstaller{
		kubeCli:        cli,
		logger:         discardLogger(),
		namespace:      unboundedCNINamespace,
		controllerName: unboundedCNIControllerName,
		waitTimeout:    200 * time.Millisecond,
		pollInterval:   50 * time.Millisecond,
	}
	err := inst.waitForController(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out")
}

func TestWaitForController_IgnoresOtherPods(t *testing.T) {
	otherPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "some-other-pod",
			Namespace: unboundedCNINamespace,
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Ready: true}},
		},
	}
	cli := fake.NewClientset(otherPod)
	inst := &kubeComponentInstaller{
		kubeCli:        cli,
		logger:         discardLogger(),
		namespace:      unboundedCNINamespace,
		controllerName: unboundedCNIControllerName,
		waitTimeout:    200 * time.Millisecond,
		pollInterval:   50 * time.Millisecond,
	}
	err := inst.waitForController(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out")
}

func TestWaitForController_MachinaNamespace(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "machina-controller-xyz789",
			Namespace: machinaNamespace,
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Ready: true}},
		},
	}
	cli := fake.NewClientset(pod)
	inst := &kubeComponentInstaller{
		kubeCli:        cli,
		logger:         discardLogger(),
		namespace:      machinaNamespace,
		controllerName: machinaControllerName,
	}
	err := inst.waitForController(context.Background())
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Accessor method tests
// ---------------------------------------------------------------------------

func TestClient_DefaultAndExplicit(t *testing.T) {
	inst := &kubeComponentInstaller{}
	require.Equal(t, http.DefaultClient, inst.client(), "zero-value httpClient should return http.DefaultClient")

	custom := &http.Client{Timeout: 42 * time.Second}
	inst.httpClient = custom
	require.Equal(t, custom, inst.client(), "explicit httpClient should be returned")
}

func TestTimeout_DefaultAndExplicit(t *testing.T) {
	inst := &kubeComponentInstaller{}
	require.Equal(t, defaultWaitTimeout, inst.timeout(), "zero-value waitTimeout should return defaultWaitTimeout")

	inst.waitTimeout = 10 * time.Second
	require.Equal(t, 10*time.Second, inst.timeout(), "explicit waitTimeout should be returned")
}

func TestInterval_DefaultAndExplicit(t *testing.T) {
	inst := &kubeComponentInstaller{}
	require.Equal(t, defaultPollInterval, inst.interval(), "zero-value pollInterval should return defaultPollInterval")

	inst.pollInterval = 1 * time.Second
	require.Equal(t, 1*time.Second, inst.interval(), "explicit pollInterval should be returned")
}

func TestTempPattern(t *testing.T) {
	inst := &kubeComponentInstaller{tempPrefix: "machina"}
	require.Equal(t, "machina-*", inst.tempPattern())
}

func TestTempArchivePattern(t *testing.T) {
	inst := &kubeComponentInstaller{tempPrefix: "unbounded-net"}
	require.Equal(t, "unbounded-net-*.tar.gz", inst.tempArchivePattern())
}

// ---------------------------------------------------------------------------
// run() method tests
// ---------------------------------------------------------------------------

// validManifest is a minimal valid Kubernetes YAML manifest.
const validManifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
  namespace: default
data:
  key: value
`

func TestRun_Success(t *testing.T) {
	// Prepare a directory with a valid manifest.
	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "cm.yaml"), []byte(validManifest), 0o644))

	// Fake controller-runtime client that accepts apply calls.
	var applyCalled bool

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applyCalled = true
				return nil
			},
		}).
		Build()

	// Fake kubernetes clientset with a running controller pod.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-controller-abc123",
			Namespace: "test-ns",
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Ready: true}},
		},
	}
	kubeCli := fake.NewClientset(pod)

	inst := &kubeComponentInstaller{
		fileOrURL:        srcDir,
		logger:           discardLogger(),
		kubeResourcesCli: kubeResourcesCli,
		kubeCli:          kubeCli,
		namespace:        "test-ns",
		controllerName:   "test-controller",
		waitTimeout:      5 * time.Second,
		pollInterval:     50 * time.Millisecond,
		tempPrefix:       "test-component",
	}

	err := inst.run(context.Background())
	require.NoError(t, err)
	require.True(t, applyCalled, "apply should have been called")
}

func TestRun_ApplyFailure(t *testing.T) {
	// Prepare a directory with a valid manifest.
	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "cm.yaml"), []byte(validManifest), 0o644))

	// Fake controller-runtime client that returns an error on apply.
	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				return fmt.Errorf("simulated apply failure")
			},
		}).
		Build()

	kubeCli := fake.NewClientset()

	inst := &kubeComponentInstaller{
		fileOrURL:        srcDir,
		logger:           discardLogger(),
		kubeResourcesCli: kubeResourcesCli,
		kubeCli:          kubeCli,
		namespace:        "test-ns",
		controllerName:   "test-controller",
		waitTimeout:      200 * time.Millisecond,
		pollInterval:     50 * time.Millisecond,
		tempPrefix:       "test-component",
	}

	err := inst.run(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "applying manifests")
	require.Contains(t, err.Error(), "simulated apply failure")
}

func TestRun_ControllerTimeout(t *testing.T) {
	// Prepare a directory with a valid manifest.
	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "cm.yaml"), []byte(validManifest), 0o644))

	// Fake controller-runtime client that accepts apply calls.
	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				return nil
			},
		}).
		Build()

	// No matching pod => controller will time out.
	kubeCli := fake.NewClientset()

	inst := &kubeComponentInstaller{
		fileOrURL:        srcDir,
		logger:           discardLogger(),
		kubeResourcesCli: kubeResourcesCli,
		kubeCli:          kubeCli,
		namespace:        "test-ns",
		controllerName:   "test-controller",
		waitTimeout:      200 * time.Millisecond,
		pollInterval:     50 * time.Millisecond,
		tempPrefix:       "test-component",
	}

	err := inst.run(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "waiting for test-controller to become running")
	require.Contains(t, err.Error(), "timed out")
}

func TestRun_ResolveManifestsFailure(t *testing.T) {
	kubeResourcesCli := fakeclient.NewClientBuilder().Build()
	kubeCli := fake.NewClientset()

	inst := &kubeComponentInstaller{
		fileOrURL:        "/nonexistent/path/to/manifests",
		logger:           discardLogger(),
		kubeResourcesCli: kubeResourcesCli,
		kubeCli:          kubeCli,
		namespace:        "test-ns",
		controllerName:   "test-controller",
		waitTimeout:      200 * time.Millisecond,
		pollInterval:     50 * time.Millisecond,
		tempPrefix:       "test-component",
	}

	err := inst.run(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolving manifests")
}

func TestRun_WithTarGzArchive(t *testing.T) {
	// Test run() with a tar.gz archive instead of a directory.
	tmpDir := t.TempDir()
	archivePath := createTarGz(t, tmpDir, map[string]string{
		"deploy/cm.yaml": validManifest,
	})

	var applyCalled bool

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applyCalled = true
				return nil
			},
		}).
		Build()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-controller-pod-xyz",
			Namespace: "deploy-ns",
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Ready: true}},
		},
	}
	kubeCli := fake.NewClientset(pod)

	inst := &kubeComponentInstaller{
		fileOrURL:        archivePath,
		logger:           discardLogger(),
		kubeResourcesCli: kubeResourcesCli,
		kubeCli:          kubeCli,
		namespace:        "deploy-ns",
		controllerName:   "my-controller-pod",
		waitTimeout:      5 * time.Second,
		pollInterval:     50 * time.Millisecond,
		tempPrefix:       "test-component",
	}

	err := inst.run(context.Background())
	require.NoError(t, err)
	require.True(t, applyCalled, "apply should have been called for tar.gz archive")
}

func TestRun_WithHTTPS(t *testing.T) {
	// Test run() with an HTTPS URL source.
	archiveBytes := createTarGzBytes(t, map[string]string{
		"manifests/cm.yaml": validManifest,
	})

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(archiveBytes)
	}))
	defer ts.Close()

	var applyCalled bool

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applyCalled = true
				return nil
			},
		}).
		Build()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "https-controller-abc",
			Namespace: "https-ns",
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Ready: true}},
		},
	}
	kubeCli := fake.NewClientset(pod)

	inst := &kubeComponentInstaller{
		fileOrURL:        ts.URL + "/manifests.tar.gz",
		httpClient:       ts.Client(),
		logger:           discardLogger(),
		kubeResourcesCli: kubeResourcesCli,
		kubeCli:          kubeCli,
		namespace:        "https-ns",
		controllerName:   "https-controller",
		waitTimeout:      5 * time.Second,
		pollInterval:     50 * time.Millisecond,
		tempPrefix:       "test-component",
	}

	err := inst.run(context.Background())
	require.NoError(t, err)
	require.True(t, applyCalled, "apply should have been called for HTTPS source")
}

func TestRun_ContextCancelled(t *testing.T) {
	// Verify run() respects context cancellation during waitForController.
	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "cm.yaml"), []byte(validManifest), 0o644))

	kubeResourcesCli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				return nil
			},
		}).
		Build()

	// No matching pod so waitForController will block.
	kubeCli := fake.NewClientset()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so waitForController sees a cancelled context.
	cancel()

	inst := &kubeComponentInstaller{
		fileOrURL:        srcDir,
		logger:           discardLogger(),
		kubeResourcesCli: kubeResourcesCli,
		kubeCli:          kubeCli,
		namespace:        "test-ns",
		controllerName:   "test-controller",
		waitTimeout:      5 * time.Minute,
		pollInterval:     50 * time.Millisecond,
		tempPrefix:       "test-component",
	}

	err := inst.run(ctx)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Constructor tests
// ---------------------------------------------------------------------------

func TestNewInstallMachina(t *testing.T) {
	logger := discardLogger()
	kubeResourcesCli := fakeclient.NewClientBuilder().Build()
	kubeCli := fake.NewClientset()
	httpCli := &http.Client{Timeout: 30 * time.Second}

	im := newInstallMachina("https://example.com/machina.tar.gz", httpCli, logger, kubeResourcesCli, kubeCli)

	require.NotNil(t, im)
	require.NotNil(t, im.kubeComponentInstaller)
	require.Equal(t, "https://example.com/machina.tar.gz", im.fileOrURL)
	require.Equal(t, httpCli, im.httpClient)
	require.Equal(t, logger, im.logger)
	require.Equal(t, kubeResourcesCli, im.kubeResourcesCli)
	require.Equal(t, kubeCli, im.kubeCli)
	require.Equal(t, machinaNamespace, im.namespace)
	require.Equal(t, machinaControllerName, im.controllerName)
	require.Equal(t, "machina", im.tempPrefix)
	require.Equal(t, 5*time.Minute, im.waitTimeout)
	require.Equal(t, 5*time.Second, im.pollInterval)
	require.Nil(t, im.embeddedFS, "embeddedFS should be nil when fileOrURL is provided")
}

func TestNewInstallMachina_EmbeddedFallback(t *testing.T) {
	logger := discardLogger()
	kubeResourcesCli := fakeclient.NewClientBuilder().Build()
	kubeCli := fake.NewClientset()

	im := newInstallMachina("", nil, logger, kubeResourcesCli, kubeCli)

	require.NotNil(t, im)
	require.Equal(t, "", im.fileOrURL)
	require.NotNil(t, im.embeddedFS, "embeddedFS should be set when fileOrURL is empty")
}

func TestNewInstallUnboundedCNI(t *testing.T) {
	logger := discardLogger()
	kubeResourcesCli := fakeclient.NewClientBuilder().Build()
	kubeCli := fake.NewClientset()
	httpCli := &http.Client{Timeout: 30 * time.Second}

	iu := newInstallUnboundedCNI("https://example.com/cni.tar.gz", httpCli, logger, kubeResourcesCli, kubeCli)

	require.NotNil(t, iu)
	require.NotNil(t, iu.kubeComponentInstaller)
	require.Equal(t, "https://example.com/cni.tar.gz", iu.fileOrURL)
	require.Equal(t, httpCli, iu.httpClient)
	require.Equal(t, logger, iu.logger)
	require.Equal(t, kubeResourcesCli, iu.kubeResourcesCli)
	require.Equal(t, kubeCli, iu.kubeCli)
	require.Equal(t, unboundedCNINamespace, iu.namespace)
	require.Equal(t, unboundedCNIControllerName, iu.controllerName)
	require.Equal(t, "unbounded-net", iu.tempPrefix)
	require.Equal(t, 5*time.Minute, iu.waitTimeout)
	require.Equal(t, 5*time.Second, iu.pollInterval)
}

func TestNewInstallUnboundedCNI_Prototype(t *testing.T) {
	t.Setenv("UB_PROTOTYPE_UNBOUNDED_CNI", "1")

	logger := discardLogger()
	kubeResourcesCli := fakeclient.NewClientBuilder().Build()
	kubeCli := fake.NewClientset()
	httpCli := &http.Client{Timeout: 30 * time.Second}

	iu := newInstallUnboundedCNI("https://example.com/cni.tar.gz", httpCli, logger, kubeResourcesCli, kubeCli)

	require.NotNil(t, iu)
	require.Equal(t, "unbounded-cni", iu.namespace)
	require.Equal(t, "unbounded-cni-controller", iu.controllerName)
	require.Equal(t, "unbounded-cni", iu.tempPrefix)
	require.Equal(t, 5*time.Minute, iu.waitTimeout)
	require.Equal(t, 5*time.Second, iu.pollInterval)
}

// ---------------------------------------------------------------------------
// copyFile standalone tests
// ---------------------------------------------------------------------------

func TestCopyFile_Success(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcPath := filepath.Join(srcDir, "source.txt")
	dstPath := filepath.Join(dstDir, "dest.txt")

	content := "hello, world!"
	require.NoError(t, os.WriteFile(srcPath, []byte(content), 0o644))

	err := copyFile(dstPath, srcPath)
	require.NoError(t, err)

	got, err := os.ReadFile(dstPath)
	require.NoError(t, err)
	require.Equal(t, content, string(got))
}

func TestCopyFile_SourceNotFound(t *testing.T) {
	dstDir := t.TempDir()
	dstPath := filepath.Join(dstDir, "dest.txt")

	err := copyFile(dstPath, "/nonexistent/source.txt")
	require.Error(t, err)
	require.Contains(t, err.Error(), "opening")
}

func TestCopyFile_DestNotWritable(t *testing.T) {
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "source.txt")
	require.NoError(t, os.WriteFile(srcPath, []byte("data"), 0o644))

	err := copyFile("/nonexistent/dir/dest.txt", srcPath)
	require.Error(t, err)
	require.Contains(t, err.Error(), "creating")
}

// ---------------------------------------------------------------------------
// materializeEmbeddedFS tests
// ---------------------------------------------------------------------------

func TestMaterializeEmbeddedFS(t *testing.T) {
	// Use the real embedded machina manifests to verify the materialization
	// produces the expected file tree.
	inst := &kubeComponentInstaller{
		embeddedFS: machinadeploy.Manifests,
		tempPrefix: "test-materialize",
	}

	dir, err := inst.materializeEmbeddedFS()
	require.NoError(t, err)

	defer func() {
		require.NoError(t, os.RemoveAll(dir))
	}()

	// Verify top-level YAML files exist.
	for _, name := range []string{
		"01-namespace.yaml",
		"02-rbac.yaml",
		"03-config.yaml",
		"04-deployment.yaml",
		"05-service.yaml",
	} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err, "expected %s to exist", name)
		require.False(t, info.IsDir())
		require.Greater(t, info.Size(), int64(0), "%s should not be empty", name)
	}

	// Verify CRD subdirectory files exist.
	for _, name := range []string{
		"crd/unbounded-kube.io_machines.yaml",
	} {
		info, err := os.Stat(filepath.Join(dir, name))
		require.NoError(t, err, "expected %s to exist", name)
		require.False(t, info.IsDir())
		require.Greater(t, info.Size(), int64(0), "%s should not be empty", name)
	}
}

func TestMaterializeEmbeddedFS_SmallFS(t *testing.T) {
	// Use a small in-memory FS to verify content fidelity.
	memFS := fstest.MapFS{
		"a.yaml":     &fstest.MapFile{Data: []byte("apiVersion: v1")},
		"sub/b.yaml": &fstest.MapFile{Data: []byte("kind: Service")},
	}

	inst := &kubeComponentInstaller{
		embeddedFS: memFS,
		tempPrefix: "test-materialize-small",
	}

	dir, err := inst.materializeEmbeddedFS()
	require.NoError(t, err)

	defer func() {
		require.NoError(t, os.RemoveAll(dir))
	}()

	got, err := os.ReadFile(filepath.Join(dir, "a.yaml"))
	require.NoError(t, err)
	require.Equal(t, "apiVersion: v1", string(got))

	got, err = os.ReadFile(filepath.Join(dir, "sub", "b.yaml"))
	require.NoError(t, err)
	require.Equal(t, "kind: Service", string(got))
}

func TestResolveManifestsWithEmbeddedFS(t *testing.T) {
	memFS := fstest.MapFS{
		"manifest.yaml": &fstest.MapFile{Data: []byte("apiVersion: v1")},
	}

	inst := &kubeComponentInstaller{
		fileOrURL:  "",
		embeddedFS: memFS,
		tempPrefix: "test-resolve-embedded",
	}

	dir, err := inst.resolveManifests()
	require.NoError(t, err)

	defer func() {
		require.NoError(t, os.RemoveAll(dir))
	}()

	got, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	require.NoError(t, err)
	require.Equal(t, "apiVersion: v1", string(got))
}

func TestResolveManifests_FileOrURLTakesPrecedence(t *testing.T) {
	// When fileOrURL is set, embeddedFS should be ignored.
	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "explicit.yaml"), []byte("explicit"), 0o644))

	memFS := fstest.MapFS{
		"embedded.yaml": &fstest.MapFile{Data: []byte("embedded")},
	}

	inst := &kubeComponentInstaller{
		fileOrURL:  srcDir,
		embeddedFS: memFS,
		tempPrefix: "test-precedence",
	}

	dir, err := inst.resolveManifests()
	require.NoError(t, err)

	defer func() {
		require.NoError(t, os.RemoveAll(dir))
	}()

	// The explicit directory's file should be present.
	got, err := os.ReadFile(filepath.Join(dir, "explicit.yaml"))
	require.NoError(t, err)
	require.Equal(t, "explicit", string(got))

	// The embedded file should NOT be present.
	_, err = os.Stat(filepath.Join(dir, "embedded.yaml"))
	require.True(t, os.IsNotExist(err), "embedded.yaml should not exist when fileOrURL is set")
}

func TestResolveManifests_NoFileOrURLNoEmbeddedFS(t *testing.T) {
	inst := &kubeComponentInstaller{
		fileOrURL:  "",
		embeddedFS: nil,
		tempPrefix: "test-no-source",
	}

	_, err := inst.resolveManifests()
	require.Error(t, err)
}
