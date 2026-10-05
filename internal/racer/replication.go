// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/racer/wire"
)

const (
	replicationPath     = "/internal/v1/snapshot"
	ReplicationAudience = "racer-controller-replication"
)

// Replication observes durable authority on every replica. No request from a
// dataplane performs these reads. Only the elected publisher supplies image bytes.
type Replication struct {
	authority    *Authority
	settings     frozenConfig
	Config       Config
	Client       client.Client
	APIReader    client.Reader
	Publications *Publications
	Trust        *Trust
	CatalogGate  *CatalogGate
	mu           sync.Mutex
	leader       context.Context
}

func (r *Replication) runtimeConfig() Config { return r.settings.get(&r.Config) }

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
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.leader != nil && r.leader.Err() == nil
}

func (*Replication) NeedLeaderElection() bool { return false }

func (r *Replication) interval() time.Duration {
	return min(5*time.Second, r.runtimeConfig().SnapshotMaxAge/3)
}

func (r *Replication) Start(ctx context.Context) error {
	// Credential observations must continue while the follower's single poll is
	// blocked or its leader is unreachable.
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
			poll, cancel := context.WithTimeout(ctx, 2*r.interval()+r.runtimeConfig().Limits.WriteTimeout)
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
	if err := r.operationAuthority().Observe(ctx); err != nil && ctx.Err() == nil {
		ctrl.LoggerFrom(ctx).V(1).Info("authority observation retry", "error", err)
	}
}

func (r *Replication) operationAuthority() *Authority {
	if r.authority != nil {
		return r.authority
	}

	return &Authority{config: r.runtimeConfig(), client: r.Client, reader: r.APIReader, gate: r.CatalogGate, trust: r.Trust, publications: r.Publications}
}

// Observe refreshes local serving authority without network replication.
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

// installReplica is the alternate proof to publisher CAS: bounded canonical
// decoding plus exact authoritative durable confirmation, never a trusted hash
// supplied by the remote peer. No blob is persisted.
func (r *Replication) installReplica(ctx, process context.Context, image wire.Publication) error {
	return r.operationAuthority().AcceptReplica(ctx, process, image)
}

// AcceptReplica installs only canonical bytes matching authoritative durable state.
func (a *Authority) AcceptReplica(ctx, process context.Context, image wire.Publication) error {
	encoded, err := wire.EncodePublication(image)
	if err != nil {
		return err
	}

	content, membership, err := wire.ContentHashes(image)
	if err != nil {
		return err
	}

	want := VersionRecord{Cluster: image.Cluster, Sequence: image.Sequence, MembershipVersion: image.MembershipVersion, ContentHash: content, MembershipHash: membership}

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
	} // Publisher advanced during transfer; retry.

	return a.publications.Install(&CommittedPublication{owner: a.publications, record: record, encoded: string(encoded), leadership: process})
}

func (r *Replication) leaderAddress(ctx context.Context) (string, error) {
	var lease coordv1.Lease
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.runtimeConfig().Namespace, Name: "racer-controller"}, &lease); err != nil {
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
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.runtimeConfig().Namespace, Name: name}, &pod); err != nil {
		return "", err
	}

	if string(pod.UID) != uid || !r.controllerPod(&pod) || net.ParseIP(pod.Status.PodIP) == nil {
		return "", wire.Unavailable
	}

	return net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(r.runtimeConfig().ReplicationPort))), nil
}

func (r *Replication) controllerPod(pod *corev1.Pod) bool {
	return controllerPod(r.runtimeConfig(), pod)
}

func controllerPod(cfg Config, pod *corev1.Pod) bool {
	return pod.Namespace == cfg.Namespace && pod.UID != "" && pod.DeletionTimestamp == nil && pod.Spec.ServiceAccountName == cfg.ControllerServiceAccount && pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded
}

func (r *Replication) poll(ctx, process context.Context) error {
	address, err := r.leaderAddress(ctx)
	if err != nil {
		return err
	}

	pem, err := os.ReadFile(r.runtimeConfig().ReplicationTrustFile)
	if err != nil {
		return err
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return wire.Unavailable
	}

	token, err := os.ReadFile(r.runtimeConfig().ReplicationTokenFile)
	if err != nil {
		return err
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: r.runtimeConfig().ReplicationServerName}, TLSHandshakeTimeout: r.interval(), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()

	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	path := "https://" + address + replicationPath
	if current, err := r.operationAuthority().Current(); err == nil {
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

	return r.installReplica(ctx, process, image)
}

func (r *Replication) authenticate(ctx context.Context, request *http.Request) (string, time.Time, error) {
	identity, err := r.operationAuthority().AuthenticateReplica(ctx, request)
	return identity.UID(), identity.Expires(), err
}

// ReplicaIdentity is verified output, never a caller-supplied authorization fact.
type ReplicaIdentity struct {
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

	return ReplicaIdentity{uid: uid, expires: expires}, err
}

func (s *Server) serveReplication(w http.ResponseWriter, request *http.Request) {
	r := s.Replication
	if !r.isLeader() {
		writeFailure(w, wire.Unavailable)
		return
	}

	after, err := snapshotCursor(request)
	if err != nil {
		writeFailure(w, err)
		return
	}

	if !take(s.bootstrapSlots) {
		writeFailure(w, wire.Overloaded)
		return
	}

	authCtx, cancel := context.WithTimeout(request.Context(), s.config.Limits.WriteTimeout)
	uid, expires, err := r.authenticate(authCtx, request)

	cancel()
	release(s.bootstrapSlots)

	if err != nil {
		writeFailure(w, err)
		return
	}

	if !s.replicationPolls.acquire(uid) {
		writeFailure(w, wire.Overloaded)

		return
	}

	defer s.replicationPolls.release(uid)

	r.mu.Lock()
	leader := r.leader
	r.mu.Unlock()

	ctx, cancel := context.WithDeadline(request.Context(), minTime(expires, time.Now().Add(r.interval())))
	defer cancel()

	stop := context.AfterFunc(leader, cancel)
	defer stop()

	responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(r.interval() + s.config.Limits.WriteTimeout)))

	var publication *PublicationHandle

	for {
		var changed <-chan struct{}

		publication, changed, err = s.servingAuthority().CurrentAndSubscribe()
		if err != nil || after == nil || publication.Sequence() > *after {
			break
		}

		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-changed:
		}

		if err != nil {
			break
		}
	}

	if !r.isLeader() || !time.Now().Before(expires) {
		writeFailure(w, wire.Unavailable)
		return
	}

	unchanged := errors.Is(err, context.DeadlineExceeded)
	if unchanged {
		publication, err = s.servingAuthority().Current()
	}

	if err != nil {
		writeFailure(w, err)
		return
	}

	if !take(s.writes) {
		writeFailure(w, wire.Overloaded)
		return
	}
	defer release(s.writes)

	windowCtx, stopWrite := context.WithDeadline(request.Context(), minTime(expires, time.Now().Add(s.config.Limits.WriteTimeout)))
	defer stopWrite()

	stopLeader := context.AfterFunc(leader, stopWrite)
	defer stopLeader()

	writeCtx, stopAuthority, err := publication.WriteContext(windowCtx)
	if err != nil {
		writeFailure(w, err)
		return
	}
	defer stopAuthority()

	deadline, _ := writeCtx.Deadline()

	stopConnection := boundConnection(writeCtx, deadline)
	defer stopConnection()

	responseControl(http.NewResponseController(w).SetWriteDeadline(deadline))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if unchanged {
		w.WriteHeader(http.StatusNoContent)
		flushResponse(writeCtx, w)

		return
	}

	if _, err := publication.ForBase("").WriteTo(writeCtx, w); err != nil {
		panic(http.ErrAbortHandler)
	}

	flushResponse(writeCtx, w)
}
