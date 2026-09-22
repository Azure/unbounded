// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Azure/unbounded/internal/racer"
	"github.com/Azure/unbounded/internal/racer/pki"
)

const replicaComponent = "racer-controlplane"

// replicaTLS owns one process-local private key. Kubernetes carries only its
// signed CSR, public certificate, and installed trust acknowledgment.
type replicaTLS struct {
	kube                       client.Client
	namespace, podName, bootID string
	podUID                     types.UID
	manager                    *pki.Manager
	hot                        *pki.HotTLS
	proofHot                   *pki.HotTLS
	keyPEM, csrPEM             []byte
	listen                     string
	proofPort                  string
	interval                   time.Duration
	mu                         sync.RWMutex
	installed                  replicaAcknowledgment
	expires                    time.Time
	drained                    func() bool
	onInstalled                func()
	installedIssuer            string
}

type replicaAcknowledgment struct {
	PodUID                string `json:"pod_uid"`
	BootID                string `json:"boot_id"`
	CSR                   string `json:"csr_digest"`
	Generation            uint64 `json:"generation"`
	Digest                string `json:"digest"`
	OldConnectionsDrained bool   `json:"old_connections_drained"`
}

func newReplicaTLS(kube client.Client, namespace, podName string, podUID types.UID, bootID string, manager *pki.Manager, hot *pki.HotTLS) (*replicaTLS, error) {
	if kube == nil || namespace == "" || podName == "" || podUID == "" || bootID == "" || bootID == "pending" || manager == nil || hot == nil {
		return nil, errors.New("replica TLS requires direct client, Pod identity, boot ID, PKI manager and HotTLS")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, err
	}

	return &replicaTLS{
		kube: kube, namespace: namespace, podName: podName, podUID: podUID, bootID: bootID, manager: manager, hot: hot, proofHot: pki.NewHotTLS(),
		keyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), csrPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}),
		listen: ":8445", proofPort: "8445", interval: 2 * time.Second,
	}, nil
}

func (*replicaTLS) NeedLeaderElection() bool { return false }

func (r *replicaTLS) ProofTLS() *pki.HotTLS { return r.proofHot }

// SetDrainedCheck supplies transport-owned evidence that old-root connections
// are gone. The conservative default never grants drain credit.
func (r *replicaTLS) SetDrainedCheck(check func() bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.drained = check
}

// SetInstalledHook notifies the transport after both contexts have installed a
// different bundle or production issuer, before observing drain state or acking.
func (r *replicaTLS) SetInstalledHook(hook func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.onInstalled = hook
}

// Start runs on followers too. Missing certificates during first-cluster
// bootstrap never prevent the elected leader's independent issuance loop.
func (r *replicaTLS) Start(ctx context.Context) error {
	listener, err := net.Listen("tcp", r.listen)
	if err != nil {
		return err
	}

	server := &http.Server{Handler: http.HandlerFunc(r.serveProof), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 5 * time.Second}

	server.SetKeepAlivesEnabled(false)

	defer func() {
		if err := server.Close(); err != nil {
			log.FromContext(ctx).Error(err, "close replica proof server")
		}
	}()

	finished := make(chan error, 1)

	go func() { finished <- server.Serve(tls.NewListener(listener, r.proofHot.ServerConfig(tls.NoClientCert))) }()

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		if err := r.reconcileLocal(ctx); err != nil && ctx.Err() == nil {
			log.FromContext(ctx).Error(err, "replica TLS refresh pending", "pod", r.podName)
		}

		select {
		case <-ctx.Done():
			return nil
		case err := <-finished:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}

			return err
		case <-ticker.C:
		}
	}
}

func (r *replicaTLS) Ready(_ *http.Request) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.installed.Digest == "" || !time.Now().Before(r.expires) {
		return errors.New("replica TLS certificate not ready")
	}

	return nil
}

func replicaDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func replicaMapName(uid types.UID) string { return "racer-replica-" + string(uid) }

func replicaMapOwned(cm *corev1.ConfigMap, pod *corev1.Pod) bool {
	owner := metav1.GetControllerOf(cm)
	return owner != nil && owner.APIVersion == "v1" && owner.Kind == "Pod" && owner.Name == pod.Name && owner.UID == pod.UID && cm.Namespace == pod.Namespace && cm.Name == replicaMapName(pod.UID)
}

// replicaPod validates the entire live ownership chain; labels alone do not
// authorize issuing a control-plane certificate. Terminating Pods remain
// members until the API confirms they are absent or replaced.
func replicaPod(ctx context.Context, kube client.Reader, pod *corev1.Pod) error {
	if pod.UID == "" || pod.Spec.ServiceAccountName != replicaComponent || pod.Labels[racer.MetadataPrefix+"component"] != replicaComponent {
		return errors.New("not a managed control-plane Pod")
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "ReplicaSet" || owner.UID == "" {
		return errors.New("control-plane Pod has no ReplicaSet controller")
	}

	var rs appsv1.ReplicaSet
	if err := kube.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: owner.Name}, &rs); err != nil {
		return err
	}

	if rs.UID != owner.UID {
		return errors.New("control-plane ReplicaSet UID mismatch")
	}

	owner = metav1.GetControllerOf(&rs)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "Deployment" || owner.Name != replicaComponent || owner.UID == "" {
		return errors.New("control-plane ReplicaSet has no managed Deployment controller")
	}

	var deployment appsv1.Deployment
	if err := kube.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: owner.Name}, &deployment); err != nil {
		return err
	}

	if deployment.UID != owner.UID || deployment.Labels[racer.MetadataPrefix+"component"] != replicaComponent || deployment.Spec.Template.Spec.ServiceAccountName != replicaComponent {
		return errors.New("control-plane Deployment identity mismatch")
	}

	return nil
}

func (r *replicaTLS) publishCSR(ctx context.Context) error {
	var pod corev1.Pod
	if err := r.kube.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: r.podName}, &pod); err != nil {
		return err
	}

	if pod.UID != r.podUID {
		return errors.New("local control-plane Pod UID changed")
	}

	if err := replicaPod(ctx, r.kube, &pod); err != nil {
		return err
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cm corev1.ConfigMap

		key := types.NamespacedName{Namespace: r.namespace, Name: replicaMapName(r.podUID)}

		err := r.kube.Get(ctx, key, &cm)
		if apierrors.IsNotFound(err) {
			controller := true
			cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: r.podName, UID: r.podUID, Controller: &controller}}}, Data: map[string]string{"boot": r.bootID, "csr": string(r.csrPEM)}}

			return r.kube.Create(ctx, &cm)
		}

		if err != nil {
			return err
		}

		if !replicaMapOwned(&cm, &pod) {
			return errors.New("replica ConfigMap owner mismatch")
		}

		if cm.Data["boot"] == r.bootID && cm.Data["csr"] == string(r.csrPEM) {
			return nil
		}
		// A restart invalidates the previous process's response and acknowledgment.
		cm.Data = map[string]string{"boot": r.bootID, "csr": string(r.csrPEM)}

		return r.kube.Update(ctx, &cm)
	})
}

func (r *replicaTLS) reconcileLocal(ctx context.Context) error {
	if err := r.publishCSR(ctx); err != nil {
		return err
	}

	var cm, trust corev1.ConfigMap
	if err := r.kube.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: replicaMapName(r.podUID)}, &cm); err != nil {
		return err
	}

	if cm.Data["certificate-boot"] != r.bootID || cm.Data["certificate-csr"] != replicaDigest(r.csrPEM) || cm.Data["certificate"] == "" || cm.Data["proof-certificate"] == "" {
		return errors.New("waiting for leader-issued replica certificates")
	}

	if err := r.kube.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: "racer-trust"}, &trust); err != nil {
		return err
	}

	bundle, err := pki.ParseBundle([]byte(trust.Data["bundle.json"]))
	if err != nil {
		return err
	}

	certificate, err := tls.X509KeyPair([]byte(cm.Data["certificate"]), r.keyPEM)
	if err != nil {
		return err
	}

	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return err
	}

	if err := replicaLeafIdentity(leaf, r.namespace); err != nil {
		return err
	}
	// Prevalidate both contexts before changing either live context. HotTLS
	// independently rejects rollback and keeps its previous valid snapshot.
	for _, name := range []string{"certificate", "proof-certificate"} {
		if !replicaCertificateMatches(cm.Data[name], string(r.csrPEM)) {
			return errors.New("replica certificate does not match local CSR")
		}

		block, _ := pem.Decode([]byte(cm.Data[name]))

		parsed, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}

		if err := replicaLeafIdentity(parsed, r.namespace); err != nil {
			return err
		}

		if parsed.NotAfter.Before(leaf.NotAfter) {
			leaf.NotAfter = parsed.NotAfter
		}

		candidate := pki.NewHotTLS()
		if err := candidate.Update([]byte(trust.Data["bundle.json"]), []byte(cm.Data[name]), r.keyPEM); err != nil {
			return err
		}
	}

	ack := replicaAcknowledgment{PodUID: string(r.podUID), BootID: r.bootID, CSR: replicaDigest(r.csrPEM), Generation: bundle.Generation, Digest: bundle.Digest()}
	issuer := ""

	for remaining := []byte(bundle.Certificates); len(bytes.TrimSpace(remaining)) > 0; {
		block, rest := pem.Decode(remaining)
		if block == nil {
			return errors.New("invalid replica trust roots")
		}

		root, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}

		if leaf.CheckSignatureFrom(root) == nil {
			issuer = replicaDigest(root.Raw)
		}

		remaining = rest
	}

	if issuer == "" {
		return errors.New("replica issuer absent from trust bundle")
	}

	r.mu.Lock()

	err = r.hot.Update([]byte(trust.Data["bundle.json"]), []byte(cm.Data["certificate"]), r.keyPEM)
	if err == nil {
		err = r.proofHot.Update([]byte(trust.Data["bundle.json"]), []byte(cm.Data["proof-certificate"]), r.keyPEM)
	}

	if err == nil {
		if (r.installed.Digest != ack.Digest || r.installedIssuer != issuer) && r.onInstalled != nil {
			r.onInstalled()
		}

		if r.drained != nil {
			ack.OldConnectionsDrained = r.drained()
		}

		r.installed, r.expires = ack, leaf.NotAfter
		r.installedIssuer = issuer
	}
	r.mu.Unlock()

	if err != nil {
		return err
	}

	data, err := json.Marshal(ack)
	if err != nil {
		return err
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current corev1.ConfigMap
		if err := r.kube.Get(ctx, client.ObjectKeyFromObject(&cm), &current); err != nil {
			return err
		}

		if current.Data["boot"] != r.bootID || current.Data["csr"] != string(r.csrPEM) || current.Data["certificate"] != cm.Data["certificate"] || current.Data["proof-certificate"] != cm.Data["proof-certificate"] {
			return errors.New("replica certificate changed before acknowledgment")
		}

		if current.Data["ack"] == string(data) {
			return nil
		}

		current.Data["ack"] = string(data)

		return r.kube.Update(ctx, &current)
	})
}

func replicaLeafIdentity(leaf *x509.Certificate, namespace string) error {
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != "spiffe://racer/controlplane" {
		return errors.New("replica certificate control-plane identity mismatch")
	}

	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth || len(leaf.UnknownExtKeyUsage) != 0 {
		return errors.New("replica certificate must be server-auth only")
	}

	return leaf.VerifyHostname(replicaComponent + "." + namespace + ".svc")
}

func (r *replicaTLS) serveProof(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if req.Method != http.MethodGet || req.URL.Path != "/v3/replica-proof" || req.TLS == nil {
		http.NotFound(w, req)
		return
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.installed.Digest == "" || !time.Now().Before(r.expires) {
		http.Error(w, "replica TLS not ready", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(r.installed); err != nil {
		log.FromContext(req.Context()).Error(err, "write replica proof")
	}
}

// probe performs a fresh full TLS handshake against the actual Pod address.
// The acknowledgment alone, whether from HTTP or Kubernetes, is never proof.
func (r *replicaTLS) probe(ctx context.Context, pod *corev1.Pod, expected replicaAcknowledgment, csr string) (pki.Proof, error) {
	var zero pki.Proof

	ip := net.ParseIP(pod.Status.PodIP)
	if ip == nil {
		return zero, errors.New("control-plane Pod has no valid IP")
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	dialer := &net.Dialer{Timeout: 5 * time.Second}

	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), r.proofPort))
	if err != nil {
		return zero, err
	}

	defer func() {
		if err := raw.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.FromContext(ctx).Error(err, "close replica proof transport")
		}
	}()

	serverName := replicaComponent + "." + r.namespace + ".svc"

	conn, finish, err := r.proofHot.HandshakeProof(ctx, raw, false, serverName)
	if err != nil {
		return zero, err
	}

	defer func() {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.FromContext(ctx).Error(err, "close replica proof TLS")
		}
	}()

	state := conn.ConnectionState()
	if err := replicaLeafIdentity(state.PeerCertificates[0], r.namespace); err != nil {
		return zero, err
	}

	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: state.PeerCertificates[0].Raw})
	if !replicaCertificateMatches(string(certificate), csr) || replicaDigest([]byte(csr)) != expected.CSR {
		return zero, errors.New("replica TLS key does not match boot CSR")
	}

	transport := &http.Transport{DisableKeepAlives: true, DialTLSContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil }}
	defer transport.CloseIdleConnections()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+serverName+"/v3/replica-proof", nil)
	if err != nil {
		return zero, err
	}

	response, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
	if err != nil {
		return zero, err
	}

	defer func() {
		if err := response.Body.Close(); err != nil {
			log.FromContext(ctx).Error(err, "close replica proof response")
		}
	}()

	if response.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("replica proof HTTP status %d", response.StatusCode)
	}

	var actual replicaAcknowledgment

	decoder := json.NewDecoder(io.LimitReader(response.Body, 4096))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&actual); err != nil {
		return zero, err
	}

	if decoder.Decode(new(any)) != io.EOF || actual != expected {
		return zero, errors.New("replica proof does not match exact boot, CSR and installed bundle")
	}

	return finish(pki.Acknowledgment{Generation: actual.Generation, Digest: actual.Digest, OldConnectionsDrained: actual.OldConnectionsDrained})
}

// Match the certificate to the exact CSR, including when reading an existing
// response after leadership takeover. CSR names are deliberately ignored.
func replicaCertificateMatches(certificate, csr string) bool {
	certBlock, _ := pem.Decode([]byte(certificate))

	csrBlock, _ := pem.Decode([]byte(csr))
	if certBlock == nil || csrBlock == nil {
		return false
	}

	leaf, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return false
	}

	request, err := x509.ParseCertificateRequest(csrBlock.Bytes)
	if err != nil || request.CheckSignature() != nil {
		return false
	}

	return bytes.Equal(leaf.RawSubjectPublicKeyInfo, request.RawSubjectPublicKeyInfo)
}

func (r *replicaTLS) issueReplica(ctx context.Context, pod *corev1.Pod, cm *corev1.ConfigMap, bundle pki.TrustBundle) error {
	if !replicaMapOwned(cm, pod) || cm.Data["boot"] == "" || cm.Data["csr"] == "" {
		return errors.New("replica CSR has invalid owner or missing boot")
	}

	id := pki.Identity{Kind: pki.ControlPlane, PodUID: string(pod.UID), BootID: cm.Data["boot"], ContainerID: runningContainerID(pod, "controller")}
	if err := r.manager.Admit(ctx, id); err != nil {
		return err
	}

	csrDigest := replicaDigest([]byte(cm.Data["csr"]))
	proofRoot := bundle.Active
	// The published overlap bundle orders the original root before the new one.
	remaining := []byte(bundle.Certificates)
	for len(remaining) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil {
			return errors.New("invalid trust certificate PEM")
		}

		proofRoot = replicaDigest(block.Bytes)
		remaining = bytes.TrimSpace(rest)
	}

	bound := cm.Data["certificate-boot"] == id.BootID && cm.Data["certificate-csr"] == csrDigest
	valid := func(name, root string) bool {
		if !bound || cm.Data[name+"-root"] != root || !replicaCertificateMatches(cm.Data[name], cm.Data["csr"]) {
			return false
		}

		block, _ := pem.Decode([]byte(cm.Data[name]))
		leaf, err := x509.ParseCertificate(block.Bytes)

		return err == nil && time.Now().Add(time.Hour).Before(leaf.NotAfter)
	}
	changed := false

	if !valid("certificate", bundle.Active) {
		issued, err := r.manager.Issue(ctx, []byte(cm.Data["csr"]), id)
		if err != nil {
			return err
		}

		cm.Data["certificate"] = string(issued.CertificatePEM)
		cm.Data["certificate-root"] = issued.RootDigest
		changed = true
	}

	if !valid("proof-certificate", proofRoot) {
		issued, err := r.manager.IssueProbe(ctx, []byte(cm.Data["csr"]), id)
		if err != nil {
			return err
		}

		cm.Data["proof-certificate"] = string(issued.CertificatePEM)
		cm.Data["proof-certificate-root"] = issued.RootDigest
		changed = true
	}

	if !changed {
		return nil
	}
	// Use the original resource version. A concurrent boot/CSR replacement must
	// cause a conflict rather than publishing a certificate for the wrong key.
	cm.Data["certificate-boot"] = id.BootID
	cm.Data["certificate-csr"] = csrDigest
	delete(cm.Data, "ack")

	return r.kube.Update(ctx, cm)
}

// ReconcileLeader must run after fence acquisition and before each rotation
// reconciliation. A full direct Pod list is also the authoritative retirement
// evidence: missing CSR, labels, readiness, or heartbeats never remove members.
func (r *replicaTLS) ReconcileLeader(ctx context.Context) error {
	var pods corev1.PodList
	if err := r.kube.List(ctx, &pods, client.InNamespace(r.namespace)); err != nil {
		return err
	}

	members, err := r.manager.Members(ctx)
	if err != nil {
		return err
	}

	present := make(map[string]*corev1.Pod, len(pods.Items))
	known := make(map[string]bool)

	for _, id := range members {
		if id.Kind == pki.ControlPlane {
			known[id.PodUID] = true
		}
	}

	var (
		eligible []*corev1.Pod
		failures []error
	)

	for i := range pods.Items {
		pod := &pods.Items[i]

		present[string(pod.UID)] = pod
		if err := replicaPod(ctx, r.kube, pod); err != nil {
			// An apparent managed Pod with temporarily unreadable ownership must
			// block parent advancement rather than silently disappear from the sweep.
			if pod.Spec.ServiceAccountName == replicaComponent && pod.Labels[racer.MetadataPrefix+"component"] == replicaComponent {
				failures = append(failures, fmt.Errorf("validate replica %s: %w", pod.Name, err))
			}

			continue
		}

		eligible = append(eligible, pod)
		if !known[string(pod.UID)] {
			if err := r.manager.Admit(ctx, pki.Identity{Kind: pki.ControlPlane, PodUID: string(pod.UID), BootID: "pending"}); err != nil {
				failures = append(failures, err)
			}
		}
	}

	if len(failures) != 0 {
		return errors.Join(failures...)
	}

	for _, id := range members {
		pod := present[id.PodUID]
		if id.Kind == pki.ControlPlane && (pod == nil || containerAuthoritativelyStopped(pod, "controller", id.ContainerID)) {
			if err := r.manager.Retire(ctx, id.Key()); err != nil {
				return err
			}
		}
	}

	if err := r.manager.Publish(ctx); err != nil {
		return err
	}

	bundle, err := r.manager.Bundle(ctx)
	if err != nil {
		return err
	}

	for _, pod := range eligible {
		var cm corev1.ConfigMap
		if err := r.kube.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: replicaMapName(pod.UID)}, &cm); err != nil {
			if !apierrors.IsNotFound(err) {
				failures = append(failures, err)
			}

			continue
		}

		if cm.Data["boot"] == "pending" {
			failures = append(failures, errors.New("reserved replica boot ID"))
			continue
		}

		if err := r.issueReplica(ctx, pod, &cm, bundle); err != nil {
			failures = append(failures, fmt.Errorf("issue replica %s: %w", pod.Name, err))
			continue
		}

		var ack replicaAcknowledgment
		if json.Unmarshal([]byte(cm.Data["ack"]), &ack) != nil {
			continue
		}

		if ack.PodUID != string(pod.UID) || ack.BootID != cm.Data["boot"] || ack.CSR != replicaDigest([]byte(cm.Data["csr"])) || ack.Generation != bundle.Generation || ack.Digest != bundle.Digest() {
			continue
		}

		proof, err := r.probe(ctx, pod, ack, cm.Data["csr"])
		if err != nil {
			failures = append(failures, fmt.Errorf("prove replica %s: %w", pod.Name, err))
			continue
		}

		key := pki.MemberKey{PodUID: string(pod.UID), BootID: ack.BootID}
		if err := r.manager.ObserveHeartbeat(ctx, key, pki.Acknowledgment{Generation: ack.Generation, Digest: ack.Digest, OldConnectionsDrained: ack.OldConnectionsDrained}); err != nil {
			failures = append(failures, err)
			continue
		}

		if err := r.manager.RecordTLSProof(ctx, key, proof); err != nil {
			failures = append(failures, err)
			continue
		}
		// This is only a pre-enrollment placeholder, never an earlier live boot.
		// Actual old boots remain until Kubernetes confirms Pod deletion or
		// replacement of their recorded container runtime identity.
		if err := r.manager.Retire(ctx, pki.MemberKey{PodUID: string(pod.UID), BootID: "pending"}); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}
