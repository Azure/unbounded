// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer implements the Racer server using controller-runtime directly.
// Constructors compose only; serving requires locally validated replicated state.
// Run and Assemble in controller.go wire the process and guarded startup.
// Topology and keyring own leader writes;
// replication, trust, authentication, and server own replica-local serving.
// workload.go provides the operator's pure builders; wire owns protocol encoding.
package racer

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type Application struct {
	Topology    *TopologyReconciler
	Keyring     *KeyringReconciler
	Server      *Server
	Lifecycle   *Lifecycle
	Replication *Replication
}

// Assemble performs no Kubernetes calls, I/O, cryptography, or goroutine startup.
// c supplies indexed discovery (configured by SetupWithManager in production);
// reader must bypass the cache for authorization and durable-state validation.
func Assemble(cfg Config, c client.Client, reader client.Reader) *Application {
	cfg = cfg.effective()
	publications := NewPublications()
	publications.maxAge = cfg.SnapshotMaxAge
	lifecycle := newLifecycle(publications)
	trust := &Trust{maxAge: cfg.SnapshotMaxAge}
	issuer := &Issuer{APIReader: reader, Config: cfg, Trust: trust}
	bootstrap := &Bootstrap{Client: c, APIReader: reader, Config: cfg, Issuer: issuer}
	// Serialize credential admission/pruning with topology's authoritative read
	// and publication commit. Informer ordering alone cannot provide this gate.
	catalogGate := newCatalogGate()
	issuer.CatalogGate = catalogGate
	replication := &Replication{Config: cfg, Client: c, APIReader: reader, Publications: publications, Trust: trust, CatalogGate: catalogGate}

	return &Application{
		Topology: &TopologyReconciler{
			Client:       c,
			APIReader:    reader,
			Config:       cfg,
			Publications: publications,
			Accepted:     make(AcceptedMembers),
			CatalogGate:  catalogGate,
			Trust:        trust,
		},
		Keyring: &KeyringReconciler{
			Client:      c,
			APIReader:   reader,
			Config:      cfg,
			CatalogGate: catalogGate,
			Trust:       trust,
		},
		Server: &Server{
			Config:       cfg,
			Trust:        trust,
			Bootstrap:    bootstrap,
			Publications: publications,
			Lifecycle:    lifecycle,
			Replication:  replication,
		},
		Lifecycle:   lifecycle,
		Replication: replication,
	}
}

func (a *Application) SetupWithManager(mgr ctrl.Manager) error {
	a.Lifecycle.waitForCacheSync = mgr.GetCache().WaitForCacheSync
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
	// Recovery validates permanent configuration and counters before starting any
	// manager runnable. The leader revalidates authoritatively for every commit.
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
	cfg, reader := a.Topology.runtimeConfig(), a.Topology.APIReader
	if err := cfg.Validate(); err != nil {
		return err
	}

	if err := ensureInstalled(ctx, writer, reader, cfg); err != nil {
		return fmt.Errorf("ensure Racer installation: %w", err)
	}

	if _, _, err := readVersion(ctx, reader, cfg); err != nil {
		return fmt.Errorf("recover Racer installation: %w", err)
	}

	return nil
}

func managerOptions(cfg Config, scheme *runtime.Scheme) ctrl.Options {
	return ctrl.Options{
		Scheme:                        scheme,
		LeaderElection:                true,
		LeaderElectionID:              "racer-controller",
		LeaderElectionNamespace:       cfg.Namespace,
		LeaderElectionReleaseOnCancel: false,
		Metrics:                       metricsserver.Options{BindAddress: cfg.MetricsAddress},
		HealthProbeBindAddress:        cfg.ProbeAddress,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}:       {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
			&corev1.Secret{}:    {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
			&corev1.ConfigMap{}: {Namespaces: map[string]cache.Config{cfg.Namespace: {}}},
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
	Limits                    Limits
	Rotation                  RotationPolicy
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

type Limits struct {
	MaxPolls               int
	MaxConcurrentWrites    int
	MaxConcurrentBootstrap int
	HeaderBytes            int
	WriteTimeout           time.Duration
	ShutdownTimeout        time.Duration
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
		return Config{}, fmt.Errorf("RACER_PEER_PORT: %w", wire.InvalidRequest)
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
		Limits: Limits{
			MaxPolls:               wire.MaxMembers,
			MaxConcurrentWrites:    128,
			MaxConcurrentBootstrap: 32,
			HeaderBytes:            16 * 1024,
			WriteTimeout:           30 * time.Second,
			ShutdownTimeout:        10 * time.Second,
		},
		Rotation: RotationPolicy{
			Interval:   24 * time.Hour,
			PrepareFor: time.Hour,
			RetainFor:  48 * time.Hour,
		},
	}

	replicationPort, err := strconv.ParseUint(env("RACER_REPLICATION_PORT", "8443"), 10, 16)
	if err != nil || replicationPort == 0 {
		return Config{}, fmt.Errorf("RACER_REPLICATION_PORT: %w", wire.InvalidRequest)
	}

	cfg.ReplicationPort = uint16(replicationPort)

	for _, setting := range []struct {
		name  string
		value *time.Duration
	}{
		{"RACER_CERTIFICATE_LIFETIME", &cfg.CertificateLifetime},
		{"RACER_SNAPSHOT_MAX_AGE", &cfg.SnapshotMaxAge},
		{"RACER_ROTATION_INTERVAL", &cfg.Rotation.Interval},
		{"RACER_ROTATION_PREPARE_FOR", &cfg.Rotation.PrepareFor},
		{"RACER_ROTATION_RETAIN_FOR", &cfg.Rotation.RetainFor},
	} {
		if value, ok := lookup(setting.name); ok {
			duration, err := time.ParseDuration(value)
			if err != nil || duration <= 0 || duration%time.Second != 0 {
				return Config{}, fmt.Errorf("%s: %w", setting.name, wire.InvalidRequest)
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

// frozenConfig retains a component's construction inputs at first use. Exported
// Config fields remain fixture-friendly before that boundary, but are never read
// again at runtime. Pass a pointer so even later calls do not read mutable input.
type frozenConfig struct {
	once  sync.Once
	value Config
}

func (f *frozenConfig) get(input *Config) Config {
	f.once.Do(func() { f.value = input.effective() })
	return f.value
}

func (c Config) Validate() error {
	c = c.effective()
	if c.SnapshotMaxAge < 0 || c.SnapshotMaxAge > 0 && c.SnapshotMaxAge < time.Second {
		return fmt.Errorf("snapshot maximum age: %w", wire.InvalidRequest)
	}

	if !wire.ValidUUID(string(c.Cluster)) || len(validation.IsDNS1123Label(c.Namespace)) != 0 || c.PeerPort == 0 {
		return fmt.Errorf("cluster, namespace, or peer port: %w", wire.InvalidRequest)
	}

	for _, name := range []string{c.VersionConfigMapName, c.InstallationConfigMapName, c.DaemonSetName, c.CredentialsSecretName, c.DataplaneServiceAccount} {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return fmt.Errorf("resource name: %w", wire.InvalidRequest)
		}
	}

	if c.VersionConfigMapName == c.InstallationConfigMapName ||
		c.Limits.MaxPolls <= 0 || c.Limits.MaxConcurrentWrites <= 0 || c.Limits.MaxConcurrentBootstrap <= 0 ||
		c.Limits.HeaderBytes <= 0 || c.Limits.WriteTimeout <= 0 || c.Limits.ShutdownTimeout <= 0 {
		return fmt.Errorf("resource names or limits: %w", wire.InvalidRequest)
	}

	// Two minutes leaves a full poll turn between renewal at two-thirds of the
	// lifetime and expiry. X.509 and rotation deadlines have second precision.
	lifetime := c.CertificateLifetime
	if lifetime < 2*time.Minute || lifetime > wire.CertificateLifetime || lifetime%time.Second != 0 {
		return fmt.Errorf("certificate lifetime: %w", wire.InvalidRequest)
	}

	if c.Rotation.PrepareFor <= 0 || c.Rotation.Interval < c.Rotation.PrepareFor || c.Rotation.RetainFor < lifetime ||
		c.Rotation.Interval > 365*24*time.Hour || c.Rotation.RetainFor > 365*24*time.Hour {
		return fmt.Errorf("credential names or rotation policy: %w", wire.InvalidRequest)
	}

	return nil
}

func (c Config) validateReplication() error {
	if len(validation.IsDNS1123Subdomain(c.PodName)) != 0 || c.PodUID == "" ||
		len(validation.IsDNS1123Subdomain(c.ControllerServiceAccount)) != 0 ||
		len(validation.IsDNS1123Subdomain(c.ReplicationServerName)) != 0 || c.ReplicationPort == 0 ||
		c.ReplicationTokenFile == "" || c.ReplicationTrustFile == "" {
		return fmt.Errorf("replication requires POD_NAME, POD_UID, controller identity, TLS trust, token, server name, and port: %w", wire.InvalidRequest)
	}

	return nil
}

// Lifecycle owns process serving, independently of the leader-owned publishers.
type Lifecycle struct {
	mu               sync.Mutex
	process          context.Context
	synced           bool
	serving          bool
	publications     *Publications
	waitForCacheSync func(context.Context) bool
}

func newLifecycle(p *Publications) *Lifecycle {
	return &Lifecycle{publications: p}
}

func (*Lifecycle) NeedLeaderElection() bool { return false }

// ProcessContext binds a request to the serving process lifetime.
// Missing or canceled process lifetime returns an already-canceled child.
func (l *Lifecycle) ProcessContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if l == nil {
		cancel()
		return ctx, cancel
	}

	l.mu.Lock()
	process := l.process
	l.mu.Unlock()

	if process == nil {
		cancel()
		return ctx, cancel
	}

	stop := context.AfterFunc(process, cancel)
	if process.Err() != nil {
		cancel()
	}

	return ctx, func() { stop(); cancel() }
}

func (l *Lifecycle) Start(ctx context.Context) error {
	l.mu.Lock()
	if l.process != nil {
		l.mu.Unlock()
		return wire.Conflict
	}

	l.process = ctx
	l.publications.bindProcess(ctx)
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		l.synced, l.serving = false, false
		l.mu.Unlock()
	}()

	if l.waitForCacheSync == nil || !l.waitForCacheSync(ctx) {
		if ctx.Err() != nil {
			return nil
		}

		return wire.Unavailable
	}

	l.mu.Lock()
	l.synced = ctx.Err() == nil
	l.mu.Unlock()
	<-ctx.Done()

	return nil
}

// SetServingReady is set only after the authenticated listener is accepting.
func (l *Lifecycle) SetServingReady(ready bool) {
	l.mu.Lock()
	l.serving = ready
	l.mu.Unlock()
}

func (l *Lifecycle) Ready(_ *http.Request) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.process == nil || l.process.Err() != nil || !l.synced || !l.serving {
		return wire.Unavailable
	}

	return l.publications.Ready(nil)
}

const (
	podNodeIndex       = "spec.nodeName"
	retryConflictDelay = 10 * time.Millisecond
)

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

		for _, key := range []string{wire.SharesAnnotation, wire.RailsAnnotation, wire.AlignmentAnnotation, enrolledSharesAnnotation} {
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

		return owner != nil && owner.APIVersion == "apps/v1" && owner.Kind == "DaemonSet" && slices.Contains(managedWorkloadNames(cfg), owner.Name)
	}, func(a, b client.Object) bool {
		x, xok := a.(*corev1.Pod)

		y, yok := b.(*corev1.Pod)
		if !xok || !yok {
			return false
		}

		return x.UID == y.UID && x.Spec.NodeName == y.Spec.NodeName && x.Status.PodIP == y.Status.PodIP && x.CreationTimestamp.Equal(&y.CreationTimestamp) && reflect.DeepEqual(x.DeletionTimestamp, y.DeletionTimestamp) && reflect.DeepEqual(x.OwnerReferences, y.OwnerReferences)
	})
}

func cacheChanges() predicate.Predicate {
	return changes(func(client.Object) bool { return true }, func(a, b client.Object) bool {
		x, xok := a.(*racerv1.ClusterCache)

		y, yok := b.(*racerv1.ClusterCache)
		if !xok || !yok {
			return false
		}

		return x.UID == y.UID && x.Name == y.Name
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
