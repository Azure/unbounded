// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func (p *PublicationHandle) ForBase(sequence wire.Sequence, hash string) Response {
	if p == nil || p.image == nil || p.owner == nil {
		return Response{}
	}

	return Response{response: p.image.ForBase(sequence, hash), owner: p.owner, image: p.image}
}

func (p *PublicationHandle) Admit(ctx context.Context) (*Admission, context.CancelFunc, error) {
	if p == nil || p.owner == nil || p.image == nil {
		return nil, nil, wire.Unavailable
	}

	guard, cancel, err := p.image.admit(ctx)
	if err != nil {
		return nil, nil, err
	}

	guard.owner = p.owner

	return guard, cancel, nil
}

// AdmitWithTrust binds publication delivery to the prior trust admission.
func (p *PublicationHandle) AdmitWithTrust(window context.Context, trust *Admission) (*Admission, context.CancelFunc, error) {
	if trust == nil || p == nil || trust.owner == nil || trust.owner != p.owner || !trust.trust {
		return nil, nil, wire.Forbidden
	}

	if err := trust.Check(window); err != nil {
		return nil, nil, err
	}

	deadline, _ := trust.Context().Deadline()
	window, stopWindow := context.WithDeadline(window, deadline)

	guard, cancel, err := p.Admit(window)
	if err != nil {
		stopWindow()
		return nil, nil, err
	}

	guard.parent = trust
	stop := context.AfterFunc(trust.Context(), cancel)

	return guard, func() { stop(); cancel(); stopWindow() }, nil
}

func (a *Authority) TrustReady() error       { _, err := a.trust.pool(); return err }
func (a *Authority) PublicationReady() error { _, err := a.publications.Current(); return err }
func (a *Authority) BindProcess(ctx context.Context) {
	a.publications.bindProcess(ctx)
	a.trust.mu.Lock()
	defer a.trust.mu.Unlock()

	a.trust.process = ctx
}

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

func (a *Authority) AdmitTrust(ctx context.Context) (*Admission, context.CancelFunc, error) {
	a.trust.mu.RLock()
	defer a.trust.mu.RUnlock()

	guard, cancel, err := a.trust.admitLocked(ctx)
	if err != nil {
		return nil, nil, err
	}

	guard.owner = a

	return guard, cancel, nil
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

// Admission is an opaque serving capability, separate from request cancellation.
// Context returns an ordinary context for transport cleanup; Check synchronously
// checks revocation and the original deadline before and after writes and flushes.
type Admission struct {
	ctx     context.Context
	owner   *Authority
	image   *committedPublication
	trust   bool
	epoch   context.Context
	bundle  *acceptedKeyring
	parent  *Admission
	process context.Context
}

func (g *Admission) Context() context.Context { return g.ctx }

func (g *Admission) Check(ctx context.Context) error {
	if g == nil || g.ctx == nil || g.epoch == nil {
		return wire.Forbidden
	}

	if err := g.epoch.Err(); err != nil {
		return err
	}

	if g.process != nil {
		if err := g.process.Err(); err != nil {
			return err
		}
	}

	if deadline, ok := g.ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}

	if g.parent != nil {
		if err := g.parent.Check(ctx); err != nil {
			return err
		}
	}

	if err := g.ctx.Err(); err != nil {
		return err
	}

	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}

	return ctx.Err()
}

func newAdmission(parent, epoch context.Context, expiry time.Time) (*Admission, context.CancelFunc) {
	ctx, cancel := context.WithDeadline(parent, expiry)
	stop := context.AfterFunc(epoch, cancel)

	return &Admission{ctx: ctx, epoch: epoch}, func() { stop(); cancel() }
}

func (Response) String() string   { return "<redacted authority response>" }
func (Response) GoString() string { return "<redacted authority response>" }

func (r Response) WriteTo(ctx context.Context, guard *Admission, w io.Writer) (int64, error) {
	if guard == nil || r.owner == nil || guard.owner != r.owner || r.trust && (!guard.trust || r.epoch == nil || r.epoch != guard.epoch || r.bundle == nil || r.bundle != guard.bundle) || r.image != nil && guard.image != r.image {
		return 0, wire.Forbidden
	}

	if err := guard.Check(ctx); err != nil {
		return 0, err
	}

	n, err := r.response.writeTo(ctx, admissionWriter{ctx: ctx, guard: guard, writer: w})
	if err == nil {
		err = guard.Check(ctx)
	}

	return n, err
}

type admissionWriter struct {
	ctx    context.Context
	guard  *Admission
	writer io.Writer
}

func (w admissionWriter) Write(b []byte) (int, error) {
	if err := w.guard.Check(w.ctx); err != nil {
		return 0, err
	}

	n, err := w.writer.Write(b)
	if err == nil {
		err = w.guard.Check(w.ctx)
	}

	return n, err
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
	return members.ControllerPod(pod, cfg.Namespace, cfg.ControllerServiceAccount)
}

type publisher struct {
	client.Writer
	APIReader    client.Reader
	Config       Config
	Publications *publicationStore
	Trust        *trustStore
}

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
	cfg := r.Config

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
		return TopologyHints{}, err
	}

	if observe == nil {
		return TopologyHints{}, wire.InvalidRequest
	}

	observation, err := observe(ctx)
	if err != nil {
		return TopologyHints{}, err
	}

	catalog, err := r.keyedCatalog(ctx, cm, observation.Catalog)
	if err != nil {
		return TopologyHints{}, err
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

func (r *publisher) keyedCatalog(ctx context.Context, version *corev1.ConfigMap, catalog []wire.CacheDefinition) ([]wire.CacheDefinition, error) {
	claim := version.Annotations[credentialClaim]
	if claim == "" {
		return nil, nil
	}

	credentials, err := readBoundCredentials(ctx, r.APIReader, r.Config, claim, version)
	if err == nil {
		err = r.Trust.validateReplay(credentials.bundle)
	}

	if err != nil {
		r.suspendInvalidAuthority(err)
		return nil, err
	}

	keyed := keyedCaches(credentials.bundle)

	accepted := make([]wire.CacheDefinition, 0, len(catalog))
	for _, cache := range catalog {
		if keyed[cache.ID] {
			accepted = append(accepted, cache)
		}
	}

	return accepted, nil
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

const (
	enrolledSharesAnnotation   = members.EnrolledSharesAnnotation
	enrolledRDMANICsAnnotation = members.EnrolledRDMANICsAnnotation
	admittedMemberAnnotation   = members.AdmittedMemberAnnotation
)

// NodeIdentity is verified output, never populated from an untrusted request.
type NodeIdentity struct {
	owner    *Authority
	bearer   bool
	nodeName string
	cluster  wire.ClusterID
	node     wire.NodeID
	expires  time.Time
}

func (i NodeIdentity) Node() wire.NodeID       { return i.node }
func (i NodeIdentity) Cluster() wire.ClusterID { return i.cluster }
func (i NodeIdentity) Expires() time.Time      { return i.expires }

// issuer accesses a controller-only Secret. Its private key is never projected
// into dataplane Pods or included in a response or diagnostic.
type issuer struct {
	APIReader   client.Reader
	Config      Config
	Trust       *trustStore
	CatalogGate *catalogGate
	Now         func() time.Time
}

type signingMaterial struct {
	Certificate []byte `json:"certificate"`
	PrivateKey  []byte `json:"private_key"`
}

type issuerMaterial struct {
	Keys map[string]signingMaterial `json:"keys"`
}

func (signingMaterial) String() string   { return "<redacted issuer>" }
func (signingMaterial) GoString() string { return "<redacted issuer>" }
func (issuerMaterial) String() string    { return "<redacted issuers>" }
func (issuerMaterial) GoString() string  { return "<redacted issuers>" }

func serialNumber() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	return n.Add(n, big.NewInt(1)), nil
}

// Backdate validity starts for modest clock skew without extending expiration.
const certificateClockSkew = time.Minute

func generateIssuer(now time.Time, cfg Config) ([]byte, []byte, error) {
	cfg = cfg.effective()

	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
	}

	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Racer " + string(cfg.Cluster)}, NotBefore: now.Add(-certificateClockSkew), NotAfter: now.Add(cfg.Rotation.Interval + cfg.Rotation.PrepareFor + cfg.Rotation.RetainFor + 2*cfg.CertificateLifetime), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}

	cert, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		return nil, nil, err
	}

	if len(cert) > reservedRootBytes {
		return nil, nil, wire.TooLarge
	}

	encoded, err := x509.MarshalPKCS8PrivateKey(key)

	return cert, encoded, err
}

func parseSigning(m signingMaterial) (*x509.Certificate, ed25519.PrivateKey, error) {
	cert, err := x509.ParseCertificate(m.Certificate)
	if err != nil {
		return nil, nil, wire.Unavailable
	}

	private, err := x509.ParsePKCS8PrivateKey(m.PrivateKey)
	if err != nil {
		return nil, nil, wire.Unavailable
	}

	key, ok := private.(ed25519.PrivateKey)

	pub, publicOK := cert.PublicKey.(ed25519.PublicKey)
	if !ok || !publicOK || !pub.Equal(key.Public()) || !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 || cert.CheckSignatureFrom(cert) != nil {
		return nil, nil, wire.Unavailable
	}

	return cert, key, nil
}

type parsedSigning struct {
	certificate *x509.Certificate
	key         ed25519.PrivateKey
}

type signingState struct {
	certificate *x509.Certificate
	key         ed25519.PrivateKey
	roots       *x509.CertPool
	bundle      wire.KeyringBundle
}

func loadSigning(ctx context.Context, reader client.Reader, cfg Config, now time.Time) (signingState, error) {
	cfg = cfg.effective()

	if err := ctx.Err(); err != nil {
		return signingState{}, err
	}

	if err := cfg.Validate(); err != nil {
		return signingState{}, err
	}

	version, _, err := readVersion(ctx, reader, cfg)
	if err != nil {
		return signingState{}, err
	}

	claim := version.Annotations[credentialClaim]
	if !validCredentialClaim(cfg, claim) {
		return signingState{}, wire.Unavailable
	}

	credentials, err := readBoundCredentials(ctx, reader, cfg, claim, version)
	if err != nil {
		return signingState{}, err
	}

	active := credentials.signing[credentials.rotation.ActiveIssuer]

	cert, key := active.certificate, active.key
	if now.Before(cert.NotBefore) || now.Add(cfg.CertificateLifetime).After(cert.NotAfter) {
		return signingState{}, wire.Unavailable
	}

	roots := x509.NewCertPool()

	for _, der := range credentials.bundle.PeerTrustRoots {
		root := credentials.signing[rootID(der)].certificate
		if !now.Before(root.NotBefore) && now.Before(root.NotAfter) {
			roots.AddCert(root)
		}
	}

	if err := ctx.Err(); err != nil {
		return signingState{}, err
	}

	return signingState{certificate: cert, key: key, roots: roots, bundle: credentials.bundle}, nil
}

func (i *issuer) now() time.Time {
	if i.Now != nil {
		return i.Now().UTC().Truncate(time.Second)
	}

	return time.Now().UTC().Truncate(time.Second)
}

// Issuance can also observe invalid durable authority. It may withdraw trust,
// but only controller reconciliation can install or restore serving trust.
func (i *issuer) loadSigning(ctx context.Context, now time.Time) (signingState, error) {
	// Serialize observations with controller installation so an in-flight valid
	// read cannot restore trust after another operation observes invalidity.
	if err := i.CatalogGate.Acquire(ctx); err != nil {
		return signingState{}, err
	}
	defer i.CatalogGate.Release()

	state, err := loadSigning(ctx, i.APIReader, i.Config, now)
	if err == nil {
		err = i.Trust.validateReplay(state.bundle)
	}

	// Issuance only reads authority. A request ending supplies no invalid evidence
	// and cannot make a write ambiguous. Inspect the returned error, not ctx.Err:
	// validation failures observed alongside cancellation must still revoke trust.
	if shouldInvalidateTrust(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		i.Trust.invalidate()
	}

	return state, err
}

// Issue accepts only the identity returned by token authentication. CSR names,
// extensions and requested usages are discarded. Enrollment is correlation only.
// It returns an owned, validated JSON response within the bootstrap wire bound.
func (i *issuer) Issue(ctx context.Context, identity NodeIdentity, request wire.BootstrapRequest) ([]byte, error) {
	cfg := i.Config

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := i.now()
	if identity.cluster != cfg.Cluster || !wire.ValidUUID(string(identity.node)) || !identity.expires.After(now) {
		return nil, wire.Forbidden
	}

	if request.Cluster != identity.cluster {
		return nil, wire.Forbidden
	}

	if err := wire.ValidateBootstrapRequest(request); err != nil {
		return nil, err
	}

	pub, err := bootstrapPublicKey(request.CSRDER)
	if err != nil {
		return nil, err
	}

	state, err := i.loadSigning(ctx, now)
	if err != nil {
		return nil, err
	}

	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}

	uri := &url.URL{Scheme: "spiffe", Host: string(identity.cluster), Path: "/node/" + string(identity.node)}
	template := &x509.Certificate{SerialNumber: serial, NotBefore: now.Add(-certificateClockSkew), NotAfter: now.Add(cfg.CertificateLifetime), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{uri}}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	leaf, err := x509.CreateCertificate(rand.Reader, template, state.certificate, pub, state.key)
	if err != nil {
		return nil, wire.Unavailable
	}

	response := wire.BootstrapResponse{SchemaVersion: wire.SchemaVersion, Cluster: identity.cluster, Node: identity.node, Enrollment: request.Enrollment, CertificateChain: [][]byte{leaf, state.certificate.Raw}}

	encoded, err := wire.EncodeBootstrap(response)
	if err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return encoded, nil
}

func bootstrapPublicKey(der []byte) (ed25519.PublicKey, error) {
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil || csr.CheckSignature() != nil {
		return nil, wire.InvalidRequest
	}

	pub, ok := csr.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, wire.InvalidRequest
	}

	return pub, nil
}

// authenticateCertificate requires a verified chain, the client-auth usage,
// cluster-scoped Node URI SAN, and current validity against local trust. Recheck on
// every poll: an existing TLS connection must not bypass certificate expiry.
// Membership and Kubernetes workload state are not certificate authorization.
func authenticateCertificate(ctx context.Context, trust *trustStore, cfg Config, state *tls.ConnectionState) (NodeIdentity, error) {
	if err := ctx.Err(); err != nil {
		return NodeIdentity{}, err
	}

	if state == nil || !state.HandshakeComplete || len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		return NodeIdentity{}, wire.Unauthenticated
	}

	now := time.Now()
	leaf := state.PeerCertificates[0]

	node, err := certificateNode(leaf, cfg.Cluster, now)
	if err != nil {
		return NodeIdentity{}, err
	}

	roots, err := trust.pool()
	if err != nil {
		return NodeIdentity{}, wire.Unavailable
	}

	intermediates := x509.NewCertPool()
	for _, cert := range state.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}

	chains, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		return NodeIdentity{}, wire.Unauthenticated
	}

	expires := leaf.NotAfter
	for _, cert := range chains[0] {
		if cert.NotAfter.Before(expires) {
			expires = cert.NotAfter
		}
	}

	if err := ctx.Err(); err != nil {
		return NodeIdentity{}, err
	}

	if !time.Now().Before(expires) {
		return NodeIdentity{}, wire.Unauthenticated
	}

	return NodeIdentity{cluster: cfg.Cluster, node: node, expires: expires}, nil
}

func certificateNode(leaf *x509.Certificate, cluster wire.ClusterID, now time.Time) (wire.NodeID, error) {
	if !validClientCertificate(leaf, now) || len(leaf.URIs) != 1 {
		return "", wire.Unauthenticated
	}

	uri := leaf.URIs[0]

	node := wire.NodeID(strings.TrimPrefix(uri.Path, "/node/"))
	if !wire.ValidUUID(string(node)) || uri.String() != "spiffe://"+uri.Host+"/node/"+string(node) {
		return "", wire.Unauthenticated
	}

	if uri.Host != string(cluster) {
		return "", wire.Forbidden
	}

	return node, nil
}

func validClientCertificate(leaf *x509.Certificate, now time.Time) bool {
	_, ed25519Key := leaf.PublicKey.(ed25519.PublicKey)

	return ed25519Key && !leaf.IsCA && leaf.KeyUsage == x509.KeyUsageDigitalSignature &&
		len(leaf.ExtKeyUsage) == 1 && leaf.ExtKeyUsage[0] == x509.ExtKeyUsageClientAuth &&
		len(leaf.UnknownExtKeyUsage) == 0 && !now.Before(leaf.NotBefore) && now.Before(leaf.NotAfter)
}

type bootstrap struct {
	owner     *Authority
	Client    client.Writer
	APIReader client.Reader
	Config    Config
	Issuer    *issuer
}

// Authenticate performs TokenReview for racer-control, checks the live bound Pod
// UID and authorized ServiceAccount/workload, and resolves its assigned Node UID.
// Token contents, CSR contents, and requested names are not authority on their own.
func (b *bootstrap) Authenticate(ctx context.Context, r *http.Request) (NodeIdentity, error) {
	cfg := b.Config

	if err := ctx.Err(); err != nil {
		return NodeIdentity{}, err
	}

	if b.Client == nil || b.APIReader == nil {
		return NodeIdentity{}, wire.Unavailable
	}

	status, token, err := reviewBearer(ctx, b.Client, r, wire.TokenAudience, cfg.MaxTokenBytes)
	if err != nil {
		return NodeIdentity{}, err
	}

	if status.User.Username != "system:serviceaccount:"+cfg.Namespace+":"+cfg.DataplaneServiceAccount {
		return NodeIdentity{}, wire.Forbidden
	}
	// TokenReview authenticates the token. Its JWT expiration is used only to
	// shorten authorization, never to establish identity or extend validity.
	expires, err := tokenExpiration(token)
	if err != nil {
		return NodeIdentity{}, err
	}

	node, err := b.authorizeTokenBinding(ctx, cfg, status.User)
	if err != nil {
		return NodeIdentity{}, err
	}

	if err := ctx.Err(); err != nil {
		return NodeIdentity{}, err
	}

	if !time.Now().Before(expires) {
		return NodeIdentity{}, wire.Unauthenticated
	}

	return NodeIdentity{owner: b.owner, bearer: true, cluster: cfg.Cluster, node: wire.NodeID(node.UID), nodeName: node.Name, expires: expires}, nil
}

func (b *bootstrap) authorizeTokenBinding(ctx context.Context, cfg Config, user authv1.UserInfo) (*corev1.Node, error) {
	podName, podUID := singleExtra(user, "pod-name"), singleExtra(user, "pod-uid")
	if podName == "" || podUID == "" || user.UID == "" {
		return nil, wire.Unauthenticated
	}

	var pod corev1.Pod
	if err := b.APIReader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: podName}, &pod); err != nil {
		return nil, authorizationError(err)
	}

	if string(pod.UID) != podUID {
		return nil, wire.Forbidden
	}

	if err := authorizePod(ctx, b.APIReader, cfg, &pod, user.UID); err != nil {
		return nil, err
	}

	var node corev1.Node
	if err := b.APIReader.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, &node); err != nil {
		return nil, authorizationError(err)
	}

	if !authorizedNode(&node) {
		return nil, wire.Forbidden
	}
	// Require unambiguous current node bindings as well as the live Pod binding.
	for key, want := range map[string]string{"node-name": node.Name, "node-uid": string(node.UID)} {
		if singleExtra(user, key) != want {
			return nil, wire.Forbidden
		}
	}

	return &node, nil
}

func singleExtra(user authv1.UserInfo, key string) string {
	values := user.Extra["authentication.kubernetes.io/"+key]
	if len(values) != 1 {
		return ""
	}

	return values[0]
}

// reviewBearer shares only token parsing and API authentication. Callers retain
// their distinct workload, service-account, binding, and expiration policies.
func reviewBearer(ctx context.Context, c client.Writer, r *http.Request, audience string, maxBytes int) (authv1.TokenReviewStatus, string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return authv1.TokenReviewStatus{}, "", wire.Unauthenticated
	}

	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n,") || maxBytes > 0 && len(token) > maxBytes {
		return authv1.TokenReviewStatus{}, "", wire.Unauthenticated
	}

	review := &authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{audience}}}
	if c == nil || c.Create(ctx, review) != nil {
		return authv1.TokenReviewStatus{}, "", wire.Unavailable
	}

	status := review.Status
	if !status.Authenticated || status.Error != "" || !slices.Contains(status.Audiences, audience) {
		return authv1.TokenReviewStatus{}, "", wire.Unauthenticated
	}

	return status, token, nil
}

func tokenExpiration(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, wire.Unauthenticated
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, wire.Unauthenticated
	}

	var claims struct {
		Expiration int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Expiration <= 0 {
		return time.Time{}, wire.Unauthenticated
	}

	expires := time.Unix(claims.Expiration, 0)
	if !time.Now().Before(expires) {
		return time.Time{}, wire.Unauthenticated
	}

	return expires, nil
}

// enroll validates CSR proof of possession and binds the issued identity to the
// token, not caller-provided SANs. Every issuance uses a token, including renewal.
// Retries correlate by enrollment ID; there is no persistent receipt ledger.
// The returned bytes are the issuer's bounded, validated JSON response.
func (b *bootstrap) enroll(ctx context.Context, r *http.Request, request wire.BootstrapRequest) ([]byte, EnrollmentHint, error) {
	identity, err := b.Authenticate(ctx, r)
	if err != nil {
		return nil, EnrollmentHint{}, err
	}

	if b.Issuer == nil {
		return nil, EnrollmentHint{}, wire.Unavailable
	}

	ctx, cancel := context.WithDeadline(ctx, identity.expires)
	defer cancel()

	response, err := b.Issuer.Issue(ctx, identity, request)
	if err != nil {
		return nil, EnrollmentHint{}, err
	}
	// Resolve the same live UID again before persisting an authenticated proposal.
	// Both annotations are proposals only; explicit administrator values win.
	var live corev1.Node
	if err := b.APIReader.Get(ctx, client.ObjectKey{Name: identity.nodeName}, &live); err != nil {
		return nil, EnrollmentHint{}, err
	}

	node := &live
	if wire.NodeID(node.UID) == identity.node {
		if !authorizedNode(node) {
			return nil, EnrollmentHint{}, wire.Forbidden
		}

		response, err = bootstrapBlockDevices(ctx, response, node)
		if err != nil {
			return nil, EnrollmentHint{}, err
		}

		return response, EnrollmentHint{Node: *node, Shares: request.Shares, RDMANICs: wire.CanonicalRDMANICs(request.RDMANICs), Expires: identity.expires}, nil
	}

	return nil, EnrollmentHint{}, wire.Forbidden
}

func bootstrapBlockDevices(ctx context.Context, response []byte, node *corev1.Node) ([]byte, error) {
	pattern := node.Annotations[wire.BlockDevicesAnnotation]
	if pattern == "" {
		return response, nil
	}

	reason := ""
	if len(pattern) > 1024 {
		reason = "exceeds 1024 bytes"
	} else if _, err := regexp.Compile(pattern); err != nil {
		reason = "invalid regular expression"
	}

	if reason != "" {
		ctrl.LoggerFrom(ctx).Info("warning: ignoring block device annotation; using file-backed storage", "node", node.Name, "annotation", wire.BlockDevicesAnnotation, "reason", reason)
		return response, nil
	}

	decoded, err := wire.DecodeBootstrapResponse(bytes.NewReader(response))
	if err != nil {
		return nil, err
	}

	// The dataplane matches /dev/disk/by-id basenames at process startup only.
	decoded.BlockDevices = pattern

	return wire.EncodeBootstrap(decoded)
}

// EnrollmentHint is a detached UID/resource-version-bound proposal, not authority.
// The root adapter persists it with optimistic concurrency after gate release.
type EnrollmentHint struct {
	Node     corev1.Node
	Shares   uint32
	RDMANICs []wire.RDMANIC
	Expires  time.Time
}

func authorizationError(err error) error {
	if apierrors.IsNotFound(err) {
		return wire.Forbidden
	}

	return wire.Unavailable
}

func authorizedNode(node *corev1.Node) bool {
	_, excluded := node.Labels[wire.ExclusionLabel]
	return node.Name != "" && wire.ValidUUID(string(node.UID)) && node.DeletionTimestamp == nil && !excluded
}

// The configured namespace/name designate the managed workload. A Pod must be
// controlled by that exact current DaemonSet UID, not just carry matching labels.
func managedPod(cfg Config, pod *corev1.Pod) bool {
	if pod.Namespace != cfg.Namespace || pod.UID == "" || pod.DeletionTimestamp != nil ||
		pod.Spec.NodeName == "" || pod.Spec.ServiceAccountName != cfg.DataplaneServiceAccount ||
		pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" ||
		owner.Name != cfg.DaemonSetName || owner.UID == "" {
		return false
	}

	return true
}

func authorizePod(ctx context.Context, reader client.Reader, cfg Config, pod *corev1.Pod, serviceAccountUID string) error {
	if !managedPod(cfg, pod) {
		return wire.Forbidden
	}

	ownership, err := members.ReadWorkloadIdentities(ctx, reader, cfg.Namespace, cfg.DaemonSetName)
	if err != nil {
		return authorizationError(err)
	}

	if !ownership.Owns(pod) {
		return wire.Forbidden
	}

	var sa corev1.ServiceAccount
	if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.DataplaneServiceAccount}, &sa); err != nil {
		return authorizationError(err)
	}

	if sa.UID == "" || sa.DeletionTimestamp != nil || serviceAccountUID != "" && string(sa.UID) != serviceAccountUID {
		return wire.Forbidden
	}

	return ctx.Err()
}

// versionRecord is the only persisted topology bookkeeping. Hashes cover
// canonical content excluding counters. No member history or publication bytes
// are stored. ResourceVersion CAS must precede installing a publication.
type versionRecord struct {
	Cluster           wire.ClusterID         `json:"cluster"`
	Sequence          wire.Sequence          `json:"sequence,string"`
	MembershipVersion wire.MembershipVersion `json:"membership_version,string"`
	ContentHash       string                 `json:"content_hash"`
	MembershipHash    string                 `json:"membership_hash"`
}

// preparedPublication owns encoded candidate bytes. Publishers require CAS;
// replicas require canonical validation and an authoritative durable confirmation.
type preparedPublication struct {
	owner           *publicationStore
	previous        versionRecord
	resourceVersion string
	record          versionRecord
	encoded         string
	delta           string
	deltaBase       string
	deltaSequence   wire.Sequence
}

type committedPublication struct {
	owner         *publicationStore
	record        versionRecord
	encoded       string
	delta         string
	deltaBase     string
	deltaSequence wire.Sequence
	leadership    context.Context
	authority     context.Context
}

// publicationResponse is a response-only view, never installable state. The
// caller owns one admitted image context through both write and flush.
type publicationResponse struct{ encoded string }

func (p publicationResponse) writeTo(ctx context.Context, w io.Writer) (int64, error) {
	var written int64

	for remaining := p.encoded; remaining != ""; {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		// ResponseWriter need not implement StringWriter. Limit conversion scratch
		// to 32 KiB rather than allocating a full publication for every response.
		chunk := remaining[:min(len(remaining), 32*1024)]
		n, err := io.WriteString(requestWriter{ctx: ctx, writer: w}, chunk)

		written += int64(n)
		if err != nil {
			return written, err
		}

		if n != len(chunk) {
			return written, io.ErrShortWrite
		}

		remaining = remaining[n:]
	}

	return written, ctx.Err()
}

// admit pins one response to its image's revocable authority and the
// freshness deadline at write admission. Later confirmations cannot extend an
// in-flight response. Suspension revokes it; ordinary advancement does not.
func (p *committedPublication) admit(parent context.Context) (*Admission, context.CancelFunc, error) {
	if p == nil || p.owner == nil {
		return nil, nil, wire.Unavailable
	}

	owner := p.owner
	owner.mu.Lock()
	defer owner.mu.Unlock()

	if _, err := owner.currentLocked(); err != nil {
		return nil, nil, err
	}

	if p != owner.current || p.authority == nil || p.authority.Err() != nil {
		return nil, nil, wire.Unavailable
	}

	guard, cancel := newAdmission(parent, p.authority, owner.confirmed.Add(owner.maxAge))
	guard.image = p

	return guard, cancel, nil
}

// publicationStore owns only the current immutable publication and one broadcast
// notification. Older state belongs to dataplanes; poll admission belongs to Server.
type publicationStore struct {
	mu        sync.Mutex
	current   *committedPublication
	changed   chan struct{}
	suspended bool
	process   context.Context
	confirmed time.Time
	maxAge    time.Duration
	observed  versionRecord
	revoke    context.CancelFunc
	epoch     context.Context
}

func newPublications() *publicationStore {
	return &publicationStore{changed: make(chan struct{}), maxAge: 30 * time.Second}
}

func (p *publicationStore) bindProcess(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.process = ctx
}

func (p *publicationStore) notifyLocked() { close(p.changed); p.changed = make(chan struct{}) }

func (p *publicationStore) Prepare(previous versionRecord, resourceVersion string, members AcceptedMembers, caches []wire.CacheDefinition) (*preparedPublication, error) {
	if !previous.valid() || resourceVersion == "" {
		return nil, wire.InvalidRequest
	}

	v := wire.Publication{SchemaVersion: wire.SchemaVersion, Cluster: previous.Cluster, Caches: caches, Members: make([]wire.Member, 0, len(members))}
	for id, member := range members {
		if id != member.Node {
			return nil, wire.InvalidRequest
		}

		v.Members = append(v.Members, member)
	}

	candidate, err := wire.NewCanonicalCandidate(v)
	if err != nil {
		return nil, err
	}

	content, membership, err := candidate.ContentHashes()
	if err != nil {
		return nil, err
	}

	record := previous
	if content != previous.ContentHash {
		if record.Sequence == ^wire.Sequence(0) {
			return nil, wire.Unavailable
		}

		record.Sequence++
	}

	if membership != previous.MembershipHash {
		if content == previous.ContentHash || record.MembershipVersion == ^wire.MembershipVersion(0) {
			return nil, wire.Unavailable
		}

		record.MembershipVersion++
	}

	record.ContentHash, record.MembershipHash = content, membership

	encoded, err := candidate.EncodePublication(record.Sequence, record.MembershipVersion)
	if err != nil {
		return nil, err
	}

	prepared := &preparedPublication{owner: p, previous: previous, resourceVersion: resourceVersion, record: record, encoded: string(encoded)}
	v.Sequence, v.MembershipVersion = record.Sequence, record.MembershipVersion
	p.prepareDelta(prepared, v)

	return prepared, nil
}

func (p *publicationStore) prepareDelta(prepared *preparedPublication, v wire.Publication) {
	previous, record := prepared.previous, prepared.record
	// Capture immutable base under the lock, then diff/encode entirely outside it.
	if current, err := p.Current(); err == nil && current.record == previous && record.Sequence > previous.Sequence {
		if base, err := wire.DecodePublication(strings.NewReader(current.encoded)); err == nil {
			if delta, err := wire.EncodeDelta(base, v); err == nil && len(delta) < len(prepared.encoded) {
				prepared.delta, prepared.deltaBase = string(delta), previous.ContentHash
				prepared.deltaSequence = previous.Sequence
			}
		}
	}
}

// CommitVersion mints publisher installable state after a resource-version CAS.
// Even unchanged content is CAS-confirmed; its counters and bytes remain identical.
func (r *publisher) CommitVersion(ctx context.Context, p *preparedPublication) (*committedPublication, error) {
	cfg := r.Config

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if p == nil || p.owner != r.Publications || p.previous.Cluster != cfg.Cluster {
		return nil, wire.InvalidRequest
	}

	cm, previous, err := readVersion(ctx, r.APIReader, cfg)
	if err != nil {
		r.suspendInvalidAuthority(err)
		return nil, err
	}

	if err := r.Publications.confirm(previous); err != nil {
		return nil, err
	}

	if cm.ResourceVersion != p.resourceVersion || previous != p.previous {
		return nil, apierrors.NewConflict(corev1.Resource("configmaps"), cm.Name, wire.Conflict)
	}

	cm.Data = versionData(p.record)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := r.Update(ctx, cm); err != nil {
		return nil, err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return &committedPublication{owner: p.owner, record: p.record, encoded: p.encoded, delta: p.delta, deltaBase: p.deltaBase, deltaSequence: p.deltaSequence, leadership: ctx}, nil
}

// ForBase returns a shared bounded delta only for the exact authenticated cursor.
// Coalesced/skipped updates and controller restarts automatically use the full image.
func (p *committedPublication) ForBase(sequence wire.Sequence, hash string) publicationResponse {
	if sequence == 0 || sequence != p.deltaSequence || hash == "" || hash != p.deltaBase || p.delta == "" {
		return publicationResponse{encoded: p.encoded}
	}

	return publicationResponse{encoded: p.delta}
}

func (p *publicationStore) Install(next *committedPublication) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if next == nil || next.owner != p || next.leadership == nil || !next.record.valid() || next.encoded == "" {
		return wire.InvalidRequest
	}

	if err := next.leadership.Err(); err != nil {
		return err
	}

	if err := p.observeLocked(next.record); err != nil {
		return err
	}

	if p.process != nil {
		copy := *next
		copy.leadership = p.process
		next = &copy
	}

	if current := p.current; current != nil {
		// observeLocked is the sole counter/hash high-water guard. Installed
		// state can only lag that observation, never exceed it.
		if next.record.Sequence == current.record.Sequence {
			return p.reconfirmLocked(next)
		}
	}

	p.current = p.authorizeLocked(next)
	p.confirmed = time.Now()
	p.suspended = false
	p.notifyLocked()

	return nil
}

func (p *publicationStore) reconfirmLocked(next *committedPublication) error {
	current := p.current
	if next.record != current.record || next.encoded != current.encoded {
		return wire.Conflict
	}

	p.confirmed = time.Now()
	if current.leadership.Err() != nil || p.suspended {
		p.current = p.authorizeLocked(next)
	}

	if p.suspended {
		p.suspended = false
		p.notifyLocked()
	}

	return nil
}

func (p *publicationStore) authorizeLocked(next *committedPublication) *committedPublication {
	if p.epoch == nil || p.epoch.Err() != nil {
		p.epoch, p.revoke = context.WithCancel(next.leadership)
	}

	copy := *next
	copy.authority = p.epoch

	return &copy
}

// confirm never promotes a hash to an image. It only renews the freshness of an
// already validated image when all durable counters and hashes still match.
func (p *publicationStore) confirm(record versionRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.observeLocked(record); err != nil {
		return err
	}

	if p.current != nil && record == p.current.record && !p.suspended {
		p.confirmed = time.Now()
	}

	return nil
}

// observed is independent of installed bytes, including before the first image.
// Suspension never forgets this high-water mark. Skipped versions may return to
// earlier hashes, but counters and unchanged membership versions must agree.
func (p *publicationStore) observeLocked(record versionRecord) error {
	if !record.valid() {
		p.suspendLocked()
		return wire.Conflict
	}

	if old := p.observed; old.Sequence != 0 {
		rollback := record.Cluster != old.Cluster || record.Sequence < old.Sequence || record.MembershipVersion < old.MembershipVersion
		conflictingReplay := record.Sequence == old.Sequence && record != old
		changedMembership := record.MembershipVersion == old.MembershipVersion && record.MembershipHash != old.MembershipHash
		// Check progression only after ruling out rollback, before subtracting
		// unsigned counters. Each membership change requires a publication change.
		if rollback || conflictingReplay || changedMembership || uint64(record.MembershipVersion-old.MembershipVersion) > uint64(record.Sequence-old.Sequence) {
			p.suspendLocked()
			return wire.Conflict
		}
	}

	p.observed = record

	return nil
}

func (p *publicationStore) Current() (*committedPublication, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.currentLocked()
}

// CurrentAndSubscribe atomically reads the current publication and subscribes to
// changes, including when unavailable. The channel closes on install or suspension;
// leadership cancellation must be observed separately by the caller.
func (p *publicationStore) CurrentAndSubscribe() (*committedPublication, <-chan struct{}, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current, err := p.currentLocked()

	return current, p.changed, err
}

func (p *publicationStore) currentLocked() (*committedPublication, error) {
	if p.current == nil || p.suspended || time.Since(p.confirmed) >= p.maxAge {
		return nil, wire.Unavailable
	}

	if err := p.current.leadership.Err(); err != nil {
		return nil, err
	}

	return p.current, nil
}

// Suspend withdraws readiness and wakes polls after failure to validate durable
// authority. Keep bytes/history for a later successful CAS, never serve them until
// then. Invalid desired inputs alone do not suspend the last valid publication.
func (p *publicationStore) Suspend() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.suspendLocked()
}

func (p *publicationStore) suspendLocked() {
	if p.revoke != nil {
		p.revoke()
	}

	if !p.suspended {
		p.suspended = true
		p.notifyLocked()
	}
}

// Wait rejects invalid identities/cursors and honors context cancellation and
// certificate expiration. It shares publication bytes and broadcast notifications;
// callers own admission for the full response lifetime, including writes and flush.
func (p *publicationStore) Wait(ctx context.Context, identity NodeIdentity, after *wire.Sequence) (*committedPublication, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !wire.ValidUUID(string(identity.node)) || !time.Now().Before(identity.expires) {
		return nil, wire.Unauthenticated
	}

	current, changed, err := p.CurrentAndSubscribe()
	if err != nil {
		return nil, err
	}

	if identity.cluster != current.record.Cluster {
		return nil, wire.Forbidden
	}

	if after != nil && *after == 0 {
		return nil, wire.Conflict
	}

	if after != nil && *after > current.record.Sequence {
		return nil, wire.Unavailable
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !time.Now().Before(identity.expires) {
		return nil, wire.Unauthenticated
	}

	if after == nil || current.record.Sequence > *after {
		if err := current.leadership.Err(); err != nil {
			return nil, err
		}

		return current, nil
	}

	return p.waitForPublication(ctx, identity, *after, current, changed)
}

func (p *publicationStore) waitForPublication(ctx context.Context, identity NodeIdentity, after wire.Sequence, current *committedPublication, changed <-chan struct{}) (*committedPublication, error) {
	timer := time.NewTimer(wire.PollWait)
	defer timer.Stop()

	expiration := time.NewTimer(time.Until(identity.expires))
	defer expiration.Stop()

	freshness := time.NewTicker(min(p.maxAge, time.Second))
	defer freshness.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-current.leadership.Done():
			return nil, current.leadership.Err()
		case <-expiration.C:
			return nil, wire.Unauthenticated
		case <-freshness.C:
			if _, err := p.Current(); err != nil {
				return nil, err
			}

			continue
		case <-timer.C:
			return nil, pollTimeoutError(ctx, current.leadership, identity.expires)
		case <-changed:
		}

		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !time.Now().Before(identity.expires) {
			return nil, wire.Unauthenticated
		}

		var err error

		current, changed, err = p.CurrentAndSubscribe()
		if err != nil {
			return nil, err
		}

		if current.record.Sequence > after {
			return current, nil
		}
	}
}

func pollTimeoutError(ctx, leadership context.Context, expires time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if err := leadership.Err(); err != nil {
		return err
	}

	if !time.Now().Before(expires) {
		return wire.Unauthenticated
	}

	return nil
}
