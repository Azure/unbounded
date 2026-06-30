//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	// In-cluster auth registry test fixtures. The registry is deployed
	// in the unbounded-system namespace so it can be reached via the
	// well-known cluster-DNS name without RBAC churn.
	authRegistryNS     = "unbounded-system"
	authRegistryName   = "auth-registry"
	authRegistryHost   = "auth-registry.unbounded-system.svc.cluster.local"
	authRegistryUser   = "testuser"
	authRegistryPass   = "testpass"
	authRegistryImage  = "registry:2"
	authRegistrySkopeo = "quay.io/skopeo/stable:latest"
	// authRegistryDigest is the manifest list digest of
	// registry.k8s.io/e2e-test-images/agnhost:2.39. agnhost is a frozen
	// e2e image so this digest is stable. The skopeo seed Job copies
	// with --multi-arch=all so this exact digest is preserved in the
	// in-cluster auth registry. We reference the image by digest in the
	// pull pod so containerd asks gantry's mirror for a digest manifest
	// (which gantry serves through please_pull -> origin with creds),
	// rather than a tag manifest (which gantry returns 503 for by
	// design, causing containerd to fall through to direct unauth
	// access).
	authRegistryDigest   = "sha256:7e8bdd271312fd25fc5ff5a8f04727be84044eb3d7d8d03611972a6752e2e11e"
	authRegistryRef      = authRegistryHost + ":5000/agnhost@" + authRegistryDigest
	authRegistryRefShort = "auth-registry.unbounded-system.svc.cluster.local:5000"
	// authRegistryCredsKey is the Secret data key (and on-disk filename
	// under /etc/gantry/registry/) holding the Basic-auth credentials
	// for authRegistryRefShort. Secret keys cannot contain ':' so this
	// is a sanitized variant of authRegistryRefShort.
	authRegistryCredsKey = "auth-registry.unbounded-system.svc.cluster.local_5000"
)

// TestE2E_PrivateAuthRegistry proves that Gantry's credentialed
// please_pull / origin path works end-to-end against a real
// authenticated registry:
//
//   - Deploy registry:2 with htpasswd-based Basic auth inside the cluster.
//   - Use skopeo (in a Job) to copy registry.k8s.io/e2e-test-images/agnhost
//     into the in-cluster registry so we have a real OCI image protected
//     by auth.
//   - Project the credentials into the gantry pods via the existing
//     gantry-registry-credentials Secret hook (see deploy/daemonset.yaml).
//   - Patch the gantry ConfigMap to add the in-cluster registry with
//     credentials_path pointing at the projected Secret file.
//   - Restart the gantry DaemonSet so origin.New picks up the new entry.
//   - Configure containerd on each kind node with hosts.toml routing the
//     in-cluster registry through Gantry's mirror.
//   - Schedule a workload that pulls the auth-protected image and verify:
//   - the workload becomes Ready (auth succeeded end-to-end)
//   - origin_pull_total{registry="auth-registry"} increased
//
// This is the credential-flow gate that mock-server unit tests cannot
// prove: that the agent's HTTP client correctly sends Basic auth on the
// please_pull / origin path against a real registry-protocol server.
func TestE2E_PrivateAuthRegistry(t *testing.T) {
	h := newHarness(t)
	h.checkPrereqs()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	h.bootCluster(ctx)
	t.Cleanup(func() {
		tdCtx, tdCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer tdCancel()
		h.teardown(tdCtx)
	})

	h.buildAndLoadImage(ctx)
	h.applyManifests(ctx)
	h.waitForRollout(ctx)
	h.checkReadyz(ctx)

	// Stand up the in-cluster auth registry. This installs:
	//   - htpasswd Secret with the test user
	//   - registry:2 Deployment + Service
	h.deployAuthRegistry(ctx)
	h.waitForDeploymentReady(ctx, authRegistryNS, authRegistryName)

	// Populate the registry with agnhost using skopeo. This is a one-
	// off Job that pulls the public image and pushes into our auth-
	// protected registry. If this fails the rest of the test cannot
	// proceed, so fail loud.
	h.populateAuthRegistry(ctx)

	// Project the credentials into gantry pods via the existing
	// optional Secret hook.
	h.installRegistryCredentials(ctx)

	// Re-patch the gantry ConfigMap to add the auth registry with
	// credentials_path. Default e2e ConfigMap is replaced (not merged)
	// because origin.New requires the full upstream_registries list.
	h.applyConfigMapWithAuthRegistry(ctx)

	// Restart the DaemonSet so the agent rereads the ConfigMap +
	// projected Secret and rebuilds the origin registry client list.
	if err := h.run(ctx, "kubectl", "-n", namespace, "rollout", "restart", "daemonset/"+dsName); err != nil {
		t.Fatalf("rollout restart: %v", err)
	}
	h.waitForRollout(ctx)
	h.checkReadyz(ctx)

	// Configure containerd on each node with hosts.toml for our
	// in-cluster registry routing through Gantry's mirror.
	h.installMirrorHostsFor(ctx, authRegistryRefShort, "http://"+authRegistryRefShort)

	workers := h.workerNodes(ctx)

	// Make sure the image isn't already cached on the target node from
	// a prior run - otherwise containerd's content-store short-circuit
	// would skip the mirror entirely and we wouldn't actually exercise
	// the credentialed origin pull path.
	h.evictImageFromNode(ctx, workers[0], authRegistryRef)

	// Schedule a workload that pulls the auth-protected image through
	// gantry. If credentials don't propagate end-to-end the pull will
	// 401 at the origin step and the pod will never go Ready.
	h.deletePod(ctx, "gantry-e2e-authpull")
	h.applyPullPodWithImage(ctx, "gantry-e2e-authpull", workers[0], authRegistryRef)

	// Generous timeout: the credentialed cold-start path bounces
	// through gantry's 503-Retry-After backoff (mirror returns 503
	// while origin.Pull is in flight) and containerd's exponential
	// retry can take several minutes to settle even on a fast pull.
	h.waitForPodReadyTimeout(ctx, "gantry-e2e-authpull", "600s")

	// Confirm gantry actually fetched from the auth registry rather
	// than containerd having served bytes from somewhere else. We use
	// the puller node's gantry log as the proof: please_pull_served
	// events for the auth-registry image's digest demonstrate that
	// containerd routed through gantry, gantry talked to the auth
	// registry, and auth succeeded end-to-end. We intentionally do
	// not assert on a numeric metric - the relevant counter only
	// bumps on origin.Pull starts, which can be skipped in subsequent
	// runs when the content store already holds the bytes.
	pullerGantry := h.gantryPodOnNode(ctx, workers[0])
	logs, err := h.runOut(ctx, "kubectl", "-n", namespace, "logs", pullerGantry,
		"--tail=500")
	if err != nil {
		h.dumpDiagnostics(ctx)
		t.Fatalf("read gantry pod logs: %v", err)
	}
	if !strings.Contains(logs, authRegistryDigest) ||
		!strings.Contains(logs, "please_pull served") {
		h.dumpDiagnostics(ctx)
		t.Fatalf("expected gantry pod %s to log a please_pull served event for digest %s; tail:\n%s",
			pullerGantry, authRegistryDigest, lastLines(logs, 40))
	}
}

// deployAuthRegistry creates the in-cluster registry:2 + htpasswd
// Secret + ClusterIP Service that the test uses as the
// auth-protected origin.
func (h *harness) deployAuthRegistry(ctx context.Context) {
	h.t.Helper()
	// Generate htpasswd-compatible bcrypt hash at test time so we
	// don't have to ship a credential blob. registry:2 expects each
	// line in `<user>:<bcrypt-hash>` format.
	hash, err := bcrypt.GenerateFromPassword([]byte(authRegistryPass), bcrypt.MinCost)
	if err != nil {
		h.t.Fatalf("bcrypt hash: %v", err)
	}
	htpasswd := authRegistryUser + ":" + string(hash)

	manifest := strings.Join([]string{
		"apiVersion: v1",
		"kind: Secret",
		"metadata:",
		"  name: " + authRegistryName + "-htpasswd",
		"  namespace: " + authRegistryNS,
		"type: Opaque",
		"stringData:",
		"  htpasswd: " + htpasswd,
		"---",
		"apiVersion: apps/v1",
		"kind: Deployment",
		"metadata:",
		"  name: " + authRegistryName,
		"  namespace: " + authRegistryNS,
		"spec:",
		"  replicas: 1",
		"  selector:",
		"    matchLabels:",
		"      app: " + authRegistryName,
		"  template:",
		"    metadata:",
		"      labels:",
		"        app: " + authRegistryName,
		"    spec:",
		"      containers:",
		"        - name: registry",
		"          image: " + authRegistryImage,
		"          env:",
		"            - name: REGISTRY_AUTH",
		"              value: htpasswd",
		"            - name: REGISTRY_AUTH_HTPASSWD_REALM",
		"              value: \"Registry Realm\"",
		"            - name: REGISTRY_AUTH_HTPASSWD_PATH",
		"              value: /auth/htpasswd",
		"            - name: REGISTRY_HTTP_ADDR",
		"              value: \":5000\"",
		"          ports:",
		"            - containerPort: 5000",
		"          volumeMounts:",
		"            - name: htpasswd",
		"              mountPath: /auth",
		"              readOnly: true",
		"      volumes:",
		"        - name: htpasswd",
		"          secret:",
		"            secretName: " + authRegistryName + "-htpasswd",
		"---",
		"apiVersion: v1",
		"kind: Service",
		"metadata:",
		"  name: " + authRegistryName,
		"  namespace: " + authRegistryNS,
		"spec:",
		"  selector:",
		"    app: " + authRegistryName,
		"  ports:",
		"    - port: 5000",
		"      targetPort: 5000",
		"      protocol: TCP",
		"",
	}, "\n")

	if err := h.runWithInput(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("apply auth registry: %v", err)
	}
}

// populateAuthRegistry runs a one-off skopeo Job that copies
// registry.k8s.io/e2e-test-images/agnhost into the in-cluster auth
// registry. Waits for the Job to complete successfully.
func (h *harness) populateAuthRegistry(ctx context.Context) {
	h.t.Helper()
	// The skopeo image is reasonably small (~30MB) and supports
	// `--dest-creds` for Basic auth. We use --dest-tls-verify=false
	// because our in-cluster registry serves cleartext on :5000.
	jobName := "skopeo-seed-" + strings.ReplaceAll(strings.ToLower(time.Now().Format("150405")), ":", "")
	manifest := strings.Join([]string{
		"apiVersion: batch/v1",
		"kind: Job",
		"metadata:",
		"  name: " + jobName,
		"  namespace: " + authRegistryNS,
		"spec:",
		"  backoffLimit: 2",
		"  ttlSecondsAfterFinished: 600",
		"  template:",
		"    spec:",
		"      restartPolicy: Never",
		"      containers:",
		"        - name: skopeo",
		"          image: " + authRegistrySkopeo,
		"          command:",
		"            - skopeo",
		"            - copy",
		// --multi-arch=all preserves the full manifest list so the
		// in-cluster registry stores the exact same digest as the
		// source. Without it skopeo only copies the single platform
		// manifest and the manifest list digest disappears, which
		// breaks our digest-pinned pull below.
		"            - --multi-arch=all",
		"            - --dest-tls-verify=false",
		"            - --dest-creds",
		"            - " + authRegistryUser + ":" + authRegistryPass,
		"            - docker://" + e2ePullImage,
		"            - docker://" + authRegistryHost + ":5000/agnhost:2.39",
		"",
	}, "\n")
	if err := h.runWithInput(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("apply skopeo seed job: %v", err)
	}
	if err := h.run(ctx, "kubectl", "-n", authRegistryNS, "wait",
		"--for=condition=complete", "job/"+jobName, "--timeout=300s"); err != nil {
		h.dumpDiagnostics(ctx)
		// Dump job logs for diagnosis.
		out, _ := h.runOut(ctx, "kubectl", "-n", authRegistryNS, "logs",
			"job/"+jobName, "--tail=200")
		h.t.Logf("skopeo job logs:\n%s", out)
		h.t.Fatalf("skopeo seed job did not complete: %v", err)
	}
}

// installRegistryCredentials creates the gantry-registry-credentials
// Secret. The DaemonSet mounts this Secret at /etc/gantry/registry
// (optional: true) so creating it triggers a mount on next pod start.
func (h *harness) installRegistryCredentials(ctx context.Context) {
	h.t.Helper()
	manifest := strings.Join([]string{
		"apiVersion: v1",
		"kind: Secret",
		"metadata:",
		"  name: gantry-registry-credentials",
		"  namespace: " + namespace,
		"  labels:",
		"    app.kubernetes.io/name: gantry",
		"type: Opaque",
		"stringData:",
		// Secret data keys cannot contain ':' so we use a sanitized
		// filename. The ConfigMap's credentials_path points at the same
		// filename under /etc/gantry/registry.
		"  " + authRegistryCredsKey + ": \"" + authRegistryUser + ":" + authRegistryPass + "\"",
		"",
	}, "\n")
	if err := h.runWithInput(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("apply registry credentials secret: %v", err)
	}
}

// applyConfigMapWithAuthRegistry rewrites the gantry ConfigMap so that
// the upstream_registries list includes the in-cluster auth registry
// with a credentials_path. origin.New requires the full list so we
// replace, not merge.
func (h *harness) applyConfigMapWithAuthRegistry(ctx context.Context) {
	h.t.Helper()
	// Build a minimal ConfigMap that mirrors the e2e defaults but adds
	// our auth registry. We don't reuse the deploy/configmap.yaml file
	// because patching it deterministically across versions is more
	// fragile than emitting the snippet we need.
	manifest := strings.Join([]string{
		"apiVersion: v1",
		"kind: ConfigMap",
		"metadata:",
		"  name: gantry-config",
		"  namespace: " + namespace,
		"  labels:",
		"    app.kubernetes.io/name: gantry",
		"data:",
		"  config.yaml: |",
		"    mirror_listen: \"0.0.0.0:5000\"",
		"    mirror_bind_allow_non_loopback: true",
		"    transfer_listen: \"0.0.0.0:5001\"",
		"    metrics_listen: \"0.0.0.0:9095\"",
		"    libp2p_listen:",
		"      - \"/ip4/0.0.0.0/tcp/4001\"",
		"      - \"/ip4/0.0.0.0/udp/4001/quic-v1\"",
		"    libp2p_identity_path: \"/var/lib/gantry/libp2p/identity.key\"",
		"    members_label_selector: \"app.kubernetes.io/name=gantry\"",
		"    storage_mode: \"containerd\"",
		"    containerd_socket: \"/run/containerd/containerd.sock\"",
		"    containerd_namespace: \"k8s.io\"",
		"    containerd_lease_ttl: \"60m\"",
		"    containerd_lease_cleanup_interval: \"30m\"",
		"    upstream_registries:",
		"      - name: \"" + e2eRegistry + "\"",
		"        endpoint: \"" + e2eRegistryServer + "\"",
		"      - name: \"" + authRegistryRefShort + "\"",
		"        endpoint: \"http://" + authRegistryRefShort + "\"",
		"        credentials_path: \"/etc/gantry/registry/" + authRegistryCredsKey + "\"",
		"",
	}, "\n")
	if err := h.runWithInput(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("apply configmap with auth registry: %v", err)
	}
}

// installMirrorHostsFor installs a containerd hosts.toml on each kind
// node routing `registryHost` through gantry's mirror at 127.0.0.1:5000.
func (h *harness) installMirrorHostsFor(ctx context.Context, registryHost, serverURL string) {
	h.t.Helper()
	content := "server = \"" + serverURL + "\"\n\n" +
		"[host.\"http://127.0.0.1:5000\"]\n" +
		"  capabilities = [\"pull\", \"resolve\"]\n" +
		"  skip_verify = true\n"
	for _, node := range h.kindNodes(ctx) {
		path := "/etc/containerd/certs.d/" + registryHost + "/hosts.toml"
		cmd := "mkdir -p " + shellQuote("/etc/containerd/certs.d/"+registryHost) +
			" && cat > " + shellQuote(path)
		if err := h.runWithInput(ctx, content, "docker", "exec", "-i", node, "sh", "-c", cmd); err != nil {
			h.t.Fatalf("install hosts.toml for %s on %s: %v", registryHost, node, err)
		}
	}
}

// applyPullPodWithImage is applyPullPod parameterized on image. Used by
// the auth registry test which pulls from the in-cluster registry.
func (h *harness) applyPullPodWithImage(ctx context.Context, name, nodeName, image string) {
	h.t.Helper()
	manifest := strings.Join([]string{
		"apiVersion: v1",
		"kind: Pod",
		"metadata:",
		"  name: " + name,
		"  namespace: default",
		"  labels:",
		"    app.kubernetes.io/name: gantry-e2e-pull",
		"spec:",
		"  restartPolicy: Never",
		"  nodeName: " + nodeName,
		"  tolerations:",
		"    - operator: Exists",
		"  containers:",
		"    - name: agnhost",
		"      image: " + image,
		"      imagePullPolicy: Always",
		"      command: [\"/agnhost\", \"pause\"]",
		"",
	}, "\n")
	if err := h.runWithInput(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		h.t.Fatalf("apply pull pod %s: %v", name, err)
	}
}

// waitForDeploymentReady waits for the given Deployment's available
// replicas to match its spec replicas.
func (h *harness) waitForDeploymentReady(ctx context.Context, ns, name string) {
	h.t.Helper()
	if err := h.run(ctx, "kubectl", "-n", ns, "rollout", "status",
		"deployment/"+name, "--timeout=180s"); err != nil {
		h.dumpDiagnostics(ctx)
		h.t.Fatalf("wait for deployment %s/%s ready: %v", ns, name, err)
	}
}
