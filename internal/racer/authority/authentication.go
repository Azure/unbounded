// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
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

// Issuer accesses a controller-only Secret. Its private key is never projected
// into dataplane Pods or included in a response or diagnostic.
type issuer struct {
	owner       *Authority
	settings    frozenConfig
	APIReader   client.Reader
	Config      Config
	Trust       *trustStore
	CatalogGate *catalogGate
	Now         func() time.Time
}

func (i *issuer) runtimeConfig() Config { return i.settings.get(&i.Config) }

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
	if i.CatalogGate != nil {
		// Controller API work can stall. Waiting for its gate must still honor
		// the enrollment deadline and release bounded authentication admission.
		if err := i.CatalogGate.Acquire(ctx); err != nil {
			return signingState{}, err
		}
		defer i.CatalogGate.Release()
	}

	state, err := loadSigning(ctx, i.APIReader, i.runtimeConfig(), now)
	if err == nil && i.Trust != nil {
		err = i.Trust.checkReplay(state.bundle)
	}

	if shouldInvalidateTrust(err) {
		i.Trust.invalidate()
	}

	return state, err
}

// Issue accepts only the identity returned by token authentication. CSR names,
// extensions and requested usages are discarded. Enrollment is correlation only.
// It returns an owned, validated JSON response within the bootstrap wire bound.
func (i *issuer) Issue(ctx context.Context, identity NodeIdentity, request wire.BootstrapRequest) ([]byte, error) {
	cfg := i.runtimeConfig()

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

	csr, err := x509.ParseCertificateRequest(request.CSRDER)
	if err != nil || csr.CheckSignature() != nil {
		return nil, wire.InvalidRequest
	}

	pub, ok := csr.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, wire.InvalidRequest
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

// AuthenticateCertificate requires a verified chain, the client-auth usage,
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

	leaf := state.PeerCertificates[0]

	now := time.Now()
	if leaf.IsCA || leaf.KeyUsage != x509.KeyUsageDigitalSignature || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth || len(leaf.UnknownExtKeyUsage) != 0 || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return NodeIdentity{}, wire.Unauthenticated
	}

	if _, ok := leaf.PublicKey.(ed25519.PublicKey); !ok {
		return NodeIdentity{}, wire.Unauthenticated
	}

	if len(leaf.URIs) != 1 {
		return NodeIdentity{}, wire.Unauthenticated
	}

	uri := leaf.URIs[0]

	node := wire.NodeID(strings.TrimPrefix(uri.Path, "/node/"))
	if !wire.ValidUUID(string(node)) || uri.String() != "spiffe://"+uri.Host+"/node/"+string(node) {
		return NodeIdentity{}, wire.Unauthenticated
	}

	if uri.Host != string(cfg.Cluster) {
		return NodeIdentity{}, wire.Forbidden
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

type bootstrap struct {
	owner     *Authority
	settings  frozenConfig
	Client    client.Writer
	APIReader client.Reader
	Config    Config
	Issuer    *issuer
}

func (b *bootstrap) runtimeConfig() Config { return b.settings.get(&b.Config) }

// Authenticate performs TokenReview for racer-control, checks the live bound Pod
// UID and authorized ServiceAccount/workload, and resolves its assigned Node UID.
// Token contents, CSR contents, and requested names are not authority on their own.
func (b *bootstrap) Authenticate(ctx context.Context, r *http.Request) (NodeIdentity, error) {
	cfg := b.runtimeConfig()

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

	podName, podUID := singleExtra(status.User, "pod-name"), singleExtra(status.User, "pod-uid")
	if podName == "" || podUID == "" || status.User.UID == "" {
		return NodeIdentity{}, wire.Unauthenticated
	}

	var pod corev1.Pod
	if err := b.APIReader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: podName}, &pod); err != nil {
		return NodeIdentity{}, authorizationError(err)
	}

	if string(pod.UID) != podUID {
		return NodeIdentity{}, wire.Forbidden
	}

	if err := authorizePod(ctx, b.APIReader, cfg, &pod, status.User.UID); err != nil {
		return NodeIdentity{}, err
	}

	var node corev1.Node
	if err := b.APIReader.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, &node); err != nil {
		return NodeIdentity{}, authorizationError(err)
	}

	if !authorizedNode(&node) {
		return NodeIdentity{}, wire.Forbidden
	}
	// Require unambiguous current node bindings as well as the live Pod binding.
	for key, want := range map[string]string{"node-name": node.Name, "node-uid": string(node.UID)} {
		if singleExtra(status.User, key) != want {
			return NodeIdentity{}, wire.Forbidden
		}
	}

	if err := ctx.Err(); err != nil {
		return NodeIdentity{}, err
	}

	if !time.Now().Before(expires) {
		return NodeIdentity{}, wire.Unauthenticated
	}

	return NodeIdentity{owner: b.owner, bearer: true, cluster: cfg.Cluster, node: wire.NodeID(node.UID), nodeName: node.Name, expires: expires}, nil
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

// Enroll validates CSR proof of possession and binds the issued identity to the
// token, not caller-provided SANs. Every issuance uses a token, including renewal.
// Retries correlate by enrollment ID; there is no persistent receipt ledger.
// The returned bytes are the issuer's bounded, validated JSON response.
func (b *bootstrap) Enroll(ctx context.Context, r *http.Request, request wire.BootstrapRequest) ([]byte, error) {
	response, _, err := b.enroll(ctx, r, request)
	return response, err
}

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

		return response, EnrollmentHint{Node: *node, Shares: request.Shares, RDMANICs: wire.CanonicalRDMANICs(request.RDMANICs), Expires: identity.expires}, nil
	}

	return nil, EnrollmentHint{}, wire.Forbidden
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
func authorizePod(ctx context.Context, reader client.Reader, cfg Config, pod *corev1.Pod, serviceAccountUID string) error {
	if pod.Namespace != cfg.Namespace || pod.UID == "" || pod.DeletionTimestamp != nil ||
		pod.Spec.NodeName == "" || pod.Spec.ServiceAccountName != cfg.DataplaneServiceAccount ||
		pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return wire.Forbidden
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" ||
		!slices.Contains(managedWorkloadNames(cfg), owner.Name) || owner.UID == "" {
		return wire.Forbidden
	}

	ownership, err := readManagedWorkloadIdentities(ctx, reader, cfg)
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
