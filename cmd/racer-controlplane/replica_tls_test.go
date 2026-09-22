// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/unbounded/internal/racer"
	"github.com/Azure/unbounded/internal/racer/pki"
)

func replicaFixture(t *testing.T) (client.Client, *corev1.Pod, *pki.Manager, *replicaTLS) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	controller := true
	labels := map[string]string{racer.MetadataPrefix + "component": replicaComponent}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: replicaComponent, Namespace: "racer-system", UID: "deployment", Labels: labels}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: replicaComponent}}}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "control-revision", Namespace: deployment.Namespace, UID: "replicaset", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: &controller}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "control-pod", Namespace: deployment.Namespace, UID: "pod-uid", Labels: labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &controller}}}, Spec: corev1.PodSpec{ServiceAccountName: replicaComponent}, Status: corev1.PodStatus{PodIP: "127.0.0.1"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, rs, pod).Build()

	manager, err := pki.New(kube, pod.Namespace, pki.Options{})
	if err != nil {
		t.Fatal(err)
	}

	r, err := newReplicaTLS(kube, pod.Namespace, pod.Name, pod.UID, "boot-one", manager, pki.NewHotTLS())
	if err != nil {
		t.Fatal(err)
	}

	return kube, pod, manager, r
}

func TestReplicaOwnershipRequiresActualManagedDeployment(t *testing.T) {
	for _, mutation := range []string{"none", "serviceaccount", "pod-label", "pod-owner", "replicaset-uid", "deployment-uid", "deployment-label", "deployment-serviceaccount"} {
		t.Run(mutation, func(t *testing.T) {
			kube, pod, _, _ := replicaFixture(t)
			ctx := context.Background()

			var rs appsv1.ReplicaSet
			if err := kube.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: "control-revision"}, &rs); err != nil {
				t.Fatal(err)
			}

			var deployment appsv1.Deployment
			if err := kube.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: replicaComponent}, &deployment); err != nil {
				t.Fatal(err)
			}

			switch mutation {
			case "serviceaccount":
				pod.Spec.ServiceAccountName = "untrusted"
			case "pod-label":
				pod.Labels = nil
			case "pod-owner":
				pod.OwnerReferences[0].Controller = nil
			case "replicaset-uid":
				pod.OwnerReferences[0].UID = "stale-replicaset"
			case "deployment-uid":
				rs.OwnerReferences[0].UID = "stale-deployment"
			case "deployment-label":
				deployment.Labels = nil
			case "deployment-serviceaccount":
				deployment.Spec.Template.Spec.ServiceAccountName = "untrusted"
			}

			if err := kube.Update(ctx, &rs); err != nil {
				t.Fatal(err)
			}

			if err := kube.Update(ctx, &deployment); err != nil {
				t.Fatal(err)
			}

			err := replicaPod(ctx, kube, pod)
			if (err == nil) != (mutation == "none") {
				t.Fatalf("ownership validation = %v", err)
			}
		})
	}
}

func replicaBootstrap(t *testing.T, r *replicaTLS) {
	t.Helper()

	ctx := context.Background()
	if err := r.manager.AcquireLeadership(ctx, "leader-one"); err != nil {
		t.Fatal(err)
	}

	if err := r.manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	if err := r.publishCSR(ctx); err != nil {
		t.Fatal(err)
	}

	var pod corev1.Pod
	if err := r.kube.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: r.podName}, &pod); err != nil {
		t.Fatal(err)
	}

	cm := replicaGetMap(t, r)

	bundle, err := r.manager.Bundle(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.issueReplica(ctx, &pod, &cm, bundle); err != nil {
		t.Fatal(err)
	}

	if err := r.reconcileLocal(ctx); err != nil {
		t.Fatal(err)
	}
}

func replicaGetMap(t *testing.T, r *replicaTLS) corev1.ConfigMap {
	t.Helper()

	var cm corev1.ConfigMap
	if err := r.kube.Get(context.Background(), types.NamespacedName{Namespace: r.namespace, Name: replicaMapName(r.podUID)}, &cm); err != nil {
		t.Fatal(err)
	}

	return cm
}

func TestReplicaBootstrapAndLastValidContext(t *testing.T) {
	kube, _, _, r := replicaFixture(t)

	ctx := context.Background()
	if err := r.reconcileLocal(ctx); err == nil {
		t.Fatal("unissued follower reported ready")
	}

	if r.Ready(nil) == nil {
		t.Fatal("unissued follower ready")
	}

	cm := replicaGetMap(t, r)
	if cm.Data["ack"] != "" {
		t.Fatal("acknowledged before TLS installation")
	}

	replicaBootstrap(t, r)

	if err := r.Ready(nil); err != nil {
		t.Fatal(err)
	}

	cm = replicaGetMap(t, r)

	var ack replicaAcknowledgment
	if err := json.Unmarshal([]byte(cm.Data["ack"]), &ack); err != nil {
		t.Fatal(err)
	}

	bundle, err := r.manager.Bundle(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if ack.Generation != bundle.Generation || ack.Digest != bundle.Digest() || ack.BootID != r.bootID || ack.CSR != replicaDigest(r.csrPEM) {
		t.Fatalf("incorrect ack: %+v", ack)
	}

	for key, value := range cm.Data {
		if strings.Contains(value, "PRIVATE KEY") || strings.Contains(key, "private") {
			t.Fatalf("private key published in %q", key)
		}
	}

	before := r.installed

	var trust corev1.ConfigMap
	if err := kube.Get(ctx, types.NamespacedName{Namespace: r.namespace, Name: "racer-trust"}, &trust); err != nil {
		t.Fatal(err)
	}

	trust.Data["bundle.json"] = `{"version":1,"generation":999,"active":"bad","certificates":"broken"}`
	if err := kube.Update(ctx, &trust); err != nil {
		t.Fatal(err)
	}

	if err := r.reconcileLocal(ctx); err == nil {
		t.Fatal("accepted invalid bundle")
	}

	if r.installed != before || r.Ready(nil) != nil {
		t.Fatal("invalid update replaced last valid context")
	}

	if got := replicaGetMap(t, r).Data["ack"]; got != cm.Data["ack"] {
		t.Fatal("invalid update acknowledged")
	}

	config, err := r.hot.ServerConfig(tls.NoClientCert).GetConfigForClient(&tls.ClientHelloInfo{})
	if err != nil || config == nil {
		t.Fatalf("last valid server config lost: %v", err)
	}
}

func TestReplicaRestartReplacesOnlyPublicRequest(t *testing.T) {
	kube, pod, manager, r := replicaFixture(t)
	replicaBootstrap(t, r)
	old := replicaGetMap(t, r)

	restarted, err := newReplicaTLS(kube, pod.Namespace, pod.Name, pod.UID, "boot-two", manager, pki.NewHotTLS())
	if err != nil {
		t.Fatal(err)
	}

	if err := restarted.publishCSR(context.Background()); err != nil {
		t.Fatal(err)
	}

	current := replicaGetMap(t, restarted)
	if current.Data["csr"] == old.Data["csr"] || current.Data["boot"] == old.Data["boot"] {
		t.Fatal("restart reused key or boot")
	}

	if current.Data["certificate"] != "" || current.Data["ack"] != "" {
		t.Fatal("restart retained old response or acknowledgment")
	}

	if restarted.Ready(nil) == nil {
		t.Fatal("new boot ready before new certificate")
	}

	if replicaCertificateMatches(old.Data["certificate"], current.Data["csr"]) {
		t.Fatal("old certificate matched new boot key")
	}
}

func TestReplicaRejectsCertificateForDifferentKeyAndBoot(t *testing.T) {
	for _, mismatch := range []string{"key", "boot", "digest"} {
		t.Run(mismatch, func(t *testing.T) {
			kube, pod, manager, r := replicaFixture(t)
			replicaBootstrap(t, r)
			cm := replicaGetMap(t, r)
			before := r.installed

			switch mismatch {
			case "key":
				other, err := newReplicaTLS(kube, pod.Namespace, pod.Name, pod.UID, "other", manager, pki.NewHotTLS())
				if err != nil {
					t.Fatal(err)
				}

				issued, err := manager.Issue(context.Background(), other.csrPEM, pki.Identity{Kind: pki.ControlPlane, PodUID: string(pod.UID), BootID: "other"})
				if err != nil {
					t.Fatal(err)
				}

				cm.Data["certificate"] = string(issued.CertificatePEM)
			case "boot":
				cm.Data["certificate-boot"] = "other"
			case "digest":
				cm.Data["certificate-csr"] = "other"
			}

			if err := kube.Update(context.Background(), &cm); err != nil {
				t.Fatal(err)
			}

			if err := r.reconcileLocal(context.Background()); err == nil {
				t.Fatal("accepted mismatched certificate")
			}

			if r.installed != before {
				t.Fatal("replaced valid TLS context")
			}
		})
	}
}

func TestReplicaProofUsesInstalledState(t *testing.T) {
	_, _, _, r := replicaFixture(t)
	req := httptest.NewRequest(http.MethodGet, "https://replica/v3/replica-proof", nil)
	w := httptest.NewRecorder()
	r.serveProof(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unready proof status %d", w.Code)
	}

	replicaBootstrap(t, r)

	w = httptest.NewRecorder()
	r.serveProof(w, req)

	var ack replicaAcknowledgment
	if err := json.Unmarshal(w.Body.Bytes(), &ack); err != nil {
		t.Fatal(err)
	}

	if ack != r.installed {
		t.Fatal("proof response did not bind installed context")
	}

	r.expires = time.Now().Add(-time.Second)
	w = httptest.NewRecorder()
	r.serveProof(w, req)

	if w.Code != http.StatusServiceUnavailable || r.Ready(nil) == nil {
		t.Fatal("expired certificate remained ready")
	}
}

func TestReplicaInstalledHookPrecedesAcknowledgmentAndOnlyTracksChanges(t *testing.T) {
	_, pod, manager, r := replicaFixture(t)
	calls := 0
	drained := true

	r.SetInstalledHook(func() {
		calls++

		for _, hot := range []*pki.HotTLS{r.hot, r.proofHot} {
			if _, err := hot.ServerConfig(tls.NoClientCert).GetConfigForClient(&tls.ClientHelloInfo{}); err != nil {
				t.Errorf("hook preceded TLS installation: %v", err)
			}
		}

		drained = false
	})
	r.SetDrainedCheck(func() bool { return drained })
	replicaBootstrap(t, r)

	if calls != 1 || r.installed.OldConnectionsDrained {
		t.Fatal("initial hook/ack order incorrect")
	}

	drained = true

	if err := r.reconcileLocal(context.Background()); err != nil {
		t.Fatal(err)
	}

	if calls != 1 || !r.installed.OldConnectionsDrained {
		t.Fatal("unchanged poll rotated contexts or lost drain evidence")
	}

	ctx := context.Background()
	if err := manager.TriggerRotation(ctx); err != nil {
		t.Fatal(err)
	}

	bundle, err := manager.Bundle(ctx)
	if err != nil {
		t.Fatal(err)
	}

	cm := replicaGetMap(t, r)
	if err := r.issueReplica(ctx, pod, &cm, bundle); err != nil {
		t.Fatal(err)
	}

	if err := r.reconcileLocal(ctx); err != nil {
		t.Fatal(err)
	}

	if calls != 2 || r.installed.OldConnectionsDrained {
		t.Fatal("changed bundle did not drain before acknowledgment")
	}
	// Switching the main leaf to the probe issuer with the same overlap bundle
	// is also a transport change, even though the bundle digest is unchanged.
	cm = replicaGetMap(t, r)

	cm.Data["certificate"] = cm.Data["proof-certificate"]
	if err := r.kube.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}

	if err := r.reconcileLocal(ctx); err != nil {
		t.Fatal(err)
	}

	if calls != 3 {
		t.Fatal("main issuer change did not trigger drain hook")
	}

	cm = replicaGetMap(t, r)

	cm.Data["proof-certificate"] = "broken"
	if err := r.kube.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}

	if err := r.reconcileLocal(ctx); err == nil {
		t.Fatal("invalid proof context accepted")
	}

	if calls != 3 {
		t.Fatal("failed installation invoked drain hook")
	}
}

func TestReplicaInstalledHookPrecedesAcknowledgmentAndRunsOnlyOnChange(t *testing.T) {
	_, pod, manager, r := replicaFixture(t)
	calls := 0
	drained := true

	r.SetInstalledHook(func() {
		calls++

		for _, hot := range []*pki.HotTLS{r.hot, r.ProofTLS()} {
			if _, err := hot.ServerConfig(tls.NoClientCert).GetConfigForClient(&tls.ClientHelloInfo{}); err != nil {
				t.Fatalf("hook before TLS install: %v", err)
			}
		}

		drained = false
	})
	r.SetDrainedCheck(func() bool { return drained })
	replicaBootstrap(t, r)

	if calls != 1 || r.installed.OldConnectionsDrained {
		t.Fatal("initial hook ordering did not affect acknowledgment")
	}

	drained = true

	if err := r.reconcileLocal(context.Background()); err != nil {
		t.Fatal(err)
	}

	if calls != 1 || !r.installed.OldConnectionsDrained {
		t.Fatal("unchanged poll rotated connections or failed to refresh drain acknowledgment")
	}

	ctx := context.Background()
	if err := manager.TriggerRotation(ctx); err != nil {
		t.Fatal(err)
	}

	bundle, err := manager.Bundle(ctx)
	if err != nil {
		t.Fatal(err)
	}

	cm := replicaGetMap(t, r)
	if err := r.issueReplica(ctx, pod, &cm, bundle); err != nil {
		t.Fatal(err)
	}

	if err := r.reconcileLocal(ctx); err != nil {
		t.Fatal(err)
	}

	if calls != 2 || r.installed.OldConnectionsDrained {
		t.Fatal("changed bundle not drained before acknowledgment")
	}
	// A production issuer can change without another bundle publication.
	cm = replicaGetMap(t, r)

	cm.Data["certificate"] = cm.Data["proof-certificate"]
	if err := r.kube.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}

	if err := r.reconcileLocal(ctx); err != nil {
		t.Fatal(err)
	}

	if calls != 3 {
		t.Fatal("changed production issuer did not trigger hook")
	}

	cm = replicaGetMap(t, r)

	cm.Data["proof-certificate"] = "invalid"
	if err := r.kube.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}

	if err := r.reconcileLocal(ctx); err == nil {
		t.Fatal("invalid context accepted")
	}

	if calls != 3 {
		t.Fatal("failed installation triggered hook")
	}
}

func TestReplicaFreshTLSProofPinsBootAndKey(t *testing.T) {
	_, pod, manager, r := replicaFixture(t)
	replicaBootstrap(t, r)
	replicaServeTestProof(t, r)

	ctx := context.Background()

	proof, err := r.probe(ctx, pod, r.installed, string(r.csrPEM))
	if err != nil {
		t.Fatal(err)
	}

	key := pki.MemberKey{PodUID: string(pod.UID), BootID: r.bootID}
	if err := manager.RecordTLSProof(ctx, key, proof); err != nil {
		t.Fatal(err)
	}

	if err := manager.RecordTLSProof(ctx, key, proof); err == nil {
		t.Fatal("replayed proof accepted")
	}

	wrong := r.installed

	wrong.BootID = "other-boot"
	if _, err := r.probe(ctx, pod, wrong, string(r.csrPEM)); err == nil {
		t.Fatal("proof accepted wrong boot")
	}

	wrong = r.installed

	wrong.Digest = strings.Repeat("0", 64)
	if _, err := r.probe(ctx, pod, wrong, string(r.csrPEM)); err == nil {
		t.Fatal("proof accepted wrong bundle digest")
	}

	other, err := newReplicaTLS(r.kube, r.namespace, r.podName, r.podUID, "other-key", manager, pki.NewHotTLS())
	if err != nil {
		t.Fatal(err)
	}

	wrong = r.installed

	wrong.CSR = replicaDigest(other.csrPEM)
	if _, err := r.probe(ctx, pod, wrong, string(other.csrPEM)); err == nil {
		t.Fatal("proof accepted different boot key")
	}
}

func replicaServeTestProof(t *testing.T, r *replicaTLS) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	_, r.proofPort, err = net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	server := &http.Server{Handler: http.HandlerFunc(r.serveProof), ReadHeaderTimeout: time.Second}
	server.SetKeepAlivesEnabled(false)

	done := make(chan error, 1)

	go func() { done <- server.Serve(tls.NewListener(listener, r.ProofTLS().ServerConfig(tls.NoClientCert))) }()

	t.Cleanup(func() { _ = server.Close(); <-done })
}

func TestReplicaOverlapRequiresInstalledPendingRootAndFreshProof(t *testing.T) {
	_, pod, manager, r := replicaFixture(t)
	replicaBootstrap(t, r)
	replicaServeTestProof(t, r)

	ctx := context.Background()

	oldBundle, err := manager.Bundle(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.TriggerRotation(ctx); err != nil {
		t.Fatal(err)
	}

	overlap, err := manager.Bundle(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if overlap.Active != oldBundle.Active || overlap.Generation == oldBundle.Generation {
		t.Fatal("rotation did not publish overlap before switch")
	}
	// A dishonest exact-generation claim on an old-root TLS session cannot
	// bypass the requirement to install and serve the pending-root leaf.
	cm := replicaGetMap(t, r)
	if err := r.proofHot.Update(overlap.JSON(), []byte(cm.Data["proof-certificate"]), r.keyPEM); err != nil {
		t.Fatal(err)
	}

	r.mu.Lock()
	r.installed.Generation, r.installed.Digest = overlap.Generation, overlap.Digest()
	r.mu.Unlock()

	proof, err := r.probe(ctx, pod, r.installed, string(r.csrPEM))
	if err != nil {
		t.Fatal(err)
	}

	key := pki.MemberKey{PodUID: string(pod.UID), BootID: r.bootID}
	if err := manager.RecordTLSProof(ctx, key, proof); err == nil {
		t.Fatal("old-root proof unlocked overlap")
	}

	if err := r.issueReplica(ctx, pod, &cm, overlap); err != nil {
		t.Fatal(err)
	}

	if cm.Data["certificate-root"] != oldBundle.Active || cm.Data["proof-certificate-root"] == oldBundle.Active {
		t.Fatal("production and probe issuers not separated during overlap")
	}

	if err := r.reconcileLocal(ctx); err != nil {
		t.Fatal(err)
	}

	if err := r.ReconcileLeader(ctx); err != nil {
		t.Fatal(err)
	}

	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	switched, err := manager.Bundle(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if switched.Active == oldBundle.Active {
		t.Fatal("fresh pending-root proof did not switch issuer")
	}
}

func TestReplicaLeaderAdmitsUnregisteredPodsAndRetiresOnlyAbsentPods(t *testing.T) {
	kube, pod, manager, r := replicaFixture(t)

	ctx := context.Background()
	if err := manager.AcquireLeadership(ctx, "leader-one"); err != nil {
		t.Fatal(err)
	}

	if err := r.ReconcileLeader(ctx); err != nil {
		t.Fatal(err)
	}

	members, err := manager.Members(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(members) != 1 || members[0].PodUID != string(pod.UID) || members[0].BootID != "pending" {
		t.Fatalf("unregistered Pod missing from durable barrier: %+v", members)
	}

	old := pki.Identity{Kind: pki.ControlPlane, PodUID: string(pod.UID), BootID: "old-boot"}
	if err := manager.Admit(ctx, old); err != nil {
		t.Fatal(err)
	}
	// Loss of labels is not proof of retirement, even if this Pod no longer
	// passes enrollment validation. Nor is missing CSR or heartbeat evidence.
	pod.Labels = nil
	if err := kube.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}

	if err := r.ReconcileLeader(ctx); err != nil {
		t.Fatal(err)
	}

	members, err = manager.Members(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(members) != 2 {
		t.Fatalf("present Pod was retired: %+v", members)
	}
	// A new leader must see the same durable obligations.
	next, err := pki.New(kube, pod.Namespace, pki.Options{})
	if err != nil {
		t.Fatal(err)
	}

	if err := next.AcquireLeadership(ctx, "leader-two"); err != nil {
		t.Fatal(err)
	}

	r.manager = next
	if err := r.ReconcileLeader(ctx); err != nil {
		t.Fatal(err)
	}

	members, err = next.Members(ctx)
	if err != nil || len(members) != 2 {
		t.Fatalf("takeover lost members: %+v, %v", members, err)
	}

	if err := kube.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}

	if err := r.ReconcileLeader(ctx); err != nil {
		t.Fatal(err)
	}

	members, err = next.Members(ctx)
	if err != nil || len(members) != 0 {
		t.Fatalf("absent Pod not retired: %+v, %v", members, err)
	}
}
