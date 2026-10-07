// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer composes the Racer controllers and replica observer.
// Constructors compose only; serving requires locally validated replicated state.
// Authority owns accepted state, server owns HTTPS serving, members owns workload
// discovery and builders, and wire owns protocol encoding.
package racer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/server"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type Application struct {
	authority   *authority.Authority
	Topology    *TopologyReconciler
	Keyring     *KeyringReconciler
	Server      *server.Server
	Lifecycle   *server.Lifecycle
	Replication *Replication
}

// Assemble performs no Kubernetes calls, I/O, cryptography, or goroutine startup.
// c supplies indexed discovery (configured by SetupWithManager in production);
// reader must bypass the cache for authorization and durable-state validation.
func Assemble(cfg Config, c client.Client, reader client.Reader) *Application {
	cfg = cfg.effective()
	a := authority.New(cfg.authorityConfig(), authority.Dependencies{Writer: c, Reader: reader})
	lifecycle := server.NewLifecycle(a)
	replication := &Replication{config: cfg, Client: c, APIReader: reader, authority: a}

	return &Application{
		authority:   a,
		Topology:    &TopologyReconciler{Client: c, APIReader: reader, config: cfg, authority: a},
		Keyring:     &KeyringReconciler{config: cfg, authority: a},
		Server:      server.New(cfg.serverConfig(), c, a, lifecycle, replication),
		Lifecycle:   lifecycle,
		Replication: replication,
	}
}

func (a *Application) SetupWithManager(mgr ctrl.Manager) error {
	a.Lifecycle.SetCacheSync(mgr.GetCache().WaitForCacheSync)

	if err := mgr.Add(a.Lifecycle); err != nil {
		return fmt.Errorf("register serving lifecycle: %w", err)
	}

	if err := mgr.Add(a.Replication); err != nil {
		return fmt.Errorf("register replica observer: %w", err)
	}

	if err := mgr.Add(&publisherLifetime{replication: a.Replication}); err != nil {
		return fmt.Errorf("register publisher lifetime: %w", err)
	}

	if err := a.Topology.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register topology: %w", err)
	}

	if err := a.Keyring.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("register keyring: %w", err)
	}

	if err := mgr.Add(a.Server); err != nil {
		return fmt.Errorf("register HTTPS server: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("register liveness: %w", err)
	}

	return mgr.AddReadyzCheck("racer", a.Server.Ready)
}

// Run uses standard manager leader election. On leadership loss the process
// exits; Lease release-on-cancel stays disabled to avoid overlapping writers.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}

	if err := racerv1.AddToScheme(scheme); err != nil {
		return err
	}

	// controller-runtime disables default client-side QPS throttling here;
	// serving admission bounds concurrency and API priority/fairness governs load.
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return err
	}

	if err := cfg.validateReplication(); err != nil {
		return err
	}

	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	options := managerOptions(cfg, scheme)
	options.LeaderElectionResourceLockInterface = &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Namespace: cfg.Namespace, Name: "racer-controller"},
		Client:     kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: cfg.PodName + "/" + cfg.PodUID},
	}

	mgr, err := ctrl.NewManager(restConfig, options)
	if err != nil {
		return err
	}

	app := Assemble(cfg, mgr.GetClient(), mgr.GetAPIReader())
	// Recovery runs before election so every replica validates durable state.
	// First-install initialization uses resource-version CAS and is safe when
	// replicas race. Only the elected leader publishes subsequent changes.
	if err := app.Recover(ctx, mgr.GetClient()); err != nil {
		return err
	}

	if err := app.SetupWithManager(mgr); err != nil {
		return err
	}

	return mgr.Start(ctx)
}

// Recover runs the production startup guard before any manager runnable starts.
// The writer need not have a running cache; all reads use the authoritative reader
// supplied to Assemble. Constructors and recovery never grant serving authority.
func (a *Application) Recover(ctx context.Context, writer client.Writer) error {
	return a.authority.Recover(ctx, writer)
}

func managerOptions(cfg Config, scheme *runtime.Scheme) ctrl.Options {
	return ctrl.Options{
		Scheme:                        scheme,
		LeaderElection:                true,
		LeaderElectionReleaseOnCancel: false,
		Metrics:                       metricsserver.Options{BindAddress: cfg.MetricsAddress},
		HealthProbeBindAddress:        cfg.ProbeAddress,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}: {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
			// Named Secret RBAC requires this selector on both LIST and WATCH.
			&corev1.Secret{}:    {Namespaces: map[string]cache.Config{cfg.Namespace: {}}, Field: fields.OneTermEqualSelector("metadata.name", cfg.CredentialsSecretName)},
			&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{cfg.Namespace: {}}, Field: fields.OneTermEqualSelector("metadata.name", cfg.VersionConfigMapName)},
			&appsv1.DaemonSet{}: {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
		}},
	}
}

// Config contains controller runtime settings, not operator workload inputs.
type Config struct {
	Cluster                   wire.ClusterID
	Namespace                 string
	ControlAddress            string
	MetricsAddress            string
	ProbeAddress              string
	TLSCertificateFile        string
	TLSPrivateKeyFile         string
	PeerPort                  uint16
	DataplaneServiceAccount   string
	DaemonSetName             string
	CredentialsSecretName     string
	VersionConfigMapName      string
	InstallationConfigMapName string
	Limits                    server.Limits
	Rotation                  authority.RotationPolicy
	CertificateLifetime       time.Duration
	PodName                   string
	PodUID                    string
	ControllerServiceAccount  string
	ReplicationTokenFile      string
	ReplicationTrustFile      string
	ReplicationServerName     string
	ReplicationPort           uint16
	SnapshotMaxAge            time.Duration
}

// LoadConfig reads deployment configuration. Initialization state is deliberately
// not an environment setting: it is read authoritatively on every recovery.
func LoadConfig() (Config, error) {
	return ConfigFromLookup(os.LookupEnv)
}

// ConfigFromLookup parses the controller configuration without process-global state.
func ConfigFromLookup(lookup func(string) (string, bool)) (Config, error) {
	env := func(key, fallback string) string {
		if value, ok := lookup(key); ok {
			return value
		}

		return fallback
	}

	port, err := strconv.ParseUint(env("RACER_PEER_PORT", "8082"), 10, 16)
	if err != nil {
		return Config{}, fmt.Errorf("RACER_PEER_PORT must be an integer from 1 to 65535: %w", err)
	}

	cfg := Config{
		Cluster:                   wire.ClusterID(env("RACER_CLUSTER_ID", "")),
		Namespace:                 env("POD_NAMESPACE", "unbounded-system"),
		ControlAddress:            env("RACER_CONTROL_ADDRESS", ":8443"),
		MetricsAddress:            env("RACER_METRICS_ADDRESS", ":8080"),
		ProbeAddress:              env("RACER_PROBE_ADDRESS", ":8081"),
		TLSCertificateFile:        env("RACER_TLS_CERTIFICATE_FILE", "/etc/racer/tls/tls.crt"),
		TLSPrivateKeyFile:         env("RACER_TLS_PRIVATE_KEY_FILE", "/etc/racer/tls/tls.key"),
		PeerPort:                  uint16(port),
		DataplaneServiceAccount:   env("RACER_DATAPLANE_SERVICE_ACCOUNT", "racer-dataplane"),
		DaemonSetName:             env("RACER_DAEMONSET_NAME", "racer-dataplane"),
		CredentialsSecretName:     env("RACER_CREDENTIALS_SECRET_NAME", "racer-credentials"),
		VersionConfigMapName:      env("RACER_VERSION_CONFIGMAP_NAME", "racer-version"),
		InstallationConfigMapName: env("RACER_INSTALLATION_CONFIGMAP_NAME", "racer-installation"),
		PodName:                   env("POD_NAME", ""),
		PodUID:                    env("POD_UID", ""),
		ControllerServiceAccount:  env("RACER_CONTROLLER_SERVICE_ACCOUNT", "racer-controller"),
		ReplicationTokenFile:      env("RACER_REPLICATION_TOKEN_FILE", "/var/run/secrets/racer-controller/token"),
		ReplicationTrustFile:      env("RACER_REPLICATION_TRUST_FILE", "/etc/racer/tls/ca.crt"),
		ReplicationServerName:     env("RACER_REPLICATION_SERVER_NAME", "racer-controller."+env("POD_NAMESPACE", "unbounded-system")+".svc"),
		SnapshotMaxAge:            30 * time.Second,
		Limits: server.Limits{
			MaxConnections:          2*wire.MaxMembers + 128,
			MaxConcurrentHandshakes: 32,
			MaxPolls:                wire.MaxMembers,
			MaxConcurrentWrites:     128,
			MaxConcurrentBootstrap:  32,
			HeaderBytes:             16 * 1024,
			WriteTimeout:            30 * time.Second,
			HandshakeTimeout:        5 * time.Second,
			ShutdownTimeout:         10 * time.Second,
		},
		Rotation: authority.RotationPolicy{
			Interval:   24 * time.Hour,
			PrepareFor: time.Hour,
			RetainFor:  48 * time.Hour,
		},
	}

	replicationPort, err := strconv.ParseUint(env("RACER_REPLICATION_PORT", "8443"), 10, 16)
	if err != nil || replicationPort == 0 {
		return Config{}, fmt.Errorf("RACER_REPLICATION_PORT must be an integer from 1 to 65535")
	}

	cfg.ReplicationPort = uint16(replicationPort)

	for _, setting := range []struct {
		name  string
		value *time.Duration
	}{
		{"RACER_CERTIFICATE_LIFETIME", &cfg.CertificateLifetime},
		{"RACER_SNAPSHOT_MAX_AGE", &cfg.SnapshotMaxAge},
		{"RACER_HANDSHAKE_TIMEOUT", &cfg.Limits.HandshakeTimeout},
		{"RACER_ROTATION_INTERVAL", &cfg.Rotation.Interval},
		{"RACER_ROTATION_PREPARE_FOR", &cfg.Rotation.PrepareFor},
		{"RACER_ROTATION_RETAIN_FOR", &cfg.Rotation.RetainFor},
	} {
		if value, ok := lookup(setting.name); ok {
			duration, err := time.ParseDuration(value)
			if err != nil || duration <= 0 || duration%time.Second != 0 {
				return Config{}, fmt.Errorf("%s must be a positive duration in whole seconds", setting.name)
			}

			*setting.value = duration
		}
	}

	cfg = cfg.effective()

	return cfg, cfg.Validate()
}

// effective resolves optional lifetimes without I/O or validation side effects.
// Zero retains the defaults accepted by programmatic callers.
func (c Config) effective() Config {
	if c.CertificateLifetime == 0 {
		c.CertificateLifetime = wire.CertificateLifetime
	}

	if c.SnapshotMaxAge == 0 {
		c.SnapshotMaxAge = 30 * time.Second
	}

	return c
}

func (c Config) Validate() error {
	c = c.effective()
	if c.SnapshotMaxAge < 0 || c.SnapshotMaxAge > 0 && c.SnapshotMaxAge < time.Second {
		return fmt.Errorf("SnapshotMaxAge must be at least one second")
	}

	if !wire.ValidUUID(string(c.Cluster)) {
		return fmt.Errorf("cluster must be a UUID")
	}

	if len(validation.IsDNS1123Label(c.Namespace)) != 0 {
		return fmt.Errorf("namespace must be a DNS label")
	}

	if c.PeerPort == 0 {
		return fmt.Errorf("PeerPort must be from 1 to 65535")
	}

	for field, name := range map[string]string{"VersionConfigMapName": c.VersionConfigMapName, "InstallationConfigMapName": c.InstallationConfigMapName, "DaemonSetName": c.DaemonSetName, "CredentialsSecretName": c.CredentialsSecretName, "DataplaneServiceAccount": c.DataplaneServiceAccount} {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return fmt.Errorf("%s must be a DNS subdomain", field)
		}
	}

	if c.VersionConfigMapName == c.InstallationConfigMapName {
		return fmt.Errorf("VersionConfigMapName and InstallationConfigMapName must differ")
	}

	if err := c.serverConfig().Validate(); err != nil {
		return fmt.Errorf("server configuration: %v", err)
	}

	return c.validateRotation()
}

func (c Config) validateRotation() error {
	// Two minutes leaves a full poll turn between renewal at two-thirds of the
	// lifetime and expiry. X.509 and rotation deadlines have second precision.
	lifetime := c.CertificateLifetime
	if lifetime < 2*time.Minute || lifetime > wire.CertificateLifetime || lifetime%time.Second != 0 {
		return fmt.Errorf("CertificateLifetime must be from 2m to %s in whole seconds", wire.CertificateLifetime)
	}

	if c.Rotation.PrepareFor <= 0 || c.Rotation.Interval < c.Rotation.PrepareFor || c.Rotation.RetainFor < lifetime ||
		c.Rotation.Interval > 365*24*time.Hour || c.Rotation.RetainFor > 365*24*time.Hour {
		return fmt.Errorf("rotation requires positive PrepareFor <= Interval <= 8760h and CertificateLifetime <= RetainFor <= 8760h")
	}

	return nil
}

func (c Config) validateReplication() error {
	if len(validation.IsDNS1123Subdomain(c.PodName)) != 0 || c.PodUID == "" ||
		len(validation.IsDNS1123Subdomain(c.ControllerServiceAccount)) != 0 ||
		len(validation.IsDNS1123Subdomain(c.ReplicationServerName)) != 0 || c.ReplicationPort == 0 ||
		c.ReplicationTokenFile == "" || c.ReplicationTrustFile == "" {
		return fmt.Errorf("replication requires valid POD_NAME, POD_UID, controller identity, TLS trust, token, server name, and nonzero port")
	}

	return nil
}

const podNodeIndex = "spec.nodeName"

// Controllers start sources only under leadership and wait for cache sync before
// workers run. Queueing directly guarantees a first reconcile even for empty lists.
func initialEnqueue() source.Source {
	return source.Func(func(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		q.Add(singleton(ctx, nil)[0])

		return nil
	})
}

func podNodeKeys(obj client.Object) []string {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Spec.NodeName == "" {
		return nil
	}

	return []string{pod.Spec.NodeName}
}

func changes(relevant func(client.Object) bool, equal func(client.Object, client.Object) bool) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return relevant(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return relevant(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return relevant(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return (relevant(e.ObjectOld) || relevant(e.ObjectNew)) && !equal(e.ObjectOld, e.ObjectNew)
		},
	}
}

func nodeChanges() predicate.Predicate {
	return changes(func(client.Object) bool { return true }, func(a, b client.Object) bool {
		_, excludedA := a.GetLabels()[wire.ExclusionLabel]

		_, excludedB := b.GetLabels()[wire.ExclusionLabel]
		if a.GetUID() != b.GetUID() || a.GetName() != b.GetName() || excludedA != excludedB {
			return false
		}

		if a.GetLabels()[machinav1.MachineSiteLabelKey] != b.GetLabels()[machinav1.MachineSiteLabelKey] {
			return false
		}

		for _, key := range []string{wire.SharesAnnotation, wire.RDMANICsAnnotation, enrolledRDMANICsAnnotation, enrolledSharesAnnotation} {
			av, ap := a.GetAnnotations()[key]

			bv, bp := b.GetAnnotations()[key]
			if ap != bp || av != bv {
				return false
			}
		}

		return true
	})
}

func managedPodChanges(cfg Config) predicate.Predicate {
	return changes(func(obj client.Object) bool {
		if obj.GetNamespace() != cfg.Namespace {
			return false
		}

		owner := metav1.GetControllerOf(obj)

		return owner != nil && owner.APIVersion == "apps/v1" && owner.Kind == "DaemonSet" && owner.Name == cfg.DaemonSetName
	}, func(a, b client.Object) bool {
		x, xok := a.(*corev1.Pod)

		y, yok := b.(*corev1.Pod)
		if !xok || !yok {
			return false
		}

		return x.UID == y.UID && x.Spec.NodeName == y.Spec.NodeName && x.Status.PodIP == y.Status.PodIP && x.Status.Phase == y.Status.Phase && x.CreationTimestamp.Equal(&y.CreationTimestamp) && reflect.DeepEqual(x.DeletionTimestamp, y.DeletionTimestamp) && reflect.DeepEqual(x.OwnerReferences, y.OwnerReferences)
	})
}

func volumeChanges() predicate.Predicate {
	return changes(func(client.Object) bool { return true }, func(a, b client.Object) bool {
		x, xok := a.(*racerv1.ClusterVolume)

		y, yok := b.(*racerv1.ClusterVolume)
		if !xok || !yok {
			return false
		}

		return x.UID == y.UID && x.Name == y.Name && x.Spec.Type == y.Spec.Type
	})
}

func namedObjects(namespace string, names ...string) func(client.Object) bool {
	return func(obj client.Object) bool {
		if obj.GetNamespace() != namespace {
			return false
		}

		for _, name := range names {
			if obj.GetName() == name {
				return true
			}
		}

		return false
	}
}

func namedChanges(namespace string, names ...string) predicate.Predicate {
	return changes(namedObjects(namespace, names...), func(a, b client.Object) bool { return a.GetResourceVersion() == b.GetResourceVersion() })
}

// Ignore our own resource-version-only CAS writes to avoid an endless hot loop.
func versionChanges(cfg Config) predicate.Predicate {
	return changes(namedObjects(cfg.Namespace, cfg.VersionConfigMapName, cfg.InstallationConfigMapName), func(a, b client.Object) bool {
		x, xok := a.(*corev1.ConfigMap)

		y, yok := b.(*corev1.ConfigMap)
		if !xok || !yok {
			return false
		}

		return x.UID == y.UID && reflect.DeepEqual(x.Data, y.Data) && reflect.DeepEqual(x.Annotations, y.Annotations) && reflect.DeepEqual(x.Immutable, y.Immutable) && reflect.DeepEqual(x.DeletionTimestamp, y.DeletionTimestamp)
	})
}

func (c Config) serverConfig() server.Config {
	return server.Config{
		ControlAddress:        c.ControlAddress,
		TLSCertificateFile:    c.TLSCertificateFile,
		TLSPrivateKeyFile:     c.TLSPrivateKeyFile,
		ReplicationServerName: c.ReplicationServerName,
		Limits:                c.Limits,
	}
}

func (c Config) authorityConfig() authority.Config {
	return authority.Config{
		Cluster: c.Cluster, Namespace: c.Namespace,
		DataplaneServiceAccount: c.DataplaneServiceAccount, ControllerServiceAccount: c.ControllerServiceAccount,
		DaemonSetName: c.DaemonSetName, CredentialsSecretName: c.CredentialsSecretName,
		VersionConfigMapName: c.VersionConfigMapName, InstallationConfigMapName: c.InstallationConfigMapName,
		Rotation: c.Rotation, CertificateLifetime: c.CertificateLifetime, SnapshotMaxAge: c.SnapshotMaxAge,
		MaxTokenBytes: c.Limits.HeaderBytes,
	}
}

type TopologyReconciler struct {
	authority *authority.Authority
	client.Client
	APIReader   client.Reader
	config      Config
	hints       map[string]nodeHint
	enqueueHint func(reconcile.Request)
}

type nodeHint struct {
	node   *corev1.Node
	member string
}

// Reconcile builds from the synchronized cache, reads the version ConfigMap
// authoritatively, commits counters/hashes with CAS, then installs the result.
// Conflicts requeue from fresh inputs; missing established counters fail closed.
func (r *TopologyReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	if err := r.config.Validate(); err != nil {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	var err error
	if request.Namespace == "hints" {
		err = r.reconcileHint(ctx, request.Name)
	} else {
		err = r.reconcile(ctx)
	}
	// Cancellation takes precedence even if a transport concurrently reports Conflict.
	if ctx.Err() != nil {
		return ctrl.Result{}, reconcile.TerminalError(ctx.Err())
	}

	return ctrl.Result{}, err
}

func (r *TopologyReconciler) reconcile(ctx context.Context) error {
	update, err := r.authority.PublishTopology(ctx, r.observeTopology)
	if err != nil {
		return err
	}

	// Hint failures have their own keyed retries and never republish topology.
	return r.queueHints(ctx, update)
}

func (r *TopologyReconciler) observeTopology(ctx context.Context) (authority.TopologyObservation, error) {
	cfg := r.config

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return authority.TopologyObservation{}, err
	}

	var volumes racerv1.ClusterVolumeList
	if err := r.APIReader.List(ctx, &volumes); err != nil {
		return authority.TopologyObservation{}, err
	}

	catalog, err := members.BuildCatalog(volumes.Items)
	if err != nil {
		return authority.TopologyObservation{}, err
	}

	ownership, err := members.ReadWorkloadIdentities(ctx, r.APIReader, cfg.Namespace, cfg.DaemonSetName)
	if err != nil {
		return authority.TopologyObservation{}, err
	}
	// Indexed namespace-scoped queries avoid scanning unrelated Pods for each
	// Node. Ownership is still verified against the current DaemonSet UID.
	podsByNode := make(map[string][]corev1.Pod, len(nodes.Items))

	for _, node := range nodes.Items {
		if err := ctx.Err(); err != nil {
			return authority.TopologyObservation{}, err
		}

		var list corev1.PodList
		if err := r.List(ctx, &list, client.InNamespace(cfg.Namespace), client.MatchingFields{podNodeIndex: node.Name}); err != nil {
			return authority.TopologyObservation{}, err
		}

		podsByNode[node.Name] = list.Items
	}

	return authority.TopologyObservation{Nodes: nodes, Catalog: catalog, Input: members.Input{
		Nodes: nodes.Items, PodsByNode: podsByNode, Ownership: ownership, PeerPort: cfg.PeerPort,
	}}, nil
}

func (r *TopologyReconciler) queueHints(ctx context.Context, update authority.TopologyHints) error {
	r.hints = make(map[string]nodeHint, len(update.Nodes.Items))
	for i := range update.Nodes.Items {
		node := &update.Nodes.Items[i]
		hint := nodeHint{node: node.DeepCopy()}

		member, ok := update.Members[wire.NodeID(node.UID)]
		if !ok {
			if _, excluded := node.Labels[wire.ExclusionLabel]; !excluded {
				continue
			}
		} else {
			encoded, err := json.Marshal(member)
			if err != nil {
				return err
			}

			if len(encoded) > 64*1024 {
				return wire.TooLarge
			}

			hint.member = string(encoded)
		}

		r.hints[node.Name] = hint
		if err := r.reconcileHint(ctx, node.Name); err != nil {
			ctrl.LoggerFrom(ctx).V(1).Info("recovery hint retry", "node", node.Name, "error", err)

			if r.enqueueHint != nil {
				r.enqueueHint(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "hints", Name: node.Name}})
			}
		}
	}

	return nil
}

func (r *TopologyReconciler) reconcileHint(ctx context.Context, name string) error {
	hint, ok := r.hints[name]
	if !ok {
		return nil
	}

	var current corev1.Node
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, &current); err != nil {
		if apierrors.IsNotFound(err) {
			delete(r.hints, name)
			return nil
		}

		return err
	}

	if current.UID != hint.node.UID {
		delete(r.hints, name)
		return nil
	}

	_, excluded := current.Labels[wire.ExclusionLabel]
	if excluded {
		hint.member = ""
	} else if nodeChanges().Update(event.UpdateEvent{ObjectOld: hint.node, ObjectNew: &current}) {
		// The topology watch will publish the new inputs before supplying a hint.
		delete(r.hints, name)
		return nil
	}

	if current.Annotations[admittedMemberAnnotation] != hint.member {
		before := current.DeepCopy()
		if hint.member == "" {
			delete(current.Annotations, admittedMemberAnnotation)
		} else {
			if current.Annotations == nil {
				current.Annotations = map[string]string{}
			}

			current.Annotations[admittedMemberAnnotation] = hint.member
		}

		if err := r.Patch(ctx, &current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}

	delete(r.hints, name)

	return nil
}

func (r *TopologyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cfg := r.config

	installation, err := installationSource(mgr, cfg)
	if err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, podNodeIndex, podNodeKeys); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-topology").
		WatchesRawSource(installation).
		WatchesRawSource(source.Func(func(_ context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
			r.enqueueHint = q.Add
			return nil
		})).
		WatchesRawSource(initialEnqueue()).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(nodeChanges())).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(managedPodChanges(cfg))).
		Watches(&appsv1.DaemonSet{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(cfg.Namespace, cfg.DaemonSetName))).
		Watches(&racerv1.ClusterVolume{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(volumeChanges())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(cfg.Namespace, cfg.CredentialsSecretName))).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(versionChanges(cfg))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// singleton coalesces input changes without introducing a singleton CR.
func singleton(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "racer"}}}
}

const (
	enrolledSharesAnnotation   = members.EnrolledSharesAnnotation
	enrolledRDMANICsAnnotation = members.EnrolledRDMANICsAnnotation
	admittedMemberAnnotation   = members.AdmittedMemberAnnotation
)

type KeyringReconciler struct {
	config    Config
	authority *authority.Authority
}

func (r *KeyringReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	delay, err := r.authority.ReconcileCredentials(ctx)
	if ctx.Err() != nil {
		return ctrl.Result{}, reconcile.TerminalError(ctx.Err())
	}

	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: delay}, nil
}

func (r *KeyringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cfg := r.config

	installation, err := installationSource(mgr, cfg)
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-credentials").
		WatchesRawSource(installation).
		WatchesRawSource(initialEnqueue()).
		Watches(&racerv1.ClusterVolume{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(volumeChanges())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(cfg.Namespace, cfg.CredentialsSecretName))).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(versionChanges(cfg))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(r)
}

// Kubernetes field selectors cannot OR two names. Each controller watches the
// installation marker through a separate named cache; the manager caches only
// the version object. Authorization always uses the uncached API reader.
func installationSource(mgr ctrl.Manager, cfg Config) (source.SyncingSource, error) {
	c, err := cache.New(mgr.GetConfig(), cache.Options{
		Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper(),
		DefaultNamespaces:    map[string]cache.Config{cfg.Namespace: {}},
		DefaultFieldSelector: fields.OneTermEqualSelector("metadata.name", cfg.InstallationConfigMapName),
	})
	if err != nil {
		return nil, err
	}

	if err := mgr.Add(c); err != nil {
		return nil, err
	}

	return source.Kind[client.Object](c, &corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), versionChanges(cfg)), nil
}

const ReplicationAudience = authority.ReplicationAudience

// Replication observes durable authority on every replica. No request from a
// dataplane performs these reads. Only the elected publisher supplies image bytes.
type Replication struct {
	authority *authority.Authority
	config    Config
	Client    client.Client
	APIReader client.Reader
	mu        sync.Mutex
	leader    context.Context
}

type publisherLifetime struct{ replication *Replication }

func (*publisherLifetime) NeedLeaderElection() bool { return true }
func (p *publisherLifetime) Start(ctx context.Context) error {
	p.replication.mu.Lock()
	p.replication.leader = ctx
	p.replication.mu.Unlock()
	<-ctx.Done()

	return nil
}

func (r *Replication) isLeader() bool {
	_, leader := r.LeaderContext()
	return leader
}

func (*Replication) NeedLeaderElection() bool { return false }

func (r *Replication) interval() time.Duration {
	return min(5*time.Second, r.config.SnapshotMaxAge/3)
}

func (r *Replication) Start(ctx context.Context) error {
	// Credential observations continue while the follower poll is blocked.
	done := make(chan struct{})

	go func() {
		defer close(done)

		for ctx.Err() == nil {
			observation, cancel := context.WithTimeout(ctx, r.interval())
			r.observe(observation)
			cancel()

			if !replicationSleep(ctx, r.interval()) {
				return
			}
		}
	}()

	defer func() { <-done }()

	for ctx.Err() == nil {
		if !r.isLeader() {
			poll, cancel := context.WithTimeout(ctx, 2*r.interval()+r.config.Limits.WriteTimeout)
			if err := r.poll(poll, ctx); err != nil && ctx.Err() == nil {
				ctrl.LoggerFrom(ctx).V(1).Info("snapshot replication retry", "error", err)
			}

			cancel()
		}

		if !replicationSleep(ctx, r.interval()/2+time.Duration(rand.Int64N(int64(r.interval()/2)+1))) {
			break
		}
	}

	return nil
}

func replicationSleep(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *Replication) observe(ctx context.Context) {
	if err := r.authority.Observe(ctx); err != nil && ctx.Err() == nil {
		ctrl.LoggerFrom(ctx).V(1).Info("authority observation retry", "error", err)
	}
}

func (r *Replication) leaderAddress(ctx context.Context) (string, error) {
	cfg := r.config

	var lease coordv1.Lease
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: "racer-controller"}, &lease); err != nil {
		return "", err
	}

	if lease.Spec.HolderIdentity == nil || lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds <= 0 || time.Since(lease.Spec.RenewTime.Time) >= time.Duration(*lease.Spec.LeaseDurationSeconds)*time.Second {
		return "", wire.Unavailable
	}

	name, uid, ok := strings.Cut(*lease.Spec.HolderIdentity, "/")
	if !ok || name == "" || uid == "" {
		return "", wire.Unavailable
	}

	var pod corev1.Pod
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: name}, &pod); err != nil {
		return "", err
	}

	if string(pod.UID) != uid || !members.ControllerPod(&pod, cfg.Namespace, cfg.ControllerServiceAccount) || net.ParseIP(pod.Status.PodIP) == nil {
		return "", wire.Unavailable
	}

	return net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(cfg.ReplicationPort))), nil
}

func (r *Replication) poll(ctx, process context.Context) error {
	address, err := r.leaderAddress(ctx)
	if err != nil {
		return err
	}

	pem, err := os.ReadFile(r.config.ReplicationTrustFile)
	if err != nil {
		return err
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return wire.Unavailable
	}

	token, err := os.ReadFile(r.config.ReplicationTokenFile)
	if err != nil {
		return err
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: r.config.ReplicationServerName}, TLSHandshakeTimeout: r.interval(), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()

	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	path := "https://" + address + server.ReplicationPath
	if current, err := r.authority.Current(); err == nil {
		path += "?after=" + strconv.FormatUint(uint64(current.Sequence()), 10)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}

	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))

	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}

	defer func() {
		if err := response.Body.Close(); err != nil {
			ctrl.LoggerFrom(ctx).V(1).Info("close replication response", "error", err)
		}
	}()

	if response.StatusCode == http.StatusNoContent {
		return nil
	} // Not a freshness confirmation.

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("replication HTTP status %d", response.StatusCode)
	}

	image, err := wire.DecodePublication(response.Body)
	if err != nil {
		return err
	}

	return r.authority.AcceptReplica(ctx, process, image)
}

// LeaderContext snapshots publisher lifetime and leadership under the same lock.
func (r *Replication) LeaderContext() (context.Context, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.leader, r.leader != nil && r.leader.Err() == nil
}

func (r *Replication) PollInterval() time.Duration { return r.interval() }

func (r *Replication) AuthenticateReplica(ctx context.Context, request *http.Request) (string, time.Time, error) {
	identity, err := r.authority.AuthenticateReplica(ctx, request)
	return identity.UID(), identity.Expires(), err
}
