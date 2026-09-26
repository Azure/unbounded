// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"reflect"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func workloadFixture(t *testing.T) *WorkloadReconciler {
	t.Helper()
	topology := initializedTopology(t)
	cfg := topology.Config
	cfg.ControlURL = "https://racer-controller.racer.svc:8443"
	cfg.DataplaneImage = "racer:test"

	return &WorkloadReconciler{Client: topology.Client, APIReader: topology.APIReader, Config: cfg}
}

func TestWorkloadProjectionAndStorage(t *testing.T) {
	r := workloadFixture(t)

	ds, err := r.DesiredDaemonSet()
	if err != nil {
		t.Fatal(err)
	}

	pod := ds.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.ServiceAccountName != r.Config.DataplaneServiceAccount {
		t.Fatal("automatic API token or wrong service account")
	}

	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		volumes[v.Name] = v
	}

	token := volumes["token"].Projected.Sources[0].ServiceAccountToken
	if token.Audience != wire.TokenAudience || *token.ExpirationSeconds != 3600 {
		t.Fatal("wrong token projection")
	}

	if volumes["keyring"].Secret.SecretName != r.Config.KeyringSecretName || len(volumes["keyring"].Secret.Items) != 1 || volumes["keyring"].Secret.Items[0].Key != "bundle.json" {
		t.Fatal("not a common bounded keyring projection")
	}

	if volumes["bootstrap"].ConfigMap.Name != r.Config.BootstrapTrustConfigMap {
		t.Fatal("bootstrap trust not independent")
	}

	for _, name := range []string{"identity", "slabs", "sockets"} {
		if volumes[name].HostPath == nil || *volumes[name].HostPath.Type != corev1.HostPathDirectoryOrCreate {
			t.Fatalf("missing persistent host mount %s", name)
		}
	}

	if volumes["identity"].HostPath.Path == volumes["slabs"].HostPath.Path {
		t.Fatal("private keys share disposable storage")
	}

	requirements := pod.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions
	if requirements[0].Key != wire.ExclusionLabel || requirements[0].Operator != corev1.NodeSelectorOpDoesNotExist {
		t.Fatal("exclusion must test presence")
	}

	for _, env := range pod.Containers[0].Env {
		if env.ValueFrom != nil && (env.Name != "RACER_POD_IP" || !reflect.DeepEqual(env.ValueFrom, &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}})) {
			t.Fatal("node identity must come from verified enrollment")
		}
	}

	for _, mount := range pod.Containers[0].VolumeMounts {
		if mount.SubPath != "" {
			t.Fatal("subPath prevents projection rotation")
		}

		if (mount.Name == "token" || mount.Name == "keyring" || mount.Name == "bootstrap") && !mount.ReadOnly {
			t.Fatal("writable credential projection")
		}
	}

	if len(pod.Containers[0].Args) != 0 || len(pod.Containers[0].Command) != 0 {
		t.Fatal("workload must use the image entrypoint")
	}
}

func TestWorkloadDataplaneEnvironment(t *testing.T) {
	for _, port := range []uint16{8082, 7443, 9090, 9091, 65535} {
		t.Run(strconv.Itoa(int(port)), func(t *testing.T) {
			r := workloadFixture(t)
			r.Config.PeerPort = port

			ds, err := r.DesiredDaemonSet()
			if err != nil {
				t.Fatal(err)
			}

			container := ds.Spec.Template.Spec.Containers[0]

			env := map[string]string{}
			for _, value := range container.Env {
				if _, exists := env[value.Name]; exists {
					t.Fatalf("duplicate configuration: %s", value.Name)
				}

				env[value.Name] = value.Value
			}

			diagnosticsPort := "9090"
			if port == 9090 {
				diagnosticsPort = "9091"
			}
			// Rust Config::from_lookup consumes these settings after kubelet expands
			// the Pod IP helper into the diagnostics address.
			expected := map[string]string{
				"RACER_CLUSTER_ID":            string(r.Config.Cluster),
				"RACER_CONTROL_ENDPOINT":      r.Config.ControlURL,
				"RACER_PEER_LISTEN":           "0.0.0.0:" + strconv.Itoa(int(port)),
				"RACER_POD_IP":                "",
				"RACER_DIAGNOSTICS_LISTEN":    "[$(RACER_POD_IP)]:" + diagnosticsPort,
				"RACER_TRUST_BUNDLE":          "/etc/racer/bootstrap/ca.crt",
				"RACER_SERVICE_ACCOUNT_TOKEN": "/var/run/racer-token/token",
				"RACER_SECRET_DIRECTORY":      "/etc/racer/keyring",
				"RACER_IDENTITY_DIRECTORY":    "/var/lib/racer/identity/private",
				"RACER_SLAB_DIRECTORY":        "/var/lib/racer/slabs",
			}
			if len(env) != len(expected) {
				t.Fatalf("unexpected configuration: %v", env)
			}

			for name, value := range expected {
				if env[name] != value {
					t.Errorf("%s = %q, want %q", name, env[name], value)
				}
			}

			if container.Ports[0].ContainerPort != int32(port) {
				t.Fatal("listener disagrees with advertised peer port")
			}

			assertWorkloadReadiness(t, ds)

			mounts := map[string]string{}
			for _, mount := range container.VolumeMounts {
				mounts[mount.Name] = mount.MountPath
			}

			for name, location := range map[string]string{
				"RACER_TRUST_BUNDLE":          path.Join(mounts["bootstrap"], "ca.crt"),
				"RACER_SERVICE_ACCOUNT_TOKEN": path.Join(mounts["token"], "token"),
				"RACER_SECRET_DIRECTORY":      mounts["keyring"],
				"RACER_IDENTITY_DIRECTORY":    path.Join(mounts["identity"], "private"),
				"RACER_SLAB_DIRECTORY":        mounts["slabs"],
			} {
				if env[name] != location {
					t.Errorf("%s does not match its mounted projection or storage", name)
				}
			}
		})
	}
}

// This contract also runs against API-defaulted objects in envtest.
func assertWorkloadReadiness(t *testing.T, ds *appsv1.DaemonSet) {
	t.Helper()

	rolling := ds.Spec.UpdateStrategy.RollingUpdate
	if ds.Spec.UpdateStrategy.Type != appsv1.RollingUpdateDaemonSetStrategyType || rolling == nil || rolling.MaxUnavailable == nil || *rolling.MaxUnavailable != intstr.FromInt32(1) || rolling.MaxSurge == nil || *rolling.MaxSurge != intstr.FromInt32(0) || ds.Spec.MinReadySeconds != 10 {
		t.Fatal("rollout must wait for sustained readiness with at most one unavailable Pod")
	}

	container := ds.Spec.Template.Spec.Containers[0]

	expected := &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromString("diagnostics"), Scheme: corev1.URISchemeHTTP}},
		PeriodSeconds: 5, TimeoutSeconds: 2, SuccessThreshold: 1, FailureThreshold: 1,
	}
	if !reflect.DeepEqual(container.ReadinessProbe, expected) || container.LivenessProbe != nil || container.StartupProbe != nil {
		t.Fatal("must probe actual Pod-IP readiness without dependency-driven restarts")
	}

	ports := map[string]int32{}

	for _, port := range container.Ports {
		if port.Protocol != corev1.ProtocolTCP || port.HostPort != 0 || port.HostIP != "" {
			t.Fatal("listeners must use Pod TCP ports")
		}

		ports[port.Name] = port.ContainerPort
	}

	if ports["diagnostics"] == 0 || ports["diagnostics"] == ports["peer"] {
		t.Fatal("diagnostics missing or collides with peer listener")
	}

	podIPSeen, diagnosticsSeen := false, false

	for _, env := range container.Env {
		switch env.Name {
		case "RACER_POD_IP":
			if podIPSeen || env.Value != "" || !reflect.DeepEqual(env.ValueFrom, &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}) {
				t.Fatal("bind address must come from the downward API Pod IP")
			}

			podIPSeen = true
		case "RACER_DIAGNOSTICS_LISTEN":
			if !podIPSeen || diagnosticsSeen || env.ValueFrom != nil || env.Value != "[$(RACER_POD_IP)]:"+strconv.Itoa(int(ports["diagnostics"])) {
				t.Fatal("diagnostics must expand the preceding Pod IP and match the probe port")
			}

			diagnosticsSeen = true
		default:
			if env.ValueFrom != nil {
				t.Fatal("unexpected indirect configuration")
			}
		}
	}

	if !podIPSeen || !diagnosticsSeen {
		t.Fatal("missing diagnostics bind configuration")
	}
}

func TestWorkloadReconcileCreateRepairAndNoop(t *testing.T) {
	r := workloadFixture(t)

	ctx := context.Background()

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	if err := initialEnqueue().Start(ctx, queue); err != nil || queue.Len() != 1 {
		t.Fatalf("startup did not enqueue missing workload: %v", err)
	}

	request, _ := queue.Get()
	defer queue.Done(request)

	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}

	ds := &appsv1.DaemonSet{}

	key := client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.DaemonSetName}
	if err := r.Get(ctx, key, ds); err != nil {
		t.Fatal(err)
	}

	ds.Annotations = map[string]string{"operator": "preserve"}
	ds.Spec.Template.Annotations = map[string]string{"rollout": "preserve"}

	ds.Spec.Template.Spec.Containers[0].Image = "drift"
	ds.Spec.Template.Spec.Containers[0].ReadinessProbe = nil

	ds.Spec.MinReadySeconds = 0
	if err := r.Update(ctx, ds); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}

	if err := r.Get(ctx, key, ds); err != nil {
		t.Fatal(err)
	}

	if ds.Spec.Template.Spec.Containers[0].Image != r.Config.DataplaneImage || ds.Annotations["operator"] != "preserve" || ds.Spec.Template.Annotations["rollout"] != "preserve" {
		t.Fatal("repair lost metadata or failed")
	}

	assertWorkloadReadiness(t, ds)

	version := ds.ResourceVersion

	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}

	if err := r.Get(ctx, key, ds); err != nil {
		t.Fatal(err)
	}

	if version != ds.ResourceVersion {
		t.Fatal("no-op wrote again")
	}
}

func TestWorkloadConflictCancellationAndOwnership(t *testing.T) {
	for _, scenario := range []string{"conflict", "cancel", "foreign", "selector", "bad URL", "missing image"} {
		t.Run(scenario, func(t *testing.T) {
			r := workloadFixture(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			ds, err := r.DesiredDaemonSet()
			if err != nil {
				t.Fatal(err)
			}

			ds.Spec.Template.Spec.Containers[0].Image = "old"
			if scenario == "foreign" {
				ds.Labels = nil
			}

			if scenario == "selector" {
				ds.Spec.Selector.MatchLabels = map[string]string{"foreign": "selector"}
			}

			if err := r.Create(ctx, ds); err != nil {
				t.Fatal(err)
			}

			writes := 0
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				writes++

				data, err := patch.Data(obj)
				if err != nil {
					t.Fatal(err)
				}

				var decoded struct {
					Metadata struct {
						ResourceVersion string `json:"resourceVersion"`
					} `json:"metadata"`
				}
				if err := json.Unmarshal(data, &decoded); err != nil || decoded.Metadata.ResourceVersion == "" {
					t.Fatal("patch lacks resource-version precondition")
				}

				return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "daemonsets"}, obj.GetName(), errors.New("conflict"))
			}})

			switch scenario {
			case "cancel":
				cancel()
			case "bad URL":
				r.Config.ControlURL = "http://insecure"
			case "missing image":
				r.Config.DataplaneImage = ""
			}

			result, err := r.Reconcile(ctx, ctrl.Request{})
			if scenario == "conflict" {
				if err != nil || result.RequeueAfter == 0 || writes != 1 {
					t.Fatalf("conflict: %v %v %d", result, err, writes)
				}
			} else if err == nil || writes != 0 {
				t.Fatalf("unsafe write: %v %d", err, writes)
			}
		})
	}
}

func TestWorkloadRepairsAddedFields(t *testing.T) {
	r := workloadFixture(t)
	ctx := context.Background()

	ds, err := r.DesiredDaemonSet()
	if err != nil {
		t.Fatal(err)
	}

	ds.Spec.Template.Spec.HostNetwork = true
	ds.Spec.Template.Spec.NodeName = "pinned-node"
	ds.Spec.Template.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true)
	ds.Spec.Template.Spec.Containers[0].Args = []string{"control"}

	ds.Spec.Template.Spec.Volumes = append(ds.Spec.Template.Spec.Volumes, corev1.Volume{Name: "unexpected", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	if err := r.Create(ctx, ds); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}

	if err := r.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
		t.Fatal(err)
	}

	pod := ds.Spec.Template.Spec
	if pod.HostNetwork || pod.NodeName != "" || pod.Containers[0].SecurityContext.Privileged != nil || len(pod.Containers[0].Args) != 0 || len(pod.Volumes) != 6 {
		t.Fatal("added fields escaped drift repair")
	}
}

func TestWorkloadRetryUsesFreshRead(t *testing.T) {
	for _, createRace := range []bool{false, true} {
		t.Run(map[bool]string{false: "patch conflict", true: "create race"}[createRace], func(t *testing.T) {
			r := workloadFixture(t)
			ctx := context.Background()

			ds, err := r.DesiredDaemonSet()
			if err != nil {
				t.Fatal(err)
			}

			ds.Spec.Template.Spec.Containers[0].Image = "old"

			base := r.Client.(client.WithWatch)
			if !createRace {
				if err := base.Create(ctx, ds); err != nil {
					t.Fatal(err)
				}
			}

			writes := 0
			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					writes++

					if err := c.Create(ctx, ds); err != nil {
						return err
					}

					return apierrors.NewAlreadyExists(schema.GroupResource{Group: "apps", Resource: "daemonsets"}, obj.GetName())
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					writes++
					if writes == 1 {
						ds.Annotations = map[string]string{"concurrent": "preserve"}
						if err := c.Update(ctx, ds); err != nil {
							return err
						}

						return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "daemonsets"}, obj.GetName(), errors.New("conflict"))
					}

					return c.Patch(ctx, obj, patch, opts...)
				},
			})

			result, err := r.Reconcile(ctx, ctrl.Request{})
			if err != nil || result.RequeueAfter == 0 || writes != 1 {
				t.Fatalf("retry not scheduled: %v %v", result, err)
			}

			if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
				t.Fatal(err)
			}

			if err := base.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
				t.Fatal(err)
			}

			if ds.Spec.Template.Spec.Containers[0].Image != r.Config.DataplaneImage || (!createRace && ds.Annotations["concurrent"] != "preserve") || writes != 2 {
				t.Fatal("retry did not repair from current state")
			}
		})
	}
}

func TestWorkloadCancellationAfterRead(t *testing.T) {
	for _, exists := range []bool{false, true} {
		r := workloadFixture(t)
		ctx, cancel := context.WithCancel(context.Background())

		ds, err := r.DesiredDaemonSet()
		if err != nil {
			t.Fatal(err)
		}

		if exists {
			ds.Spec.Template.Spec.Containers[0].Image = "old"
			if err := r.Create(ctx, ds); err != nil {
				t.Fatal(err)
			}
		}

		r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)

			cancel()

			return err
		}})

		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				t.Fatal("create after cancellation")
				return nil
			},
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				t.Fatal("patch after cancellation")
				return nil
			},
		})
		if _, err := r.Reconcile(ctx, ctrl.Request{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}

		cancel()
	}
}

func TestWorkloadInvalidEndpointFailsBeforeManagerStartup(t *testing.T) {
	for _, endpoint := range []string{"", "http://host", "https://host:0", "https://host:65536", "https://user@host", "https://host/path", "https://host?", "https://host/#fragment"} {
		r := workloadFixture(t)

		r.Config.ControlURL = endpoint
		if err := Run(context.Background(), r.Config); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("endpoint %q: %v", endpoint, err)
		}
	}
}

func TestWorkloadDeletionAndAPIFailure(t *testing.T) {
	r := workloadFixture(t)
	ctx := context.Background()

	ds, err := r.DesiredDaemonSet()
	if err != nil {
		t.Fatal(err)
	}

	ds.Finalizers = []string{"test.unbounded-cloud.io/hold"}
	if err := r.Create(ctx, ds); err != nil {
		t.Fatal(err)
	}

	if err := r.Delete(ctx, ds); err != nil {
		t.Fatal(err)
	}

	result, err := r.Reconcile(ctx, ctrl.Request{})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("terminating workload: %v %v", result, err)
	}

	if err := r.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
		t.Fatal(err)
	}

	if ds.DeletionTimestamp.IsZero() {
		t.Fatal("deletion interrupted")
	}

	ds.Finalizers = nil
	if err := r.Update(ctx, ds); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}

	if err := r.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
		t.Fatal(err)
	}

	if !ds.DeletionTimestamp.IsZero() {
		t.Fatal("workload not recreated")
	}

	apiFailure := apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "daemonsets"}, ds.Name, errors.New("denied"))

	r.APIReader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return apiFailure
	}})
	if _, err := r.Reconcile(ctx, ctrl.Request{}); !errors.Is(err, apiFailure) {
		t.Fatalf("API failure hidden: %v", err)
	}
}

func TestWorkloadDefaultsDoNotCauseRollout(t *testing.T) {
	r := workloadFixture(t)
	ctx := context.Background()

	ds, err := r.DesiredDaemonSet()
	if err != nil {
		t.Fatal(err)
	}
	// Defaults on the DaemonSet itself and its template metadata are supplied by
	// the API server, not workload drift. Pod defaults are explicit in desired state.
	ds.Spec.RevisionHistoryLimit = ptr.To(int32(10))

	ds.Spec.Template.CreationTimestamp = metav1.Time{}
	if err := r.Create(ctx, ds); err != nil {
		t.Fatal(err)
	}

	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		t.Fatal("default-only rollout")
		return nil
	}})
	if _, err := r.Reconcile(ctx, ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
}
