// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func testSite(name string) *unboundedv1alpha3.Site {
	return &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)}, Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Racer: &unboundedv1alpha3.RacerComponentSpec{SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: ptr.To(true)}}}}}
}

func envValues(c corev1.Container) map[string]string {
	out := map[string]string{}
	for _, e := range c.Env {
		out[e.Name] = e.Value
	}

	return out
}

// Shipping contracts are checked against the operator's resource constructors.
func TestShippingMTLS(t *testing.T) {
	const ns = "custom-system"

	d := controlDeployment(ns, component.Config{})

	pod := d.Spec.Template.Spec
	if len(pod.Containers) != 1 || pod.ServiceAccountName != controlPlaneName {
		t.Fatal("missing controlplane container or service account")
	}

	dataplane := dataplaneDaemonSet(ns, component.Config{}, testSite("rack-a")).Spec.Template.Spec
	for _, p := range []corev1.PodSpec{pod, dataplane} {
		c := p.Containers[0]
		for _, key := range []string{"RACER_ALLOW_UNSIGNED", "RACER_SIGNING_KEY", "RACER_VERIFY_KEYS_DIR", "RACER_CONFIG_VERIFY_KEYS_DIR", "RACER_PEER_KEYS_DIR", "RACER_CONFIG_KEYS_DIR"} {
			if _, exists := envValues(c)[key]; exists {
				t.Fatalf("unexpected legacy signing setting %s", key)
			}
		}

		assertPodIdentityEnv(t, c)
		assertTrustBundleMount(t, p, p.ServiceAccountName == controlPlaneName)
	}

	verbs := map[string]bool{}
	foundBinding := false

	for _, obj := range sharedResources(ns) {
		var rules []rbacv1.PolicyRule

		switch o := obj.(type) {
		case *rbacv1.Role:
			rules = o.Rules
		case *rbacv1.ClusterRole:
			rules = o.Rules
		case *rbacv1.RoleBinding:
			if o.Name == stateRoleName {
				foundBinding = o.Namespace == ns && o.RoleRef == (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: stateRoleName}) && reflect.DeepEqual(o.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: controlPlaneName, Namespace: ns}})
			}
		}

		for _, rule := range rules {
			if !slices.Contains(rule.Resources, "secrets") && !slices.Contains(rule.Resources, "*") {
				continue
			}

			if obj.GetObjectKind().GroupVersionKind().Kind != "Role" || obj.GetNamespace() != ns || obj.GetName() != stateRoleName || !reflect.DeepEqual(rule.APIGroups, []string{""}) || !reflect.DeepEqual(rule.Resources, []string{"secrets"}) {
				t.Fatal("Secret permission escaped the state Role")
			}

			for _, verb := range rule.Verbs {
				if !slices.Contains([]string{"get", "update", "create"}, verb) {
					t.Fatalf("unnecessary Secret verb %s", verb)
				}

				if verb == "get" || verb == "update" {
					if !reflect.DeepEqual(rule.ResourceNames, []string{"racer-ca"}) {
						t.Fatal("CA Secret read/update must be name-restricted")
					}
				} else if len(rule.ResourceNames) != 0 {
					t.Fatal("Secret create must be namespace-wide")
				}

				verbs[verb] = true
			}
		}
	}

	if len(verbs) != 3 || !foundBinding {
		t.Fatal("missing managed CA RBAC")
	}

	p := dataplane
	if len(p.Containers) != 1 {
		t.Fatal("expected one dataplane container")
	}

	c := p.Containers[0]

	for key, want := range map[string]string{
		"RACER_ENROLL_URL":          "https://racer-controlplane.custom-system.svc:8444/v3/enroll",
		"RACER_CONTROL_SERVER_NAME": "racer-controlplane.custom-system.svc",
		"RACER_CONTROL_TOKEN_FILE":  "/var/run/racer-control/token",
	} {
		if envValues(c)[key] != want {
			t.Fatalf("%s = %q, want %q", key, envValues(c)[key], want)
		}
	}

	tokenFound := false

	for _, v := range p.Volumes {
		if v.Projected == nil {
			continue
		}

		if len(v.Projected.Sources) != 1 {
			t.Fatal("unexpected token projection")
		}

		token := v.Projected.Sources[0].ServiceAccountToken
		tokenFound = token != nil && token.Audience == "racer-control" && token.Path == "token" && ptr.Deref(token.ExpirationSeconds, 0) == 3600
	}

	if !tokenFound {
		t.Fatal("missing audience-bound control token")
	}

	tokenMounted := false

	for _, mount := range c.VolumeMounts {
		if mount.Name == "control-token" && mount.MountPath+"/token" == envValues(c)["RACER_CONTROL_TOKEN_FILE"] && mount.ReadOnly {
			tokenMounted = true
		}
	}

	if !tokenMounted {
		t.Fatal("control token file is not mounted")
	}
}

func assertPodIdentityEnv(t *testing.T, c corev1.Container) {
	t.Helper()

	for name, field := range map[string]string{"RACER_POD_NAME": "metadata.name", "RACER_POD_NAMESPACE": "metadata.namespace", "RACER_POD_UID": "metadata.uid"} {
		count := 0

		for _, env := range c.Env {
			if env.Name != name {
				continue
			}

			count++

			if env.Value != "" || env.ValueFrom == nil || env.ValueFrom.FieldRef == nil || env.ValueFrom.FieldRef.APIVersion != "v1" || env.ValueFrom.FieldRef.FieldPath != field {
				t.Fatalf("%s must use downward API %s", name, field)
			}
		}

		if count != 1 {
			t.Fatalf("expected one %s, got %d", name, count)
		}
	}
}

func TestEnrollmentVerificationRBAC(t *testing.T) {
	const namespace = "custom-system"

	objects := sharedResources(namespace)
	allowed := func(account, group, resource, verb, name, ns string) bool {
		for _, obj := range objects {
			var (
				rules []rbacv1.PolicyRule
				ref   rbacv1.RoleRef
			)

			switch role := obj.(type) {
			case *rbacv1.ClusterRole:
				rules = role.Rules
				ref = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}
			case *rbacv1.Role:
				if role.Namespace != ns {
					continue
				}

				rules = role.Rules
				ref = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}
			default:
				continue
			}

			bound := false

			for _, binding := range objects {
				var subjects []rbacv1.Subject

				switch b := binding.(type) {
				case *rbacv1.ClusterRoleBinding:
					if b.RoleRef == ref {
						subjects = b.Subjects
					}
				case *rbacv1.RoleBinding:
					if b.RoleRef == ref && b.Namespace == ns {
						subjects = b.Subjects
					}
				}

				bound = bound || slices.Contains(subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: account, Namespace: namespace})
			}

			for _, rule := range rules {
				if bound && slices.Contains(rule.APIGroups, group) && slices.Contains(rule.Resources, resource) && slices.Contains(rule.Verbs, verb) && (len(rule.ResourceNames) == 0 || slices.Contains(rule.ResourceNames, name)) {
					return true
				}
			}
		}

		return false
	}

	for _, tc := range []struct {
		group, resource, name, namespace string
		verbs                            []string
	}{
		{"authentication.k8s.io", "tokenreviews", "", "", []string{"create"}},
		{"", "pods", "actual-pod", namespace, []string{"get", "list", "watch", "delete"}},
		{"", "nodes", "actual-node", "", []string{"get", "list", "watch"}},
		{unboundedv1alpha3.GroupVersion.Group, "sites", "rack-a", "", []string{"get", "list", "watch"}},
		{"apps", "daemonsets", "racer-rack-a", namespace, []string{"get"}},
		{"apps", "replicasets", "racer-controlplane-revision", namespace, []string{"get"}},
		{"apps", "deployments", controlPlaneName, namespace, []string{"get"}},
		{"", "secrets", "racer-ca", namespace, []string{"get", "update", "create"}},
		{"", "configmaps", "racer-trust", namespace, []string{"get", "list", "watch", "create", "update", "delete"}},
		{"coordination.k8s.io", "leases", "racer-controlplane", namespace, []string{"get", "list", "watch", "create", "update", "patch"}},
	} {
		for _, verb := range tc.verbs {
			if !allowed(controlPlaneName, tc.group, tc.resource, verb, tc.name, tc.namespace) {
				t.Errorf("controlplane cannot %s %s/%s %s in %q", verb, tc.group, tc.resource, tc.name, tc.namespace)
			}
		}
	}

	for _, account := range []string{controlPlaneName, dataplaneName} {
		for _, ns := range []string{namespace, "other-namespace"} {
			for _, verb := range []string{"delete", "deletecollection", "patch", "update"} {
				want := account == controlPlaneName && ns == namespace && verb == "delete"
				if got := allowed(account, "", "pods", verb, "actual-pod", ns); got != want {
					t.Errorf("%s %s Pods in %s = %v, want %v", account, verb, ns, got, want)
				}
			}

			for _, name := range []string{"racer-ca", "other-secret", "racer-config-signing", "racer-peer-signing"} {
				for _, verb := range []string{"get", "update", "list", "watch", "delete", "patch"} {
					want := account == controlPlaneName && ns == namespace && name == "racer-ca" && (verb == "get" || verb == "update")
					if got := allowed(account, "", "secrets", verb, name, ns); got != want {
						t.Errorf("%s %s Secret %s/%s = %v, want %v", account, verb, ns, name, got, want)
					}
				}
			}
		}
	}
}

func assertTrustBundleMount(t *testing.T, pod corev1.PodSpec, optional bool) {
	t.Helper()

	c := pod.Containers[0]
	if envValues(c)["RACER_TLS_TRUST_DIR"] != "/var/run/racer-trust" {
		t.Fatal("missing trust directory setting")
	}

	found := false

	for _, volume := range pod.Volumes {
		if volume.Secret != nil {
			t.Fatal("private or legacy Secret mounted in workload")
		}

		if volume.Projected != nil {
			for _, source := range volume.Projected.Sources {
				if source.Secret != nil {
					t.Fatal("private or legacy Secret projected into workload")
				}
			}
		}

		if volume.ConfigMap == nil || volume.ConfigMap.Name != "racer-trust" {
			continue
		}

		if !reflect.DeepEqual(volume.ConfigMap.Items, []corev1.KeyToPath{{Key: "bundle.json", Path: "bundle.json"}}) || ptr.Deref(volume.ConfigMap.Optional, false) != optional {
			t.Fatal("trust projection must contain only bundle.json with bootstrap-safe optionality")
		}

		for _, mount := range c.VolumeMounts {
			if mount.Name == volume.Name && mount.MountPath == "/var/run/racer-trust" && mount.ReadOnly && mount.SubPath == "" && mount.SubPathExpr == "" {
				found = true
			}
		}
	}

	if !found {
		t.Fatal("missing read-only rotating public trust mount")
	}
}

func TestManagementBindingMatchesPodIPProbes(t *testing.T) {
	d := dataplaneDaemonSet("custom", component.Config{}, testSite("rack-a"))

	p := d.Spec.Template.Spec
	if len(p.Containers) != 1 || len(p.InitContainers) != 1 {
		t.Fatal("missing profile containers")
	}

	c := p.Containers[0]

	for path, probe := range map[string]*corev1.Probe{"/startupz": c.StartupProbe, "/readyz": c.ReadinessProbe, "/livez": c.LivenessProbe} {
		if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Host != "" || probe.HTTPGet.Port.IntVal != 9090 || probe.HTTPGet.Port.StrVal != "" || probe.HTTPGet.Path != path {
			t.Fatalf("probe must target primary Pod IP: %+v", probe)
		}
	}

	podIPs := 0

	for _, e := range c.Env {
		if e.Name == "RACER_METRICS_ADDR" {
			t.Fatal("management binding overridden")
		}

		if e.Name == "RACER_POD_IP" {
			podIPs++

			if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.FieldRef == nil || e.ValueFrom.FieldRef.FieldPath != "status.podIP" {
				t.Fatal("missing downward primary Pod IP")
			}
		}
	}

	if podIPs != 1 {
		t.Fatal("expected one primary Pod IP")
	}
}

func TestShippingDataplaneProfile(t *testing.T) {
	d := dataplaneDaemonSet("custom", component.Config{}, testSite("rack-a"))

	p := d.Spec.Template.Spec
	if len(p.Containers) != 1 || len(p.InitContainers) != 1 {
		t.Fatal("missing profile containers")
	}

	c, b := p.Containers[0], p.InitContainers[0]
	if ptr.Deref(p.TerminationGracePeriodSeconds, 0) != 35 || c.StartupProbe == nil || c.StartupProbe.PeriodSeconds*c.StartupProbe.FailureThreshold != 180 {
		t.Fatal("lifecycle deadline drift")
	}

	for _, container := range []corev1.Container{c, b} {
		s := container.SecurityContext
		if s == nil || s.Privileged == nil || *s.Privileged || s.AllowPrivilegeEscalation == nil || *s.AllowPrivilegeEscalation || !ptr.Deref(s.ReadOnlyRootFilesystem, false) || s.Capabilities == nil || !reflect.DeepEqual(s.Capabilities.Drop, []corev1.Capability{"ALL"}) {
			t.Fatalf("%s has ambient privilege", container.Name)
		}

		for _, r := range []corev1.ResourceList{container.Resources.Requests, container.Resources.Limits} {
			if r.Cpu().Value() != 3 || r.Memory().Value() != 4*1024*1024*1024 {
				t.Fatal("Guaranteed profile resource drift")
			}
		}

		for _, mount := range container.VolumeMounts {
			if mount.SubPath != "" || mount.SubPathExpr != "" {
				t.Fatal("subPath prevents rotation")
			}
		}
	}

	s := c.SecurityContext
	if s.RunAsUser == nil || *s.RunAsUser != 0 || s.RunAsGroup == nil || *s.RunAsGroup != 65532 || !reflect.DeepEqual(s.Capabilities.Add, []corev1.Capability{"SYS_RESOURCE"}) || s.SeccompProfile == nil || s.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined || s.SeccompProfile.LocalhostProfile != nil {
		t.Fatal("main must use root, only SYS_RESOURCE, and Unconfined")
	}

	if !ptr.Deref(b.SecurityContext.RunAsNonRoot, false) || ptr.Deref(b.SecurityContext.RunAsUser, 0) != 65532 || ptr.Deref(b.SecurityContext.RunAsGroup, 0) != 65532 || len(b.SecurityContext.Capabilities.Add) != 0 || b.SecurityContext.SeccompProfile == nil || b.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("bootstrap must remain unprivileged")
	}

	if p.HostNetwork || p.HostPID || p.HostIPC {
		t.Fatal("unexpected host namespaces")
	}

	for name, value := range map[string]string{"RACER_IO_WORKERS": "1", "RACER_COMPUTE_WORKERS": "1", "RACER_SHARDS": "1", "RACER_BUFFERS_PER_NODE": "8", "RACER_SLAB_SIZE": "10737418240", "RACER_SLAB_PATH": "/cache/cache.slab", "RACER_STARTUP_SECONDS": "90", "RACER_STALL_SECONDS": "5", "RACER_DRAIN_SECONDS": "20", "RACER_QUIESCE_SECONDS": "5"} {
		if envValues(c)[name] != value {
			t.Fatalf("profile setting drift: %s", name)
		}
	}

	command := c.Args[0]

	wantCommand := strings.Join([]string{
		"ulimit -l 262144",
		". /bootstrap/identity",
		"chgrp 65532 /dev/racer",
		"chmod 2770 /dev/racer",
		`export RACER_CONTROL_PLANE_URL="https://racer-controlplane.custom.svc:8443/v3/$RACER_UNIVERSE/$RACER_NODE"`,
		"exec /usr/local/bin/racer-dataplane",
	}, "\n")
	if command != wantCommand || strings.Join(c.Command, " ") != "/bin/sh -ec" {
		t.Fatal("main must set memlock and bootstrap identity before directly executing the daemon")
	}

	for _, fragment := range []string{`-bootstrap-node="$NODE_NAME"`, `-bootstrap-universe="$POD_UNIVERSE"`, `-bootstrap-namespace="$POD_NAMESPACE"`, "-bootstrap-service=racer-controlplane"} {
		if !strings.Contains(b.Args[0], fragment) {
			t.Fatalf("missing bootstrap flag: %s", fragment)
		}
	}

	cache, sockets := false, false

	for _, v := range p.Volumes {
		if v.Name == "cache" {
			cache = v.HostPath != nil && ptr.Deref(v.HostPath.Type, "") == corev1.HostPathDirectoryOrCreate && v.HostPath.Path == "/var/lib/racer"
		}

		if v.Name == "sockets" {
			sockets = v.HostPath != nil && ptr.Deref(v.HostPath.Type, "") == corev1.HostPathDirectoryOrCreate && v.HostPath.Path == "/dev/racer"
		}
	}

	if !cache || strings.Contains(command, "rm ") || strings.Contains(command, "mkfs") {
		t.Fatal("slab must persist without wiping or formatting")
	}

	writableDirectory, socketDirectory := false, false

	for _, mount := range c.VolumeMounts {
		if mount.Name == "cache" && mount.MountPath == "/cache" && !mount.ReadOnly && mount.SubPath == "" && mount.SubPathExpr == "" {
			writableDirectory = true
		}

		if mount.Name == "sockets" && mount.MountPath == "/dev/racer" && !mount.ReadOnly && mount.SubPath == "" && mount.SubPathExpr == "" {
			socketDirectory = true
		}
	}

	if !writableDirectory {
		t.Fatal("resize requires a writable slab directory for .lock, .resize, rename and directory sync")
	}

	if !sockets || !socketDirectory {
		t.Fatal("socket replacement requires the whole /dev/racer host directory")
	}

	if d.Spec.UpdateStrategy.RollingUpdate.MaxSurge.IntVal != 0 || d.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.IntVal != 1 {
		t.Fatal("rollout must prevent concurrent slab writers")
	}
}

func TestStoragePolicyRBAC(t *testing.T) {
	var controller, bootstrap *rbacv1.ClusterRole

	bound := false

	for _, object := range sharedResources("custom") {
		switch object := object.(type) {
		case *rbacv1.ClusterRole:
			if object.Name == controlPlaneName {
				controller = object
			}

			if object.Name == bootstrapRoleName {
				bootstrap = object
			}
		case *rbacv1.ClusterRoleBinding:
			if object.Name == controlPlaneName {
				bound = object.RoleRef.Name == controlPlaneName && reflect.DeepEqual(object.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: controlPlaneName, Namespace: "custom"}})
			}
		}
	}

	if controller == nil || bootstrap == nil || !bound {
		t.Fatal("missing controller/bootstrap roles or controller binding")
	}

	for _, tc := range []struct {
		group, resource string
		verbs           []string
	}{
		{unboundedv1alpha3.GroupVersion.Group, "sites", []string{"get", "list", "watch"}},
		{"", "nodes", []string{"get", "list", "patch", "watch"}},
		{"", "nodes/status", nil},
	} {
		var verbs []string

		for _, rule := range controller.Rules {
			if slices.Contains(rule.APIGroups, "*") || slices.Contains(rule.Resources, "*") {
				t.Fatal("wildcard controller permission")
			}

			if slices.Contains(rule.APIGroups, tc.group) && slices.Contains(rule.Resources, tc.resource) {
				if len(rule.ResourceNames) != 0 {
					t.Fatal("dynamic Site/Node names must not be restricted")
				}

				verbs = append(verbs, rule.Verbs...)
			}
		}

		slices.Sort(verbs)

		if !slices.Equal(verbs, tc.verbs) {
			t.Fatalf("%s/%s verbs = %v, want %v", tc.group, tc.resource, verbs, tc.verbs)
		}
	}

	if !reflect.DeepEqual(bootstrap.Rules, []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"}}}) {
		t.Fatal("dataplane bootstrap gained storage-controller privileges")
	}
}

func TestSiteIdentityAndScheduling(t *testing.T) {
	site := testSite("rack-a")
	d := dataplaneDaemonSet("custom", component.Config{}, site)

	p := d.Spec.Template.Spec
	if !reflect.DeepEqual(p.NodeSelector, map[string]string{corev1.LabelOSStable: "linux"}) {
		t.Fatal("unexpected positive enrollment selector")
	}

	if !reflect.DeepEqual(p.Affinity.NodeAffinity, racermeta.RequiredNodeAffinity(site.Name)) {
		t.Fatal("shared membership affinity must be authoritative")
	}

	for _, tc := range siteAdmissionCases() {
		t.Run(tc.name, func(t *testing.T) {
			tc.labels[corev1.LabelOSStable] = "linux"
			n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{UID: "node-uid", Labels: tc.labels, Annotations: map[string]string{racermeta.UniverseKey: "foreign"}}}

			got := matchesNode(t, p, n)
			if got != tc.want {
				t.Fatalf("match=%v want=%v", got, tc.want)
			}

			// The constructor's bootstrap universe must agree with the shared
			// bootstrap guard, including canonical conflicts and exclusion.
			universe := envValues(p.InitContainers[0])["POD_UNIVERSE"]
			if err := racermeta.ValidateBootstrapNode(n, universe); (err == nil) != tc.want {
				t.Fatalf("bootstrap and scheduling disagree: %v", err)
			}

			n.UID = ""
			if racermeta.ValidateBootstrapNode(n, universe) == nil {
				t.Fatal("bootstrap accepted a missing Node UID")
			}
		})
	}

	if matchesNode(t, p, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{racermeta.SiteLabelKey: site.Name, corev1.LabelOSStable: "windows"}}}) {
		t.Fatal("dataplane must only schedule on Linux")
	}

	for _, name := range []string{"default", "rack-a", "rack.b", strings.Repeat("a", 57), strings.Repeat("a", 58), strings.Repeat("a", 63) + "." + strings.Repeat("b", 63)} {
		site := testSite(name)
		d := dataplaneDaemonSet("custom", component.Config{}, site)

		universe := racermeta.UniverseForSite(name)
		if d.Spec.Selector.MatchLabels[racermeta.UniverseKey] != universe || d.Spec.Template.Labels[racermeta.UniverseKey] != universe || envValues(d.Spec.Template.Spec.InitContainers[0])["POD_UNIVERSE"] != universe {
			t.Fatal("bootstrap/Pod/selector identity diverged")
		}

		if len(validation.IsDNS1123Subdomain(d.Name)) != 0 || len(d.Name) > 63 || !strings.HasPrefix(d.Name, "racer-") {
			t.Fatalf("unsafe name %s", d.Name)
		}

		if len(name) <= 57 && !strings.Contains(name, ".") {
			if d.Name != "racer-"+name {
				t.Fatalf("DaemonSet name = %q, want racer-%s", d.Name, name)
			}
		} else if !strings.HasPrefix(d.Name, "racer-site.") {
			t.Fatalf("expected encoded Site name, got %q", d.Name)
		}

		if !reflect.DeepEqual(d.OwnerReferences, []metav1.OwnerReference{component.SiteOwnerReference(site)}) {
			t.Fatal("missing Site controller owner")
		}
	}

	if SiteDaemonSetName("rack-a") == SiteDaemonSetName("rack.a") {
		t.Fatal("safe names collided")
	}
}

// Both unit and API-server tests exercise the same admission decisions. Node
// universe annotations/labels and deployment-profile labels are not enrollment.
func siteAdmissionCases() []struct {
	name   string
	labels map[string]string
	want   bool
} {
	return []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"default-enrollment", map[string]string{racermeta.SiteLabelKey: "rack-a"}, true},
		{"fallback", map[string]string{racermeta.DeprecatedSiteLabelKey: "rack-a"}, true},
		{"canonical-wins", map[string]string{racermeta.SiteLabelKey: "rack-a", racermeta.DeprecatedSiteLabelKey: "rack-b"}, true},
		{"conflict", map[string]string{racermeta.SiteLabelKey: "rack-b", racermeta.DeprecatedSiteLabelKey: "rack-a"}, false},
		{"canonical-empty", map[string]string{racermeta.SiteLabelKey: "", racermeta.DeprecatedSiteLabelKey: "rack-a"}, false},
		{"unassigned", map[string]string{}, false},
		{"old-mirror-only", map[string]string{racermeta.UniverseKey: "rack-a", racermeta.MetadataPrefix + "deployment-profile": "http-small-v1"}, false},
		{"excluded", map[string]string{racermeta.SiteLabelKey: "rack-a", racermeta.ExcludeLabelKey: "true"}, false},
		{"fallback-excluded", map[string]string{racermeta.DeprecatedSiteLabelKey: "rack-a", racermeta.ExcludeLabelKey: "true"}, false},
		{"explicit-inclusion", map[string]string{racermeta.SiteLabelKey: "rack-a", racermeta.ExcludeLabelKey: "false"}, true},
		{"case-sensitive-exclusion", map[string]string{racermeta.SiteLabelKey: "rack-a", racermeta.ExcludeLabelKey: "True"}, true},
		{"non-boolean-exclusion", map[string]string{racermeta.DeprecatedSiteLabelKey: "rack-a", racermeta.ExcludeLabelKey: "1"}, true},
		{"old-mirror-irrelevant", map[string]string{racermeta.SiteLabelKey: "rack-a", racermeta.UniverseKey: "foreign"}, true},
	}
}

func matchesNode(t *testing.T, spec corev1.PodSpec, node *corev1.Node) bool {
	t.Helper()

	if !labels.SelectorFromSet(spec.NodeSelector).Matches(labels.Set(node.Labels)) {
		return false
	}

	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil || spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatal("missing required Site affinity")
	}

	for _, term := range spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		if len(term.MatchFields) != 0 {
			t.Fatal("unexpected field affinity")
		}

		if len(term.MatchExpressions) == 0 {
			continue
		}

		selector := &metav1.LabelSelector{}
		for _, req := range term.MatchExpressions {
			selector.MatchExpressions = append(selector.MatchExpressions, metav1.LabelSelectorRequirement{Key: req.Key, Operator: metav1.LabelSelectorOperator(req.Operator), Values: req.Values})
		}

		compiled, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			t.Fatal(err)
		}

		if compiled.Matches(labels.Set(node.Labels)) {
			return true
		}
	}

	return false
}

func TestControlPlaneDefaultsAndNamespaceImages(t *testing.T) {
	cfg := component.Config{ImageRegistry: "example.test/team/", ImageTag: "v123"}

	d := controlDeployment("custom", cfg)
	if ptr.Deref(d.Spec.Replicas, 0) != 2 || d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || d.Spec.Strategy.RollingUpdate.MaxSurge.IntVal != 1 || d.Spec.Strategy.RollingUpdate.MaxUnavailable.IntVal != 1 || d.Spec.MinReadySeconds != 10 {
		t.Fatal("warm-standby rollout default drift")
	}

	c := d.Spec.Template.Spec.Containers[0]
	if c.Image != "example.test/team/racer-controlplane:v123" || !slices.Contains(c.Args, "-state-namespace=custom") || slices.Contains(c.Args, "-reserved-management-ports=9090,9443") || c.ReadinessProbe.HTTPGet.Path != "/readyz" || c.ReadinessProbe.HTTPGet.Port.StrVal != "health" || c.LivenessProbe.HTTPGet.Path != "/healthz" || c.LivenessProbe.HTTPGet.Port.StrVal != "health" {
		t.Fatal("controlplane image/flags/probes drift")
	}

	svc := controlService("custom")
	if labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(d.Spec.Template.Labels)) || svc.Spec.Selector[racermeta.MetadataPrefix+"serving-leader"] != "true" || svc.Spec.Selector[componentLabel] != controlPlaneName || svc.Spec.PublishNotReadyAddresses {
		t.Fatal("Service must select serving leader only")
	}

	if len(svc.Spec.Ports) != 3 || len(c.Ports) != 5 {
		t.Fatal("expected TLS subscription/enrollment/trust proof Service ports and container-only health/replica proof ports")
	}

	for name, port := range map[string]int32{"subscription": 8443, "enrollment": 8444, "replica-proof": 8445, "trust-proof": 8446, "health": 8081} {
		if !slices.Contains(c.Ports, corev1.ContainerPort{Name: name, ContainerPort: port, Protocol: corev1.ProtocolTCP}) {
			t.Fatalf("missing container port %s:%d", name, port)
		}

		if name != "health" && name != "replica-proof" && !slices.ContainsFunc(svc.Spec.Ports, func(p corev1.ServicePort) bool {
			return p.Name == name && p.Port == port && p.TargetPort.StrVal == name && p.Protocol == corev1.ProtocolTCP
		}) {
			t.Fatalf("missing Service port %s:%d", name, port)
		}
	}

	ds := dataplaneDaemonSet("custom", cfg, testSite("rack-a"))
	if ds.Namespace != "custom" || ds.Spec.Template.Spec.Containers[0].Image != "example.test/team/racer-dataplane:v123" || ds.Spec.Template.Spec.InitContainers[0].Image != c.Image {
		t.Fatal("dataplane namespace/images drift")
	}

	for _, obj := range sharedResources("custom") {
		if len(obj.GetOwnerReferences()) != 0 || (obj.GetNamespace() != "" && obj.GetNamespace() != "custom") || obj.GetObjectKind().GroupVersionKind().Empty() {
			t.Fatalf("bad shared resource %T", obj)
		}

		switch o := obj.(type) {
		case *rbacv1.RoleBinding:
			if o.Subjects[0].Namespace != "custom" {
				t.Fatal("RoleBinding namespace drift")
			}
		case *rbacv1.ClusterRoleBinding:
			if o.Subjects[0].Namespace != "custom" {
				t.Fatal("ClusterRoleBinding namespace drift")
			}
		}
	}
}
