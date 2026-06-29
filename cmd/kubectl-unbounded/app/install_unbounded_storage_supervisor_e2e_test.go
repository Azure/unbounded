//go:build e2e

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/internal/kube"
)

const (
	storageSupervisorE2EClusterName = "storage-installer-e2e"
	storageSupervisorE2EImage       = "unbounded-storage-supervisor:e2e"
	storageSupervisorE2ENamespace   = unboundedStorageSupervisorNamespace
	storageSupervisorE2ESecret      = "unbounded-storage-e2e-release"
	storageSupervisorE2EPrefix      = "/opt/unbounded-storage-e2e"
	storageSupervisorE2EServiceName = "unbounded-storage-e2e"
	storageSupervisorE2EVersion     = "e2e"
)

func TestSmoke_StorageSupervisorInstallerKind(t *testing.T) {
	arch := storageSupervisorE2EArch(t)
	h := newStorageSupervisorE2EHarness(t)
	h.checkPrereqs()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	h.bootCluster(ctx)
	t.Cleanup(func() {
		tdCtx, tdCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer tdCancel()

		h.teardown(tdCtx)
	})

	h.exportKubeconfig(ctx)
	h.buildAndLoadImage(ctx)
	manifestDir := h.prepareManifests(ctx, arch)
	h.install(ctx, manifestDir)
	h.verifyInstall(ctx, arch)
}

type storageSupervisorE2EHarness struct {
	t              *testing.T
	repoRoot       string
	workDir        string
	kubeconfig     string
	createdCluster bool
	keepCluster    bool
}

func newStorageSupervisorE2EHarness(t *testing.T) *storageSupervisorE2EHarness {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	workDir := t.TempDir()

	return &storageSupervisorE2EHarness{
		t:           t,
		repoRoot:    filepath.Clean(filepath.Join(wd, "..", "..", "..")),
		workDir:     workDir,
		kubeconfig:  filepath.Join(workDir, "kubeconfig"),
		keepCluster: os.Getenv("E2E_KEEP") == "1",
	}
}

func (h *storageSupervisorE2EHarness) checkPrereqs() {
	h.t.Helper()

	for _, bin := range []string{"docker", "kind", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			h.t.Skipf("e2e prereq %q missing on PATH; skipping suite", bin)
		}
	}

	if err := h.run(context.Background(), "docker", "info"); err != nil {
		h.t.Skipf("docker engine unreachable (%v); skipping suite", err)
	}
}

func (h *storageSupervisorE2EHarness) bootCluster(ctx context.Context) {
	h.t.Helper()

	if h.clusterExists(ctx) {
		h.t.Logf("kind cluster %q already exists; reusing", storageSupervisorE2EClusterName)

		return
	}

	if err := h.run(ctx, "kind", "create", "cluster", "--name", storageSupervisorE2EClusterName, "--wait", "120s"); err != nil {
		h.t.Fatalf("kind create cluster: %v", err)
	}

	h.createdCluster = true
}

func (h *storageSupervisorE2EHarness) clusterExists(ctx context.Context) bool {
	h.t.Helper()

	out, err := h.runOut(ctx, "kind", "get", "clusters")
	if err != nil {
		return false
	}

	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == storageSupervisorE2EClusterName {
			return true
		}
	}

	return false
}

func (h *storageSupervisorE2EHarness) exportKubeconfig(ctx context.Context) {
	h.t.Helper()

	if err := h.run(ctx, "kind", "export", "kubeconfig", "--name", storageSupervisorE2EClusterName, "--kubeconfig", h.kubeconfig); err != nil {
		h.t.Fatalf("kind export kubeconfig: %v", err)
	}
}

func (h *storageSupervisorE2EHarness) buildAndLoadImage(ctx context.Context) {
	h.t.Helper()

	arch := storageSupervisorE2EArch(h.t)
	imageDir := filepath.Join(h.workDir, "image")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		h.t.Fatalf("mkdir image dir: %v", err)
	}

	supervisorBin := filepath.Join(imageDir, "unbounded-storage-supervisor")
	if err := h.runWithEnv(ctx, []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + arch},
		"go", "build", "-trimpath", "-o", supervisorBin, "./cmd/unbounded-storage-supervisor",
	); err != nil {
		h.t.Fatalf("build storage supervisor binary: %v", err)
	}

	dockerfile := `FROM busybox:1.36.1
COPY unbounded-storage-supervisor /bin/unbounded-storage-supervisor
ENTRYPOINT ["/bin/unbounded-storage-supervisor"]
`
	if err := os.WriteFile(filepath.Join(imageDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		h.t.Fatalf("write e2e Dockerfile: %v", err)
	}

	platform := "linux/" + storageSupervisorE2EArch(h.t)
	if err := h.run(ctx,
		"docker", "build",
		"--platform", platform,
		"-t", storageSupervisorE2EImage,
		imageDir,
	); err != nil {
		h.t.Fatalf("build storage supervisor image: %v", err)
	}

	if err := h.run(ctx, "kind", "load", "docker-image", storageSupervisorE2EImage, "--name", storageSupervisorE2EClusterName); err != nil {
		h.t.Fatalf("kind load storage supervisor image: %v", err)
	}
}

func (h *storageSupervisorE2EHarness) prepareManifests(ctx context.Context, arch string) string {
	h.t.Helper()

	manifestDir := filepath.Join(h.workDir, "manifests")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		h.t.Fatalf("mkdir manifest dir: %v", err)
	}

	if err := h.run(ctx,
		"go", "run", "./hack/cmd/render-manifests",
		"--templates-dir", "deploy/unbounded-storage-supervisor",
		"--output-dir", manifestDir,
		"--set", "Namespace="+storageSupervisorE2ENamespace,
		"--set", "Image="+storageSupervisorE2EImage,
	); err != nil {
		h.t.Fatalf("render storage supervisor manifests: %v", err)
	}

	tarballName := fmt.Sprintf("unbounded-storage-linux-%s.tar.gz", arch)
	h.writeReleaseSecret(manifestDir, tarballName, fakeStorageReleaseTarball(h.t, arch))
	h.patchDaemonSetManifest(filepath.Join(manifestDir, "04-daemonset.yaml"), arch, tarballName)

	return manifestDir
}

func (h *storageSupervisorE2EHarness) writeReleaseSecret(manifestDir, tarballName string, tarball []byte) {
	h.t.Helper()

	data := base64.StdEncoding.EncodeToString(tarball)
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
data:
  %s: %s
`, storageSupervisorE2ESecret, storageSupervisorE2ENamespace, tarballName, data)

	if err := os.WriteFile(filepath.Join(manifestDir, "02-e2e-release-secret.yaml"), []byte(manifest), 0o644); err != nil {
		h.t.Fatalf("write release secret manifest: %v", err)
	}
}

func (h *storageSupervisorE2EHarness) patchDaemonSetManifest(path, arch, tarballName string) {
	h.t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("read daemonset manifest: %v", err)
	}

	var ds appsv1.DaemonSet
	if err := yaml.Unmarshal(data, &ds); err != nil {
		h.t.Fatalf("unmarshal daemonset manifest: %v", err)
	}

	if len(ds.Spec.Template.Spec.InitContainers) != 1 {
		h.t.Fatalf("expected one init container, got %d", len(ds.Spec.Template.Spec.InitContainers))
	}

	if len(ds.Spec.Template.Spec.Containers) != 1 {
		h.t.Fatalf("expected one run container, got %d", len(ds.Spec.Template.Spec.Containers))
	}

	install := &ds.Spec.Template.Spec.InitContainers[0]
	run := &ds.Spec.Template.Spec.Containers[0]
	install.ImagePullPolicy = corev1.PullNever
	run.ImagePullPolicy = corev1.PullNever
	run.Command = []string{"/bin/sh", "-c"}
	run.Args = []string{"unbounded-storage-supervisor run; status=$?; echo \"run exited with status $status\" >&2; sleep infinity"}
	setEnv(run, "NODE_NAME", "")
	setEnv(install, "SOURCE", "/e2e-release/"+tarballName)
	setEnv(install, "VERSION", storageSupervisorE2EVersion)
	setEnv(install, "ARCH", arch)
	setEnv(install, "NO_ENABLE", "1")
	setEnv(install, "NO_HUGEPAGES", "1")
	setEnv(install, "SYSTEMCTL", "/bin/true")
	setEnv(install, "PREFIX", storageSupervisorE2EPrefix)
	setEnv(install, "SERVICE_NAME", storageSupervisorE2EServiceName)
	install.VolumeMounts = append(install.VolumeMounts, corev1.VolumeMount{
		Name:      "e2e-release",
		MountPath: "/e2e-release",
		ReadOnly:  true,
	})

	for idx := range install.VolumeMounts {
		if install.VolumeMounts[idx].Name == "storage-prefix" {
			install.VolumeMounts[idx].MountPath = storageSupervisorE2EPrefix
		}
	}

	hostPathType := corev1.HostPathDirectoryOrCreate
	for idx := range ds.Spec.Template.Spec.Volumes {
		vol := &ds.Spec.Template.Spec.Volumes[idx]
		switch vol.Name {
		case "storage-prefix":
			vol.HostPath.Path = storageSupervisorE2EPrefix
			vol.HostPath.Type = &hostPathType
		case "systemd-units":
			vol.HostPath.Type = &hostPathType
		}
	}

	ds.Spec.Template.Spec.Volumes = append(ds.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "e2e-release",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: storageSupervisorE2ESecret},
		},
	})

	patched, err := yaml.Marshal(&ds)
	if err != nil {
		h.t.Fatalf("marshal daemonset manifest: %v", err)
	}

	if err := os.WriteFile(path, patched, 0o644); err != nil {
		h.t.Fatalf("write daemonset manifest: %v", err)
	}
}

func (h *storageSupervisorE2EHarness) install(ctx context.Context, manifestDir string) {
	h.t.Helper()

	kubeCli, restCfg, err := kube.ClientAndConfigFromFile(h.kubeconfig)
	if err != nil {
		h.t.Fatalf("create kubernetes client: %v", err)
	}

	kubeResourcesCli, err := client.New(restCfg, client.Options{})
	if err != nil {
		h.t.Fatalf("create controller-runtime client: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	installer := newInstallUnboundedStorageSupervisor(manifestDir, "", nil, logger, kubeResourcesCli, kubeCli)
	installer.waitTimeout = 3 * time.Minute
	installer.pollInterval = 2 * time.Second

	if err := installer.run(ctx); err != nil {
		h.dumpDiagnostics(ctx)
		h.t.Fatalf("install storage supervisor: %v", err)
	}
}

func (h *storageSupervisorE2EHarness) verifyInstall(ctx context.Context, arch string) {
	h.t.Helper()

	releaseDir := fmt.Sprintf("%s/releases/%s-%s", storageSupervisorE2EPrefix, storageSupervisorE2EVersion, arch)
	for _, node := range h.kindNodes(ctx) {
		checks := [][]string{
			{"test", "-x", filepath.Join(releaseDir, "bin", "unbounded-storage")},
			{"test", "-s", "/etc/unbounded-storage/config.binpb"},
			{"test", "-f", "/etc/systemd/system/" + storageSupervisorE2EServiceName + ".service"},
		}

		for _, check := range checks {
			if err := h.waitForDockerExec(ctx, node, 60*time.Second, check...); err != nil {
				h.dumpDiagnostics(ctx)
				h.t.Fatalf("verify node %s: docker exec %v: %v", node, check, err)
			}
		}

		link, err := h.runOut(ctx, "docker", "exec", node, "readlink", filepath.Join(storageSupervisorE2EPrefix, "current"))
		if err != nil {
			h.dumpDiagnostics(ctx)
			h.t.Fatalf("read current symlink on %s: %v", node, err)
		}

		if got, want := strings.TrimSpace(link), releaseDir; got != want {
			h.dumpDiagnostics(ctx)
			h.t.Fatalf("current symlink on %s = %q, want %q", node, got, want)
		}

		unitCheck := fmt.Sprintf("grep -q %s /etc/systemd/system/%s.service", shellQuote(storageSupervisorE2EPrefix+"/current/bin/unbounded-storage"), storageSupervisorE2EServiceName)
		if err := h.run(ctx, "docker", "exec", node, "sh", "-c", unitCheck); err != nil {
			h.dumpDiagnostics(ctx)
			h.t.Fatalf("verify systemd unit on %s: %v", node, err)
		}
	}
}

func (h *storageSupervisorE2EHarness) waitForDockerExec(ctx context.Context, node string, timeout time.Duration, args ...string) error {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		if err := h.dockerExecQuiet(ctx, node, args...); err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for docker exec %s %v", node, args)
		case <-ticker.C:
		}
	}
}

func (h *storageSupervisorE2EHarness) dockerExecQuiet(ctx context.Context, node string, args ...string) error {
	h.t.Helper()

	cmdArgs := append([]string{"exec", node}, args...)
	cmd := exec.CommandContext(ctx, "docker", cmdArgs...)
	cmd.Dir = h.repoRoot
	cmd.Env = os.Environ()

	return cmd.Run()
}

func (h *storageSupervisorE2EHarness) kindNodes(ctx context.Context) []string {
	h.t.Helper()

	out, err := h.runOut(ctx, "kind", "get", "nodes", "--name", storageSupervisorE2EClusterName)
	if err != nil {
		h.t.Fatalf("kind get nodes: %v", err)
	}

	var nodes []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			nodes = append(nodes, line)
		}
	}

	if len(nodes) == 0 {
		h.t.Fatal("kind returned no nodes")
	}

	return nodes
}

func (h *storageSupervisorE2EHarness) teardown(ctx context.Context) {
	h.t.Helper()

	if h.keepCluster {
		h.t.Logf("E2E_KEEP=1 set; preserving kind cluster %q", storageSupervisorE2EClusterName)

		return
	}

	if !h.createdCluster {
		h.t.Logf("kind cluster %q was pre-existing; leaving it in place", storageSupervisorE2EClusterName)

		return
	}

	if err := h.run(ctx, "kind", "delete", "cluster", "--name", storageSupervisorE2EClusterName); err != nil {
		h.t.Logf("kind delete cluster failed: %v", err)
	}
}

func (h *storageSupervisorE2EHarness) dumpDiagnostics(ctx context.Context) {
	h.t.Helper()

	commands := [][]string{
		{"kubectl", "--kubeconfig", h.kubeconfig, "-n", storageSupervisorE2ENamespace, "get", "daemonset,pods", "-o", "wide"},
		{"kubectl", "--kubeconfig", h.kubeconfig, "-n", storageSupervisorE2ENamespace, "describe", "daemonset", unboundedStorageSupervisorDaemonSetName},
		{"kubectl", "--kubeconfig", h.kubeconfig, "-n", storageSupervisorE2ENamespace, "describe", "pods", "-l", "app.kubernetes.io/name=unbounded-storage-supervisor"},
		{"kubectl", "--kubeconfig", h.kubeconfig, "-n", storageSupervisorE2ENamespace, "logs", "daemonset/" + unboundedStorageSupervisorDaemonSetName, "-c", "install", "--tail", "200"},
		{"kubectl", "--kubeconfig", h.kubeconfig, "-n", storageSupervisorE2ENamespace, "logs", "daemonset/" + unboundedStorageSupervisorDaemonSetName, "-c", "run", "--tail", "200"},
	}

	for _, command := range commands {
		out, err := h.runOut(ctx, command[0], command[1:]...)
		if err != nil {
			h.t.Logf("diagnostic %v failed: %v", command, err)
		}

		if strings.TrimSpace(out) != "" {
			h.t.Logf("diagnostic %v:\n%s", command, out)
		}
	}

	for _, node := range h.kindNodes(ctx) {
		out, err := h.runOut(ctx, "docker", "exec", node, "sh", "-c", "ls -la /opt/unbounded-storage-e2e /opt/unbounded-storage-e2e/releases /etc/unbounded-storage /etc/systemd/system 2>/dev/null || true")
		if err != nil {
			h.t.Logf("node diagnostic for %s failed: %v", node, err)
		}

		if strings.TrimSpace(out) != "" {
			h.t.Logf("node diagnostic for %s:\n%s", node, out)
		}
	}
}

func (h *storageSupervisorE2EHarness) run(ctx context.Context, name string, args ...string) error {
	h.t.Helper()

	_, err := h.runCombinedWithEnv(ctx, nil, name, args...)

	return err
}

func (h *storageSupervisorE2EHarness) runWithEnv(ctx context.Context, extraEnv []string, name string, args ...string) error {
	h.t.Helper()

	_, err := h.runCombinedWithEnv(ctx, extraEnv, name, args...)

	return err
}

func (h *storageSupervisorE2EHarness) runOut(ctx context.Context, name string, args ...string) (string, error) {
	h.t.Helper()

	return h.runCombinedWithEnv(ctx, nil, name, args...)
}

func (h *storageSupervisorE2EHarness) runCombinedWithEnv(ctx context.Context, extraEnv []string, name string, args ...string) (string, error) {
	h.t.Helper()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = h.repoRoot
	cmd.Env = append(os.Environ(), extraEnv...)
	if h.kubeconfig != "" {
		cmd.Env = append(cmd.Env, "KUBECONFIG="+h.kubeconfig)
	}

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()
	if err != nil {
		trimmed := strings.TrimSpace(out.String())
		if trimmed != "" {
			h.t.Logf("%s %s output:\n%s", name, strings.Join(args, " "), trimmed)
		}

		return out.String(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}

	return out.String(), nil
}

func fakeStorageReleaseTarball(t *testing.T, arch string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	top := "unbounded-storage-linux-" + arch
	entries := []struct {
		name string
		mode int64
		body string
	}{
		{name: top + "/bin/", mode: 0o755},
		{name: top + "/lib/", mode: 0o755},
		{name: top + "/bin/unbounded-storage", mode: 0o755, body: "#!/bin/sh\nexit 0\n"},
	}

	for _, entry := range entries {
		hdr := &tar.Header{
			Name: entry.name,
			Mode: entry.mode,
		}
		if strings.HasSuffix(entry.name, "/") {
			hdr.Typeflag = tar.TypeDir
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(entry.body))
		}

		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header: %v", err)
		}

		if entry.body != "" {
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatalf("write tar body: %v", err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}

	return buf.Bytes()
}

func setEnv(container *corev1.Container, name, value string) {
	for idx := range container.Env {
		if container.Env[idx].Name == name {
			container.Env[idx].Value = value
			container.Env[idx].ValueFrom = nil

			return
		}
	}

	container.Env = append(container.Env, corev1.EnvVar{Name: name, Value: value})
}

func storageSupervisorE2EArch(t *testing.T) string {
	t.Helper()

	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH
	default:
		t.Skipf("unbounded-storage supervisor installer supports amd64/arm64 release archives, got %s", runtime.GOARCH)

		return ""
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
