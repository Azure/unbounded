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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	authv1 "k8s.io/api/authentication/v1"
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
	Config       Config
	Client       client.Client
	APIReader    client.Reader
	Publications *Publications
	Trust        *Trust
	Lifecycle    *Lifecycle
	CatalogGate  *CatalogGate
	mu           sync.Mutex
	leader       context.Context
	polls        map[string]bool
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
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.leader != nil && r.leader.Err() == nil
}

func (*Replication) NeedLeaderElection() bool { return false }

func (r *Replication) interval() time.Duration {
	return min(5*time.Second, r.Config.snapshotMaxAge()/3)
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
			poll, cancel := context.WithTimeout(ctx, 2*r.interval()+r.Config.Limits.WriteTimeout)
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
	if err := r.CatalogGate.Acquire(ctx); err != nil {
		return
	}
	defer r.CatalogGate.Release()

	state, err := loadSigning(ctx, r.APIReader, r.Config, time.Now())
	if err == nil {
		err = r.Trust.install(ctx, state.roots, state.bundle)
	}

	if err == nil {
		_, record, readErr := readVersion(ctx, r.APIReader, r.Config)

		err = readErr
		if err == nil {
			err = r.Publications.confirm(record)
		}
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}

	if shouldInvalidateTrust(err) {
		r.Trust.invalidate()
		r.Publications.Suspend()
	}

	_, trustErr := r.Trust.pool()
	r.Lifecycle.SetIssuerReady(trustErr == nil)
}

// confirm never promotes a hash to an image. It only renews the freshness of an
// already validated image when all durable counters and hashes still match.
func (p *Publications) confirm(record VersionRecord) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.current == nil {
		return nil
	}

	old := p.current.record
	if record.Cluster != old.Cluster || record.Sequence < old.Sequence || record.MembershipVersion < old.MembershipVersion || record.Sequence == old.Sequence && record != old || record.MembershipVersion == old.MembershipVersion && record.MembershipHash != old.MembershipHash {
		return wire.Conflict
	}

	if record == old && !p.suspended {
		p.confirmed = time.Now()
	}

	return nil
}

// installReplica is the alternate proof to publisher CAS: bounded canonical
// decoding plus exact authoritative durable confirmation, never a trusted hash
// supplied by the remote peer. No blob is persisted.
func (r *Replication) installReplica(ctx, process context.Context, image wire.Publication) error {
	encoded, err := wire.EncodePublication(image)
	if err != nil {
		return err
	}

	content, membership, err := wire.ContentHashes(image)
	if err != nil {
		return err
	}

	want := VersionRecord{Cluster: image.Cluster, Sequence: image.Sequence, MembershipVersion: image.MembershipVersion, ContentHash: content, MembershipHash: membership}

	if err := r.CatalogGate.Acquire(ctx); err != nil {
		return err
	}
	defer r.CatalogGate.Release()

	_, record, err := readVersion(ctx, r.APIReader, r.Config)
	if err != nil {
		if shouldInvalidateTrust(err) {
			r.Publications.Suspend()
			r.Trust.invalidate()
		}

		return err
	}

	if err := r.Publications.confirm(record); err != nil {
		r.Publications.Suspend()
		return err
	}

	if record != want {
		return wire.Unavailable
	} // Publisher advanced during transfer; retry.

	return r.Publications.Install(&CommittedPublication{owner: r.Publications, record: record, encoded: string(encoded), leadership: process})
}

func (r *Replication) leaderAddress(ctx context.Context) (string, error) {
	var lease coordv1.Lease
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: "racer-controller"}, &lease); err != nil {
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
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, &pod); err != nil {
		return "", err
	}

	if string(pod.UID) != uid || !r.controllerPod(&pod) || net.ParseIP(pod.Status.PodIP) == nil {
		return "", wire.Unavailable
	}

	return net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(r.Config.ReplicationPort))), nil
}

func (r *Replication) controllerPod(pod *corev1.Pod) bool {
	return pod.Namespace == r.Config.Namespace && pod.UID != "" && pod.DeletionTimestamp == nil && pod.Spec.ServiceAccountName == r.Config.ControllerServiceAccount && pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded
}

func (r *Replication) poll(ctx, process context.Context) error {
	address, err := r.leaderAddress(ctx)
	if err != nil {
		return err
	}

	pem, err := os.ReadFile(r.Config.ReplicationTrustFile)
	if err != nil {
		return err
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return wire.Unavailable
	}

	token, err := os.ReadFile(r.Config.ReplicationTokenFile)
	if err != nil {
		return err
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: r.Config.ReplicationServerName}, TLSHandshakeTimeout: r.interval(), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()

	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	path := "https://" + address + replicationPath
	if current, err := r.Publications.Current(); err == nil {
		path += "?after=" + strconv.FormatUint(uint64(current.record.Sequence), 10)
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
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		return "", time.Time{}, wire.Unauthenticated
	}

	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n,") {
		return "", time.Time{}, wire.Unauthenticated
	}

	review := &authv1.TokenReview{Spec: authv1.TokenReviewSpec{Token: token, Audiences: []string{ReplicationAudience}}}
	if err := r.Client.Create(ctx, review); err != nil {
		return "", time.Time{}, wire.Unavailable
	}

	status := review.Status
	if !status.Authenticated || status.Error != "" || !slices.Contains(status.Audiences, ReplicationAudience) {
		return "", time.Time{}, wire.Unauthenticated
	}

	if status.User.Username != "system:serviceaccount:"+r.Config.Namespace+":"+r.Config.ControllerServiceAccount {
		return "", time.Time{}, wire.Forbidden
	}

	name, uid := singleExtra(status.User, "pod-name"), singleExtra(status.User, "pod-uid")
	if name == "" || uid == "" || status.User.UID == "" {
		return "", time.Time{}, wire.Unauthenticated
	}

	var pod corev1.Pod
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: name}, &pod); err != nil {
		return "", time.Time{}, authorizationError(err)
	}

	if !r.controllerPod(&pod) || string(pod.UID) != uid {
		return "", time.Time{}, wire.Forbidden
	}

	var sa corev1.ServiceAccount
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.ControllerServiceAccount}, &sa); err != nil {
		return "", time.Time{}, authorizationError(err)
	}

	if string(sa.UID) != status.User.UID || sa.DeletionTimestamp != nil {
		return "", time.Time{}, wire.Forbidden
	}

	expires, err := tokenExpiration(token)

	return uid, expires, err
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

	authCtx, cancel := context.WithTimeout(request.Context(), s.Config.Limits.WriteTimeout)
	uid, expires, err := r.authenticate(authCtx, request)

	cancel()
	release(s.bootstrapSlots)

	if err != nil {
		writeFailure(w, err)
		return
	}

	r.mu.Lock()
	if r.polls == nil {
		r.polls = make(map[string]bool)
	}

	if r.polls[uid] || len(r.polls) >= s.Config.Limits.MaxConcurrentBootstrap {
		r.mu.Unlock()
		writeFailure(w, wire.Overloaded)

		return
	}

	r.polls[uid] = true
	leader := r.leader
	r.mu.Unlock()

	defer func() { r.mu.Lock(); delete(r.polls, uid); r.mu.Unlock() }()

	ctx, cancel := context.WithDeadline(request.Context(), minTime(expires, time.Now().Add(r.interval())))
	defer cancel()

	stop := context.AfterFunc(leader, cancel)
	defer stop()

	responseControl(http.NewResponseController(w).SetWriteDeadline(time.Now().Add(r.interval() + s.Config.Limits.WriteTimeout)))

	var publication *CommittedPublication

	for {
		var changed <-chan struct{}

		publication, changed, err = s.Publications.CurrentAndSubscribe()
		if err != nil || after == nil || publication.record.Sequence > *after {
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

	if errors.Is(err, context.DeadlineExceeded) {
		if s.Publications.Ready(nil) != nil {
			writeFailure(w, wire.Unavailable)
			return
		}

		w.WriteHeader(http.StatusNoContent)

		return
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

	writeCtx, stopWrite := context.WithDeadline(request.Context(), minTime(expires, time.Now().Add(s.Config.Limits.WriteTimeout)))
	defer stopWrite()

	stopLeader := context.AfterFunc(leader, stopWrite)
	defer stopLeader()

	deadline, _ := writeCtx.Deadline()

	stopConnection := boundConnection(writeCtx, deadline)
	defer stopConnection()

	responseControl(http.NewResponseController(w).SetWriteDeadline(deadline))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if _, err := publication.WriteTo(requestWriter{ctx: writeCtx, writer: w}); err != nil {
		panic(http.ErrAbortHandler)
	}
}
