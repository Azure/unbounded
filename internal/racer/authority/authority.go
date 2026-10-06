// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/members"
	"github.com/Azure/unbounded/internal/racer/wire"
)

// Authority owns durable credential and publication operations and their shared
// admission gate. No operation exposes installation proofs or issuer material.
// Its stores, installation proofs, signing material, and gate are private.
type Authority struct {
	config       Config
	reader       client.Reader
	client       client.Writer
	gate         *catalogGate
	publications *publicationStore
	trust        *trustStore
	publisher    *publisher
	credentials  *credentials
	bootstrap    *bootstrap
	accepted     AcceptedMembers
}

// New composes only: no I/O, cryptography, or goroutines. Config is copied.
// Dependencies must provide authoritative reads independently of discovery caches.
func New(cfg Config, deps Dependencies) *Authority {
	cfg = cfg.effective()
	c, reader := deps.Writer, deps.Reader
	a := &Authority{config: cfg, reader: reader, client: c, gate: newCatalogGate(), publications: newPublications(), trust: &trustStore{maxAge: cfg.SnapshotMaxAge}}
	a.publications.maxAge = cfg.SnapshotMaxAge
	a.accepted = make(AcceptedMembers)
	a.publisher = &publisher{Writer: c, APIReader: reader, Config: cfg, Publications: a.publications, Trust: a.trust}
	a.credentials = &credentials{Writer: c, APIReader: reader, Config: cfg, Trust: a.trust, Now: deps.Now}
	a.bootstrap = &bootstrap{Client: c, APIReader: reader, Config: cfg, Issuer: &issuer{APIReader: reader, Config: cfg, Trust: a.trust, CatalogGate: a.gate, Now: deps.Now}}
	a.bootstrap.owner = a
	a.bootstrap.Issuer.owner = a

	return a
}

// Recover validates permanent installation state without granting serving rights.
func (a *Authority) Recover(ctx context.Context, writer client.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := a.config.Validate(); err != nil {
		return err
	}

	if err := a.gate.Acquire(ctx); err != nil {
		return err
	}
	defer a.gate.Release()

	if err := ensureInstalled(ctx, writer, a.reader, a.config); err != nil {
		return fmt.Errorf("ensure Racer installation: %w", err)
	}

	if _, _, err := readVersion(ctx, a.reader, a.config); err != nil {
		return fmt.Errorf("recover Racer installation: %w", err)
	}

	return nil
}

// PublicationHandle is a response-only view. It cannot be installed or advanced.
type PublicationHandle struct {
	image *committedPublication
	owner *Authority
}

func (p *PublicationHandle) Sequence() wire.Sequence {
	if p == nil || p.image == nil {
		return 0
	}

	return p.image.record.Sequence
}

func (p *PublicationHandle) ForBase(hash string) Response {
	if p == nil || p.image == nil || p.owner == nil {
		return Response{}
	}

	return Response{response: p.image.ForBase(hash), owner: p.owner, image: p.image}
}

func (p *PublicationHandle) WriteContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if p == nil || p.owner == nil || p.image == nil {
		return nil, nil, wire.Unavailable
	}

	guard, cancel, err := p.image.writeContext(ctx)
	if err != nil {
		return nil, nil, err
	}

	return responseGuard{Context: guard, owner: p.owner, image: p.image}, cancel, nil
}

// WriteContextWithTrust keeps both synchronous guards visible through a caller's
// bounded write window. A plain context child would hide synchronous revocation.
func (p *PublicationHandle) WriteContextWithTrust(window, trust context.Context) (context.Context, context.CancelFunc, error) {
	guard, ok := trust.(responseGuard)
	if !ok || p == nil || guard.owner == nil || guard.owner != p.owner || !guard.trust {
		return nil, nil, wire.Forbidden
	}

	return p.WriteContext(authorityWriteContext{Context: window, authority: trust, parent: trust})
}

func (a *Authority) TrustReady() error               { _, err := a.trust.pool(); return err }
func (a *Authority) PublicationReady() error         { _, err := a.publications.Current(); return err }
func (a *Authority) BindProcess(ctx context.Context) { a.publications.bindProcess(ctx) }

func (a *Authority) Current() (*PublicationHandle, error) {
	p, err := a.publications.Current()
	if err != nil {
		return nil, err
	}

	return &PublicationHandle{image: p, owner: a}, nil
}

func (a *Authority) CurrentAndSubscribe() (*PublicationHandle, <-chan struct{}, error) {
	p, changed, err := a.publications.CurrentAndSubscribe()
	if err != nil {
		return nil, changed, err
	}

	return &PublicationHandle{image: p, owner: a}, changed, nil
}

func (a *Authority) Wait(ctx context.Context, identity NodeIdentity, after *wire.Sequence) (*PublicationHandle, error) {
	if identity.owner != a || a == nil {
		return nil, wire.Unauthenticated
	}

	p, err := a.publications.Wait(ctx, identity, after)
	if err != nil || p == nil {
		return nil, err
	}

	return &PublicationHandle{image: p, owner: a}, nil
}

func (a *Authority) TrustContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	a.trust.mu.RLock()
	defer a.trust.mu.RUnlock()

	guard, cancel, err := a.trust.writeContextLocked(ctx)
	if err != nil {
		return nil, nil, err
	}

	write, ok := guard.(authorityWriteContext)
	if !ok {
		cancel()
		return nil, nil, wire.Unavailable
	}

	epoch := write.authority

	return responseGuard{Context: guard, owner: a, trust: true, epoch: epoch, bundle: a.trust.bundle}, cancel, nil
}

// TrustPool returns an independent pool: TLS callers cannot mutate local trust.
func (a *Authority) TrustPool() (*x509.CertPool, error) {
	p, err := a.trust.pool()
	if err != nil {
		return nil, err
	}

	return p.Clone(), nil
}

func (a *Authority) AuthenticateCertificate(ctx context.Context, state *tls.ConnectionState) (NodeIdentity, error) {
	identity, err := authenticateCertificate(ctx, a.trust, a.config, state)
	if err != nil {
		return NodeIdentity{}, err
	}

	identity.owner = a

	return identity, nil
}

func (a *Authority) Authenticate(ctx context.Context, request *http.Request) (NodeIdentity, error) {
	return a.bootstrap.Authenticate(ctx, request)
}

func (a *Authority) Issue(ctx context.Context, identity NodeIdentity, request wire.BootstrapRequest) ([]byte, error) {
	if identity.owner != a || a == nil || !identity.bearer {
		return nil, wire.Unauthenticated
	}

	return a.bootstrap.Issuer.Issue(ctx, identity, request)
}

func (a *Authority) Enroll(ctx context.Context, request *http.Request, body wire.BootstrapRequest) ([]byte, error) {
	return a.bootstrap.Enroll(ctx, request, body)
}

func (a *Authority) EnrollWithHint(ctx context.Context, request *http.Request, body wire.BootstrapRequest) ([]byte, EnrollmentHint, error) {
	return a.bootstrap.enroll(ctx, request, body)
}

// KeyringHandle is a comparable opaque view, suitable for detecting replacement
// across authentication without exposing the accepted encoding or install API.
type KeyringHandle struct {
	image *acceptedKeyring
	owner *Authority
	epoch context.Context
}

func (k KeyringHandle) Generation() wire.Generation {
	if k.image == nil {
		return 0
	}

	return k.image.generation
}

func (k KeyringHandle) Response() Response {
	if k.image == nil || k.owner == nil {
		return Response{}
	}

	return Response{response: publicationResponse{encoded: k.image.encoded}, owner: k.owner, trust: true, epoch: k.epoch, bundle: k.image}
}

// Response exposes bounded writing, never an installation proof or mutable bytes.
type Response struct {
	response publicationResponse
	owner    *Authority
	image    *committedPublication
	trust    bool
	epoch    context.Context
	bundle   *acceptedKeyring
}

type responseGuard struct {
	context.Context
	owner  *Authority
	image  *committedPublication
	trust  bool
	epoch  context.Context
	bundle *acceptedKeyring
}

func (Response) String() string   { return "<redacted authority response>" }
func (Response) GoString() string { return "<redacted authority response>" }

func (r Response) WriteTo(ctx context.Context, w io.Writer) (int64, error) {
	guard, ok := ctx.(responseGuard)
	if !ok || r.owner == nil || guard.owner != r.owner || r.trust && (!guard.trust || r.epoch == nil || r.epoch != guard.epoch || r.bundle == nil || r.bundle != guard.bundle) || r.image != nil && guard.image != r.image {
		return 0, wire.Forbidden
	}

	return r.response.writeTo(ctx, w)
}

func (a *Authority) WaitKeyring(ctx context.Context, after *wire.Generation) (*KeyringHandle, error) {
	k, err := a.trust.waitKeyring(ctx, after)
	if err != nil || k == nil {
		return nil, err
	}

	a.trust.mu.RLock()
	defer a.trust.mu.RUnlock()

	if k != a.trust.bundle {
		return nil, wire.Unavailable
	}

	return &KeyringHandle{image: k, owner: a, epoch: a.trust.authority}, nil
}

func (a *Authority) Keyring() (KeyringHandle, error) {
	a.trust.mu.RLock()
	defer a.trust.mu.RUnlock()

	if a.trust.bundle == nil || a.trust.authority == nil || a.trust.authority.Err() != nil || time.Since(a.trust.confirmed) >= a.trust.maxAge {
		return KeyringHandle{}, wire.Unavailable
	}

	return KeyringHandle{image: a.trust.bundle, owner: a, epoch: a.trust.authority}, nil
}

// Config is copied by New. It contains authority policy, not listener, manager,
// filesystem, network replication, or HTTP admission settings.
type Config struct {
	Cluster                   wire.ClusterID
	Namespace                 string
	DataplaneServiceAccount   string
	ControllerServiceAccount  string
	DaemonSetName             string
	CredentialsSecretName     string
	VersionConfigMapName      string
	InstallationConfigMapName string
	Rotation                  RotationPolicy
	CertificateLifetime       time.Duration
	SnapshotMaxAge            time.Duration
	MaxTokenBytes             int
}

// Dependencies are captured at construction. Reader must bypass informer caches.
// Writer needs only ordinary Kubernetes writes, including TokenReview creation.
type Dependencies struct {
	Reader client.Reader
	Writer client.Writer
	Now    func() time.Time
}

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
	if !wire.ValidUUID(string(c.Cluster)) || len(validation.IsDNS1123Label(c.Namespace)) != 0 || c.SnapshotMaxAge < time.Second {
		return wire.InvalidRequest
	}

	for _, name := range []string{c.VersionConfigMapName, c.InstallationConfigMapName, c.DaemonSetName, c.CredentialsSecretName, c.DataplaneServiceAccount} {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return fmt.Errorf("resource name: %w", wire.InvalidRequest)
		}
	}

	if c.VersionConfigMapName == c.InstallationConfigMapName {
		return wire.InvalidRequest
	}

	lifetime := c.CertificateLifetime
	if lifetime < 2*time.Minute || lifetime > wire.CertificateLifetime || lifetime%time.Second != 0 {
		return wire.InvalidRequest
	}

	if c.Rotation.PrepareFor <= 0 || c.Rotation.Interval < c.Rotation.PrepareFor || c.Rotation.RetainFor < lifetime || c.Rotation.Interval > 365*24*time.Hour || c.Rotation.RetainFor > 365*24*time.Hour {
		return wire.InvalidRequest
	}

	return nil
}

// Private engines freeze once for whitebox fixtures. New owns their inputs.
type frozenConfig struct {
	once  sync.Once
	value Config
}

func (f *frozenConfig) get(input *Config) Config {
	f.once.Do(func() { f.value = input.effective() })
	return f.value
}

type requestWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w requestWriter) Write(b []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}

	n, err := w.writer.Write(b)
	if err == nil {
		err = w.ctx.Err()
	}

	return n, err
}

const ReplicationAudience = "racer-controller-replication"

func (a *Authority) Observe(ctx context.Context) error {
	if err := a.gate.Acquire(ctx); err != nil {
		return err
	}
	defer a.gate.Release()

	state, err := loadSigning(ctx, a.reader, a.config, time.Now())
	if err == nil {
		err = a.trust.install(ctx, state.roots, state.bundle)
	}

	if err == nil {
		_, record, readErr := readVersion(ctx, a.reader, a.config)

		err = readErr
		if err == nil {
			err = a.publications.confirm(record)
		}
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	if shouldInvalidateTrust(err) {
		a.trust.invalidate()
		a.publications.Suspend()
	}

	return err
}

func (a *Authority) AcceptReplica(ctx, process context.Context, image wire.Publication) error {
	encoded, err := wire.EncodePublication(image)
	if err != nil {
		return err
	}

	content, membership, err := wire.ContentHashes(image)
	if err != nil {
		return err
	}

	want := versionRecord{Cluster: image.Cluster, Sequence: image.Sequence, MembershipVersion: image.MembershipVersion, ContentHash: content, MembershipHash: membership}

	if err := a.gate.Acquire(ctx); err != nil {
		return err
	}
	defer a.gate.Release()

	_, record, err := readVersion(ctx, a.reader, a.config)
	if err != nil {
		if shouldInvalidateTrust(err) {
			a.publications.Suspend()
			a.trust.invalidate()
		}

		return err
	}

	if err := a.publications.confirm(record); err != nil {
		a.publications.Suspend()
		return err
	}

	if record != want {
		return wire.Unavailable
	}

	return a.publications.Install(&committedPublication{owner: a.publications, record: record, encoded: string(encoded), leadership: process})
}

type ReplicaIdentity struct {
	owner   *Authority
	uid     string
	expires time.Time
}

func (i ReplicaIdentity) UID() string        { return i.uid }
func (i ReplicaIdentity) Expires() time.Time { return i.expires }

func (a *Authority) AuthenticateReplica(ctx context.Context, request *http.Request) (ReplicaIdentity, error) {
	status, token, err := reviewBearer(ctx, a.client, request, ReplicationAudience, 0)
	if err != nil {
		return ReplicaIdentity{}, err
	}

	if status.User.Username != "system:serviceaccount:"+a.config.Namespace+":"+a.config.ControllerServiceAccount {
		return ReplicaIdentity{}, wire.Forbidden
	}

	name, uid := singleExtra(status.User, "pod-name"), singleExtra(status.User, "pod-uid")
	if name == "" || uid == "" || status.User.UID == "" {
		return ReplicaIdentity{}, wire.Unauthenticated
	}

	var pod corev1.Pod
	if err := a.reader.Get(ctx, client.ObjectKey{Namespace: a.config.Namespace, Name: name}, &pod); err != nil {
		return ReplicaIdentity{}, authorizationError(err)
	}

	if !controllerPod(a.config, &pod) || string(pod.UID) != uid {
		return ReplicaIdentity{}, wire.Forbidden
	}

	var sa corev1.ServiceAccount
	if err := a.reader.Get(ctx, client.ObjectKey{Namespace: a.config.Namespace, Name: a.config.ControllerServiceAccount}, &sa); err != nil {
		return ReplicaIdentity{}, authorizationError(err)
	}

	if string(sa.UID) != status.User.UID || sa.DeletionTimestamp != nil {
		return ReplicaIdentity{}, wire.Forbidden
	}

	expires, err := tokenExpiration(token)
	if err != nil {
		return ReplicaIdentity{}, err
	}

	return ReplicaIdentity{owner: a, uid: uid, expires: expires}, nil
}

func controllerPod(cfg Config, pod *corev1.Pod) bool {
	return pod.Namespace == cfg.Namespace && pod.UID != "" && pod.DeletionTimestamp == nil && pod.Spec.ServiceAccountName == cfg.ControllerServiceAccount && pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded
}

type publisher struct {
	settings frozenConfig
	client.Writer
	APIReader    client.Reader
	Config       Config
	Publications *publicationStore
	Trust        *trustStore
}

func (r *publisher) runtimeConfig() Config { return r.settings.get(&r.Config) }
func (r *publisher) suspendInvalidAuthority(err error) {
	if shouldInvalidateTrust(err) {
		r.Publications.Suspend()
		r.Trust.invalidate()
	}
}

type (
	AcceptedMembers = members.History
	TopologyHints   struct {
		Nodes   corev1.NodeList
		Members members.History
	}
	TopologyObservation struct {
		Nodes   corev1.NodeList
		Input   members.Input
		Catalog []wire.CacheDefinition
	}
)

// PublishTopology performs discovery under private admission, then durable CAS and
// installation. Only this successful operation advances publisher history.
func (a *Authority) PublishTopology(ctx context.Context, observe func(context.Context) (TopologyObservation, error)) (TopologyHints, error) {
	r := a.publisher
	cfg := r.runtimeConfig()

	if err := a.gate.Acquire(ctx); err != nil {
		return TopologyHints{}, err
	}
	defer a.gate.Release()

	if err := ctx.Err(); err != nil {
		return TopologyHints{}, err
	}

	cm, previous, err := readVersion(ctx, r.APIReader, cfg)
	if err != nil {
		r.suspendInvalidAuthority(err)
		return TopologyHints{}, err
	}

	if err := r.Publications.confirm(previous); err != nil {
		r.suspendInvalidAuthority(err)
		return TopologyHints{}, err
	}

	if observe == nil {
		return TopologyHints{}, wire.InvalidRequest
	}

	observation, err := observe(ctx)
	if err != nil {
		return TopologyHints{}, err
	}

	catalog := observation.Catalog

	if claim := cm.Annotations[credentialClaim]; claim != "" {
		credentials, err := readBoundCredentials(ctx, r.APIReader, cfg, claim, cm)
		if err != nil {
			r.suspendInvalidAuthority(err)
			return TopologyHints{}, err
		}

		if r.Trust != nil {
			if err := r.Trust.checkReplay(credentials.bundle); err != nil {
				r.suspendInvalidAuthority(err)
				return TopologyHints{}, err
			}
		}

		keyed := keyedCaches(credentials.bundle)

		accepted := make([]wire.CacheDefinition, 0, len(catalog))
		for _, cache := range catalog {
			if keyed[cache.ID] {
				accepted = append(accepted, cache)
			}
		}

		catalog = accepted
	} else {
		catalog = nil
	}

	result, err := members.Reconcile(observation.Input, a.accepted)
	if err != nil {
		return TopologyHints{}, err
	}

	for _, d := range result.Diagnostics {
		ctrl.LoggerFrom(ctx).Info("membership input rejected", "object", d.Object, "field", d.Field, "reason", d.Reason)
	}

	prepared, err := r.Publications.Prepare(previous, cm.ResourceVersion, result.Members, catalog)
	if err != nil {
		return TopologyHints{}, err
	}

	committed, err := r.CommitVersion(ctx, prepared)
	if err != nil {
		return TopologyHints{}, err
	}

	if err := ctx.Err(); err != nil {
		return TopologyHints{}, err
	}

	if err := r.Publications.Install(committed); err != nil {
		return TopologyHints{}, err
	}

	a.accepted = result.Members

	return TopologyHints{Nodes: *observation.Nodes.DeepCopy(), Members: cloneAccepted(result.Members)}, nil
}

func cloneAccepted(accepted AcceptedMembers) AcceptedMembers {
	copy := make(AcceptedMembers, len(accepted))
	for id, member := range accepted {
		member.RDMANICs = slices.Clone(member.RDMANICs)
		for i := range member.RDMANICs {
			if numa := member.RDMANICs[i].NUMANode; numa != nil {
				copy := *numa
				member.RDMANICs[i].NUMANode = &copy
			}
		}

		copy[id] = member
	}

	return copy
}

type (
	DataplaneWorkloadIdentities struct {
		namespace string
		workloads [2]workloadIdentity
	}
	workloadIdentity struct {
		name string
		uid  types.UID
	}
)

func (ids DataplaneWorkloadIdentities) observed() members.WorkloadIdentities {
	observed := members.WorkloadIdentities{Namespace: ids.namespace}
	for i, workload := range ids.workloads {
		observed.Workloads[i] = members.WorkloadIdentity{Name: workload.name, UID: workload.uid}
	}

	return observed
}

func (ids DataplaneWorkloadIdentities) Owns(pod *corev1.Pod) bool { return ids.observed().Owns(pod) }

func managedWorkloadNames(cfg Config) []string { return members.ManagedNames(cfg.DaemonSetName) }

func readManagedWorkloadIdentities(ctx context.Context, reader client.Reader, cfg Config) (DataplaneWorkloadIdentities, error) {
	ids := DataplaneWorkloadIdentities{namespace: cfg.Namespace}
	for i, name := range managedWorkloadNames(cfg) {
		ids.workloads[i].name = name

		var ds appsv1.DaemonSet
		if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: name}, &ds); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return DataplaneWorkloadIdentities{}, err
		}

		if ds.DeletionTimestamp == nil {
			ids.workloads[i].uid = ds.UID
		}
	}

	return ids, nil
}

const (
	enrolledSharesAnnotation   = members.EnrolledSharesAnnotation
	enrolledRDMANICsAnnotation = members.EnrolledRDMANICsAnnotation
	admittedMemberAnnotation   = members.AdmittedMemberAnnotation
)
