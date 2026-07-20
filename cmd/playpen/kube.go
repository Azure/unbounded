// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

// activeDeadlineGrace is added to the TTL when computing the pod's
// activeDeadlineSeconds. The in-pod reaper deletes the run at expiry (TTL); the
// kubelet-enforced deadline is only the backstop for a wedged reaper, so it sits
// slightly beyond the TTL.
const activeDeadlineGrace = 2 * time.Minute

// DefaultPodImage is the container image used for the demo pod. It ships the
// playpen binary itself, whose hidden `server` subcommand configures the
// pod-side VXLAN overlay. The value is overridden at build time via
// `-X main.DefaultPodImage=...` so a released client points at the matching
// pushed image.
var DefaultPodImage = "ghcr.io/azure/playpen:dev"

// playpenScheme registers the API types playpen touches: core/RBAC types via
// the client-go scheme plus the unbounded net types used for slice membership.
func playpenScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(netv1alpha1.AddToScheme(scheme))

	return scheme
}

// newClient builds a controller-runtime client honoring the configured
// kubeconfig context.
func newClient(cfg Config) (client.Client, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}

	if cfg.KubeContext != "" {
		overrides.CurrentContext = cfg.KubeContext
	}

	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig (context %q): %w", cfg.KubeContext, err)
	}

	c, err := client.New(restCfg, client.Options{Scheme: playpenScheme()})
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}

	return c, nil
}

// newInClusterClient builds a controller-runtime client from the pod's mounted
// ServiceAccount. It is used by the in-pod reaper running inside the demo pod.
func newInClusterClient() (client.Client, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster config: %w", err)
	}

	c, err := client.New(restCfg, client.Options{Scheme: playpenScheme()})
	if err != nil {
		return nil, fmt.Errorf("create in-cluster client: %w", err)
	}

	return c, nil
}

// ensureTempNode creates the temporary Node object that gives the local box a
// mesh identity, then patches its status with the internal IP so the
// unbounded-net controller matches it to a site. The Node uses GenerateName so
// concurrent runs never collide, and it carries an expires-at annotation making
// it the run's garbage-collection anchor: deleting it cascades to every owned
// resource, and the reaper deletes it once the expiry passes. The created Node
// (with its assigned Name and UID) is returned so callers can wire owner
// references and tell the pod its own node name.
func ensureTempNode(ctx context.Context, c client.Client, cfg Config, pubKey string, now time.Time) (*corev1.Node, error) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: cfg.NodeName + "-",
			Labels: map[string]string{
				TempNodeLabelKey:   cfg.Namespace,
				AKSManagedLabelKey: "false",
				SiteLabelKey:       cfg.NodeSite,
			},
			Annotations: map[string]string{
				WireGuardPubKeyAnnotation: pubKey,
				ExpiresAtAnnotation:       cfg.expiryString(now),
				TTLAnnotation:             cfg.TTL.String(),
			},
		},
		Spec: corev1.NodeSpec{
			PodCIDR:  cfg.NodePodCIDR,
			PodCIDRs: []string{cfg.NodePodCIDR},
		},
	}

	if err := c.Create(ctx, node); err != nil {
		return nil, fmt.Errorf("create node: %w", err)
	}

	// Patch status.addresses (a subresource; no kubelet sets it for us). AKS
	// node controllers race to modify the freshly created node, so retry on
	// conflict against the latest version.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Name: node.Name}, fresh); err != nil {
			return fmt.Errorf("get node %q: %w", node.Name, err)
		}

		fresh.Status.Addresses = []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: cfg.NodeInternalIP},
			{Type: corev1.NodeHostName, Address: node.Name},
		}

		return c.Status().Update(ctx, fresh)
	}); err != nil {
		return nil, fmt.Errorf("update node %q status: %w", node.Name, err)
	}

	return node, nil
}

// nodeOwnerRef builds an owner reference to the run's Node anchor. The Node is
// cluster-scoped and owns the run's namespaced Pod, so deleting the Node
// cascade-deletes the Pod. The shared namespace and reaper RBAC are not owned by
// any run and persist across runs.
func nodeOwnerRef(node *corev1.Node) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "Node",
		Name:       node.Name,
		UID:        node.UID,
	}
}

// sharedLabels returns the labels stamped on the shared (non-run-owned)
// namespace and reaper RBAC objects so they can be found by namespace scope.
func sharedLabels(cfg Config) map[string]string {
	return map[string]string{
		TempNodeLabelKey: cfg.Namespace,
	}
}

// ensureSharedRBAC creates the shared ServiceAccount plus the ClusterRole and
// ClusterRoleBinding granting the in-pod reaper the minimal node
// get/list/delete permissions it needs. These are shared by every run in the
// namespace: created if missing, not owned by any run, and never reaped. It
// returns the ServiceAccount name to set on the pod.
func ensureSharedRBAC(ctx context.Context, c client.Client, cfg Config) (string, error) {
	labels := sharedLabels(cfg)
	clusterName := cfg.reaperClusterName()

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ReaperServiceAccountName,
			Namespace: cfg.Namespace,
			Labels:    labels,
		},
	}
	if err := c.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create reaper service account: %w", err)
	}

	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   clusterName,
			Labels: labels,
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"get", "list", "delete"},
		}},
	}
	if err := c.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create reaper cluster role: %w", err)
	}

	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   clusterName,
			Labels: labels,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     clusterName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      ReaperServiceAccountName,
			Namespace: cfg.Namespace,
		}},
	}
	if err := c.Create(ctx, binding); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create reaper cluster role binding: %w", err)
	}

	return ReaperServiceAccountName, nil
}

// waitForSliceMembership polls the site's SiteNodeSlices until the temporary
// node's public key appears, confirming the controller published us to the
// mesh. It is best-effort and returns nil on timeout.
func waitForSliceMembership(ctx context.Context, c client.Client, cfg Config, pubKey string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		list := &netv1alpha1.SiteNodeSliceList{}
		if err := c.List(ctx, list); err == nil {
			for i := range list.Items {
				if list.Items[i].SiteName != cfg.NodeSite {
					continue
				}

				for _, n := range list.Items[i].Nodes {
					if n.WireGuardPublicKey == pubKey {
						return true
					}
				}
			}
		}

		time.Sleep(2 * time.Second)
	}

	return false
}

// deleteNode deletes a Node by name with background propagation so the cluster
// garbage collector cascades to its owned Pod, ServiceAccount, ClusterRole, and
// ClusterRoleBinding. A missing Node is not an error.
func deleteNode(ctx context.Context, c client.Client, name string) error {
	policy := metav1.DeletePropagationBackground
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}

	if err := c.Delete(ctx, node, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete node %q: %w", name, err)
	}

	return nil
}

// cleanupRun best-effort deletes a run using a fresh bounded context, so it can
// run during shutdown after the caller's context has already been cancelled.
func cleanupRun(c client.Client, nodeName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := deleteNode(ctx, c, nodeName); err != nil {
		fmt.Printf("warning: deleting run %q: %v\n", nodeName, err)
		return
	}

	fmt.Printf("deleted run %q\n", nodeName)
}

// listRuns returns the playpen Node anchors scoped to the given namespace (the
// value of the temp label), i.e. the runs owned by this user/namespace.
func listRuns(ctx context.Context, c client.Client, namespace string) ([]corev1.Node, error) {
	list := &corev1.NodeList{}
	if err := c.List(ctx, list, client.MatchingLabels{TempNodeLabelKey: namespace}); err != nil {
		return nil, fmt.Errorf("list playpen nodes: %w", err)
	}

	return list.Items, nil
}

// deleteAllRuns deletes every playpen run in the namespace scope.
func deleteAllRuns(ctx context.Context, c client.Client, namespace string) ([]string, error) {
	nodes, err := listRuns(ctx, c, namespace)
	if err != nil {
		return nil, err
	}

	deleted := make([]string, 0, len(nodes))

	for i := range nodes {
		if err := deleteNode(ctx, c, nodes[i].Name); err != nil {
			return deleted, err
		}

		deleted = append(deleted, nodes[i].Name)
	}

	return deleted, nil
}

// reapExpired deletes every playpen run in the namespace scope whose expiry has
// passed, cascading to each run's owned resources. It returns the names of the
// Node anchors it deleted so callers (notably the in-pod reaper) can detect when
// they have reaped themselves.
func reapExpired(ctx context.Context, c client.Client, namespace string, now time.Time) ([]string, error) {
	nodes, err := listRuns(ctx, c, namespace)
	if err != nil {
		return nil, err
	}

	var reaped []string

	for i := range nodes {
		if !isExpired(nodes[i].Annotations[ExpiresAtAnnotation], now) {
			continue
		}

		if err := deleteNode(ctx, c, nodes[i].Name); err != nil {
			return reaped, err
		}

		reaped = append(reaped, nodes[i].Name)
	}

	return reaped, nil
}

// deleteSharedResources deletes the resources playpen shares across every run
// in a namespace scope and otherwise (on purpose) never cleans up: the reaper
// ClusterRoleBinding, ClusterRole, and ServiceAccount, the bootstrapped Site and
// SiteGatewayPoolAssignment, and finally the shared namespace itself. Callers are
// expected to have already removed the per-run Node anchors (which cascade to
// their pods and per-run RBAC); this only targets the shared, unowned objects.
//
// Each delete is best-effort: a missing object is not an error, so cleanup is
// idempotent and tolerates partially provisioned scopes. It returns the
// human-readable descriptions of the objects it actually deleted.
func deleteSharedResources(ctx context.Context, c client.Client, cfg Config) ([]string, error) {
	clusterName := cfg.reaperClusterName()

	// Order matters only for readability; deletes are independent. The binding
	// and role are cluster-scoped and named per namespace scope.
	targets := []struct {
		desc string
		obj  client.Object
	}{
		{
			desc: fmt.Sprintf("clusterrolebinding %q", clusterName),
			obj:  &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterName}},
		},
		{
			desc: fmt.Sprintf("clusterrole %q", clusterName),
			obj:  &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: clusterName}},
		},
		{
			desc: fmt.Sprintf("serviceaccount %q/%q", cfg.Namespace, ReaperServiceAccountName),
			obj:  &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: ReaperServiceAccountName, Namespace: cfg.Namespace}},
		},
		{
			desc: fmt.Sprintf("sitegatewaypoolassignment %q", cfg.NodeSite),
			obj:  &netv1alpha1.SiteGatewayPoolAssignment{ObjectMeta: metav1.ObjectMeta{Name: cfg.NodeSite}},
		},
		{
			desc: fmt.Sprintf("site %q", cfg.NodeSite),
			obj:  &netv1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: cfg.NodeSite}},
		},
		{
			desc: fmt.Sprintf("namespace %q", cfg.Namespace),
			obj:  &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.Namespace}},
		},
	}

	deleted := make([]string, 0, len(targets))

	for _, t := range targets {
		if err := c.Delete(ctx, t.obj); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return deleted, fmt.Errorf("delete %s: %w", t.desc, err)
		}

		deleted = append(deleted, t.desc)
	}

	return deleted, nil
}

// ensureNamespace creates the shared working namespace if it does not exist. It
// is shared by every run and is never deleted by the reaper or by down.
func ensureNamespace(ctx context.Context, c client.Client, cfg Config) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   cfg.Namespace,
			Labels: sharedLabels(cfg),
		},
	}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %q: %w", cfg.Namespace, err)
	}

	return nil
}

// ensureSite bootstraps the dedicated unbounded-net site the temporary node
// joins. It creates a Site (named cfg.NodeSite) plus a SiteGatewayPoolAssignment
// binding that site to cfg.GatewayPools, so the roaming client has a home site
// that is distinct from wherever the demo pod runs; only then does inter-site
// traffic get relayed through the gateways the client peers with. Like the
// namespace and shared RBAC, both objects are shared across runs, created if
// missing, and never reaped. It is a no-op when they already exist.
func ensureSite(ctx context.Context, c client.Client, cfg Config) error {
	site := &netv1alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{
			Name:   cfg.NodeSite,
			Labels: sharedLabels(cfg),
		},
		Spec: netv1alpha1.SiteSpec{
			NodeCidrs: []string{cfg.SiteNodeCIDR},
			PodCidrAssignments: []netv1alpha1.PodCidrAssignment{
				{CidrBlocks: []string{cfg.SitePodCIDR}},
			},
		},
	}
	if err := c.Create(ctx, site); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create site %q: %w", cfg.NodeSite, err)
	}

	assignment := &netv1alpha1.SiteGatewayPoolAssignment{
		ObjectMeta: metav1.ObjectMeta{
			Name:   cfg.NodeSite,
			Labels: sharedLabels(cfg),
		},
		Spec: netv1alpha1.SiteGatewayPoolAssignmentSpec{
			Sites:        []string{cfg.NodeSite},
			GatewayPools: cfg.GatewayPools,
		},
	}
	if err := c.Create(ctx, assignment); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create site gateway pool assignment %q: %w", cfg.NodeSite, err)
	}

	return nil
}

// ensureDemoPod creates the self-configuring VXLAN server pod and waits for it
// to report a pod IP. The pod is scheduled by CPU architecture (and, in VM
// mode, KVM-capable) nodeSelector unless --pod-node pins it to an explicit
// node. It is owned by the Node anchor (so it is cascade-deleted with the run),
// runs under the per-run reaper ServiceAccount, and carries an
// activeDeadlineSeconds backstop derived from the TTL so the kubelet terminates
// it even if every reaper is gone. It returns the pod IP and the node the pod
// was scheduled onto.
func ensureDemoPod(ctx context.Context, c client.Client, cfg Config, node *corev1.Node, saName string) (string, string, error) {
	privileged := true

	nodeSelector, err := cfg.podNodeSelector()
	if err != nil {
		return "", "", err
	}

	args, err := serverArgs(cfg, node.Name)
	if err != nil {
		return "", "", err
	}

	container := corev1.Container{
		Name:            "server",
		Image:           cfg.PodImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"playpen"},
		Args:            args,
		SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
		// Request only a small CPU share so the pod schedules easily, but allow
		// bursting up to 4 cores so the guest (cloud-hypervisor vCPUs, plus the
		// swtpm and netboot proxy) can decompress and write the OS image quickly.
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("500m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("4"),
			},
		},
		Env: []corev1.EnvVar{{
			Name:      "POD_IP",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}},
		}},
	}

	var volumes []corev1.Volume

	// The guest runs under cloud-hypervisor (KVM). Although the pod is
	// Privileged (which grants access to the host /dev), mounting the char
	// devices explicitly makes the KVM/tun requirement clear.
	charDevice := corev1.HostPathCharDev
	volumes = append(volumes,
		corev1.Volume{
			Name: "dev-kvm",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/dev/kvm", Type: &charDevice},
			},
		},
		corev1.Volume{
			Name: "dev-net-tun",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/dev/net/tun", Type: &charDevice},
			},
		},
	)
	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{Name: "dev-kvm", MountPath: "/dev/kvm"},
		corev1.VolumeMount{Name: "dev-net-tun", MountPath: "/dev/net/tun"},
	)

	activeDeadline := int64((cfg.TTL + activeDeadlineGrace) / time.Second)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName:    cfg.PodName + "-",
			Namespace:       cfg.Namespace,
			Labels:          map[string]string{TempNodeLabelKey: cfg.Namespace, "app": "playpen-server"},
			OwnerReferences: []metav1.OwnerReference{nodeOwnerRef(node)},
		},
		Spec: corev1.PodSpec{
			NodeName:              cfg.PodNode,
			NodeSelector:          nodeSelector,
			RestartPolicy:         corev1.RestartPolicyNever,
			ServiceAccountName:    saName,
			ActiveDeadlineSeconds: &activeDeadline,
			Tolerations:           []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			Containers:            []corev1.Container{container},
			Volumes:               volumes,
		},
	}

	if err := c.Create(ctx, pod); err != nil {
		return "", "", fmt.Errorf("create pod: %w", err)
	}

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		fresh := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: pod.Name}, fresh); err == nil {
			if fresh.Status.PodIP != "" && fresh.Status.Phase == corev1.PodRunning {
				return fresh.Status.PodIP, fresh.Spec.NodeName, nil
			}
		}

		time.Sleep(2 * time.Second)
	}

	return "", "", fmt.Errorf("pod %q did not become ready with an IP", pod.Name)
}

// serverArgs builds the argument vector passed to the `playpen server`
// subcommand running inside the demo pod. The pod configures its own VXLAN
// overlay endpoint from these flags (see server.go) and runs the in-pod reaper
// scoped to its namespace, deleting its own Node anchor (selfNodeName) once the
// TTL expires. POD_IP is provided to the container via the downward API rather
// than as a flag.
func serverArgs(cfg Config, selfNodeName string) ([]string, error) {
	if _, err := cfg.clientUnderlayIP(); err != nil {
		return nil, err
	}

	args := []string{
		"server",
		"--namespace", cfg.Namespace,
		"--self-node-name", selfNodeName,
		"--vni", strconv.Itoa(cfg.VNI),
		"--vxlan-port", strconv.Itoa(cfg.VXLANPort),
		"--vxlan-interface", cfg.VXLANInterface,
		"--overlay-remote-ip", cfg.OverlayRemoteIP,
		"--overlay-prefix", strconv.Itoa(cfg.OverlayPrefix),
		"--overlay-mtu", strconv.Itoa(cfg.OverlayMTU),
		"--node-pod-cidr", cfg.NodePodCIDR,
		"--uplink", serverUplink,
	}

	args = append(args,
		"--vm-memory", strconv.Itoa(cfg.VMMemoryMiB),
		"--vm-cpus", strconv.Itoa(cfg.VMCPUs),
		"--vm-mac", cfg.VMMAC,
		"--vm-disk-size", strconv.Itoa(cfg.VMDiskSizeGiB),
		"--netboot-proxy-port", strconv.Itoa(cfg.NetbootProxyPort),
		"--bridge-interface", cfg.BridgeInterface,
		"--tap-interface", cfg.TapInterface,
		"--redfish-port", strconv.Itoa(cfg.RedfishPort),
		"--redfish-username", cfg.RedfishUsername,
		"--redfish-password", cfg.RedfishPassword,
		"--redfish-device-id", cfg.RedfishDeviceID,
	)

	return args, nil
}
