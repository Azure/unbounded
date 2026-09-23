//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const (
	namespace = "racer-system"
	prefix    = "racer.unbounded-cloud.io/"
)

// Put fixture Pod IPs in an isolated network namespace. The production topology
// correctly rejects loopback Pod IPs. No host addresses or routes are changed.
func TestMain(m *testing.M) {
	if os.Getenv("RACER_CONTROLPLANE_BINARY") != "" && os.Getenv("RACER_LIVE_NETNS") != "1" {
		args := []string{"-n", "unshare", "--net", "sh", "-ec", `ip link set lo up; ip addr add 192.0.2.30/32 dev lo; ip addr add 192.0.2.31/32 dev lo; ip addr add 192.0.2.32/32 dev lo; exec "$@"`, "racer-live", "setpriv", "--reuid", strconv.Itoa(os.Getuid()), "--regid", strconv.Itoa(os.Getgid()), "--clear-groups", "env"}
		args = append(args, os.Environ()...)
		args = append(args, "RACER_LIVE_NETNS=1")
		args = append(args, os.Args...)
		cmd := exec.Command("sudo", args...)

		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		os.Exit(0)
	}

	os.Exit(m.Run())
}

var (
	siteResource  = schema.GroupVersionResource{Group: "unbounded-cloud.io", Version: "v1alpha3", Resource: "sites"}
	cacheResource = schema.GroupVersionResource{Group: "racer.unbounded-cloud.io", Version: "v1alpha1", Resource: "p2pcaches"}
)

type process struct {
	cmd        *exec.Cmd
	done       chan struct{}
	err        error
	once       sync.Once
	log        string
	privileged bool
}

type replica struct {
	pod                                       *corev1.Pod
	ip, control, enroll, health, proof, trust string
	process                                   *process
}

type campaign struct {
	t                                               *testing.T
	ctx                                             context.Context
	kube                                            *kubernetes.Clientset
	dynamic                                         dynamic.Interface
	cpBinary, dpBinary, kubeconfig, dir, socketRoot string
	replicas                                        []*replica
	http                                            *http.Client
	artifacts                                       string
}

func require(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}

func identity(domain, value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte("racer/"+domain+"/v1\x00"+value)))
}

func prerequisite(t *testing.T, name string) string {
	t.Helper()

	value := os.Getenv(name)
	if value == "" {
		if os.Getenv("RACER_REQUIRE_LIVE") == "1" || os.Getenv("CI") != "" {
			t.Fatalf("required live prerequisite %s is unset", name)
		}

		t.Skipf("set %s to enable production-binary integration (RACER_REQUIRE_LIVE=1 makes missing prerequisites fatal)", name)
	}

	return value
}

func newCampaign(t *testing.T) *campaign {
	t.Helper()
	cp := prerequisite(t, "RACER_CONTROLPLANE_BINARY")
	dp := prerequisite(t, "RACER_DATAPLANE_BINARY")
	assets := prerequisite(t, "KUBEBUILDER_ASSETS")
	root := prerequisite(t, "RACER_LIVE_SOCKET_ROOT")
	prerequisite(t, "TMPDIR")

	for _, path := range []string{cp, dp, filepath.Join(assets, "etcd"), filepath.Join(assets, "kube-apiserver")} {
		info, err := os.Stat(path)
		require(t, err)

		if !filepath.IsAbs(path) || info.Mode()&0o111 == 0 {
			t.Fatalf("not an absolute executable: %s", path)
		}
	}

	if !filepath.IsAbs(root) || len(root)+71 > 107 {
		t.Fatal("RACER_LIVE_SOCKET_ROOT must be an existing workspace directory with an absolute path of at most 36 bytes")
	}

	info, err := os.Stat(root)
	require(t, err)

	if !info.IsDir() {
		t.Fatal("socket root is not a directory")
	}
	// Each daemon gets its own mount of the same production socket paths. No
	// snapshot rewriting or alternate control protocol is involved.
	require(t, exec.Command("sudo", "-n", "unshare", "--mount", "true").Run())
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	c := &campaign{t: t, ctx: ctx, cpBinary: cp, dpBinary: dp, dir: t.TempDir(), socketRoot: root, http: &http.Client{Timeout: 2 * time.Second}}
	c.artifacts, err = os.MkdirTemp(os.Getenv("TMPDIR"), "racer-live-artifacts-")
	require(t, err)
	t.Logf("persistent diagnostics: %s", c.artifacts)

	for name, path := range map[string]string{"controlplane": cp, "dataplane": dp} {
		file, err := os.Open(path)
		require(t, err)

		hash := sha256.New()
		_, err = io.Copy(hash, file)
		require(t, err)
		require(t, file.Close())
		require(t, os.WriteFile(filepath.Join(c.artifacts, name+".sha256"), []byte(fmt.Sprintf("%x  %s\n", hash.Sum(nil), path)), 0o600))
	}

	t.Cleanup(c.http.CloseIdleConnections)

	env := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../deploy/machina/crd", "../../deploy/racer/crd"}, ErrorIfCRDPathMissing: true, AttachControlPlaneOutput: os.Getenv("RACER_LIVE_API_LOG") == "1"}
	env.ControlPlane.GetAPIServer().Configure().Set("advertise-address", "192.0.2.30")
	cfg, err := env.Start()
	require(t, err)
	t.Cleanup(func() { require(t, env.Stop()) })

	c.kube, err = kubernetes.NewForConfig(cfg)
	require(t, err)
	c.dynamic, err = dynamic.NewForConfig(cfg)
	require(t, err)

	c.kubeconfig = filepath.Join(c.dir, "kubeconfig")
	kc := clientcmdapi.Config{Clusters: map[string]*clientcmdapi.Cluster{"api": {Server: cfg.Host, CertificateAuthorityData: cfg.CAData}}, AuthInfos: map[string]*clientcmdapi.AuthInfo{"admin": {ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData}}, Contexts: map[string]*clientcmdapi.Context{"live": {Cluster: "api", AuthInfo: "admin"}}, CurrentContext: "live"}
	require(t, clientcmd.WriteToFile(kc, c.kubeconfig))
	_, err = c.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	require(t, err)

	for _, name := range []string{"racer-controlplane", "racer-dataplane"} {
		_, err = c.kube.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		require(t, err)
	}

	labels := map[string]string{prefix + "component": "racer-controlplane"}
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{ServiceAccountName: "racer-controlplane", Containers: []corev1.Container{{Name: "controller", Image: "fixture"}}}}
	deployment, err := c.kube.AppsV1().Deployments(namespace).Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "racer-controlplane", Labels: labels}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: template}}, metav1.CreateOptions{})
	require(t, err)
	rs, err := c.kube.AppsV1().ReplicaSets(namespace).Create(ctx, &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "controllers", OwnerReferences: []metav1.OwnerReference{owner("apps/v1", "Deployment", deployment.Name, deployment.UID)}}, Spec: appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: template}}, metav1.CreateOptions{})
	require(t, err)
	_, err = c.kube.CoreV1().Services(namespace).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "racer-controlplane"}, Spec: corev1.ServiceSpec{Selector: map[string]string{prefix + "serving-leader": "true"}, Ports: []corev1.ServicePort{{Name: "control", Port: 8443}}}}, metav1.CreateOptions{})
	require(t, err)
	proofPort := port(t)

	for i := 0; i < 2; i++ {
		ip := fmt.Sprintf("127.0.0.%d", 20+i)
		pod, err := c.kube.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("controller-%d", i), Labels: labels, OwnerReferences: []metav1.OwnerReference{owner("apps/v1", "ReplicaSet", rs.Name, rs.UID)}}, Spec: template.Spec}, metav1.CreateOptions{})
		require(t, err)

		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip}
		pod, err = c.kube.CoreV1().Pods(namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
		require(t, err)
		r := &replica{pod: pod, ip: ip, control: net.JoinHostPort(ip, port(t)), enroll: net.JoinHostPort(ip, port(t)), health: net.JoinHostPort(ip, port(t)), proof: net.JoinHostPort(ip, proofPort), trust: net.JoinHostPort(ip, port(t))}
		c.replicas = append(c.replicas, r)
	}

	return c
}

func owner(api, kind, name string, uid types.UID) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{APIVersion: api, Kind: kind, Name: name, UID: uid, Controller: &yes}
}

func port(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require(t, err)

	p := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	require(t, l.Close())

	return p
}

func cleanEnv() []string {
	var result []string

	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "RACER_") && !strings.HasPrefix(v, "KUBECONFIG=") {
			result = append(result, v)
		}
	}

	return result
}

func (c *campaign) start(name string, cmd *exec.Cmd) *process {
	c.t.Helper()
	log, err := os.CreateTemp(c.artifacts, name+"-*.log")
	require(c.t, err)

	cmd.Stdout, cmd.Stderr = log, log
	p := &process{cmd: cmd, done: make(chan struct{}), log: log.Name()}
	require(c.t, cmd.Start())

	go func() { p.err = cmd.Wait(); close(p.done) }()

	c.t.Cleanup(func() {
		p.stop(false)
		require(c.t, log.Close())

		if c.t.Failed() {
			data, _ := os.ReadFile(p.log)
			c.t.Logf("%s:\n%s", p.log, data)
		}
	})

	return p
}

func (p *process) stop(crash bool) {
	p.once.Do(func() {
		if p.privileged {
			signal := "-INT"
			if crash {
				signal = "-KILL"
			}

			_ = exec.Command("sudo", "-n", "kill", signal, "--", strconv.Itoa(-p.cmd.Process.Pid)).Run()
			select {
			case <-p.done:
				return
			case <-time.After(10 * time.Second):
				_ = exec.Command("sudo", "-n", "kill", "-KILL", "--", strconv.Itoa(-p.cmd.Process.Pid)).Run()
				<-p.done

				return
			}
		}

		if crash {
			_ = p.cmd.Process.Kill()
		} else {
			_ = p.cmd.Process.Signal(os.Interrupt)
		}

		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
}

func (c *campaign) startReplica(r *replica) {
	cmd := exec.Command(c.cpBinary, "--state-namespace", namespace, "--pod-name", r.pod.Name, "--pod-uid", string(r.pod.UID), "--listen", r.control, "--enroll-listen", r.enroll, "--health-listen", r.health, "--replica-proof-listen", r.proof, "--trust-proof-listen", r.trust, "--socket-root", c.socketRoot, "--leaf-lifetime", "120s", "--clock-skew", "1s")
	cmd.Env = append(cleanEnv(), "KUBECONFIG="+c.kubeconfig, "RUST_LOG=racer_controlplane=debug,kube=warn")
	r.process = c.start(r.pod.Name, cmd)
}

func (c *campaign) await(what string, timeout time.Duration, check func() error) {
	c.t.Helper()

	start := time.Now()

	var err error
	for time.Since(start) < timeout {
		err = check()
		if err == nil {
			c.t.Logf("%s: %s", what, time.Since(start).Round(time.Millisecond))
			return
		}

		select {
		case <-c.ctx.Done():
			c.t.Fatal(c.ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}

	c.t.Fatalf("%s timed out: %v", what, err)
}

func (c *campaign) leader() (*replica, error) {
	var found *replica

	for _, r := range c.replicas {
		pod, err := c.kube.CoreV1().Pods(namespace).Get(c.ctx, r.pod.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		if pod.Labels[prefix+"serving-leader"] == "true" {
			if found != nil {
				return nil, fmt.Errorf("multiple serving leaders")
			}

			found = r
		}
	}

	if found == nil {
		return nil, fmt.Errorf("no serving leader")
	}

	return found, nil
}

func (c *campaign) getJSON(url string, out any) error {
	r, err := c.http.Get(url)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	if r.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d", url, r.StatusCode)
	}

	return json.NewDecoder(r.Body).Decode(out)
}

func (c *campaign) create(resource schema.GroupVersionResource, obj map[string]any) *unstructured.Unstructured {
	c.t.Helper()
	v, err := c.dynamic.Resource(resource).Create(c.ctx, &unstructured.Unstructured{Object: obj}, metav1.CreateOptions{})
	require(c.t, err)

	return v
}

func (c *campaign) patch(resource schema.GroupVersionResource, name string, patch string) {
	c.t.Helper()
	_, err := c.dynamic.Resource(resource).Patch(c.ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	require(c.t, err)
}

func (c *campaign) token(pod *corev1.Pod) string {
	c.t.Helper()
	token, err := c.kube.CoreV1().ServiceAccounts(namespace).CreateToken(c.ctx, "racer-dataplane", &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{Audiences: []string{"racer-control"}, BoundObjectRef: &authv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}}}, metav1.CreateOptions{})
	require(c.t, err)

	return token.Status.Token
}

func (c *campaign) metrics(address string) string {
	c.t.Helper()
	r, err := c.http.Get("http://" + address + "/metrics")
	require(c.t, err)

	defer r.Body.Close()

	b, err := io.ReadAll(r.Body)
	require(c.t, err)

	return string(b)
}
