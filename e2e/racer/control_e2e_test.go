//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// TestControlPlaneSeatsAndFailover checks the control plane embedded in the
// racer-dataplane DaemonSet: no standalone controller is installed, enough
// pods on distinct nodes hold unexpired seats, the primary holds a seat, the
// state Secret and webhook wiring exist, and deleting the primary pod moves
// the primary to another seat. Every dataplane must report the same fully
// applied goal state through /debug/membership before and after failover,
// and again after all dataplane pods are deleted at once, without the goal
// state moving backward or the racer-state Secret being recreated. The election
// checks need no Azure egress; the ClusterCache only triggers the install. The
// object-read-after-failover subtest also reads a blob through the sidecar and
// needs egress.
func TestControlPlaneSeatsAndFailover(t *testing.T) {
	h := cluster(t)

	ns := "fetch-failover"

	t.Cleanup(func() { h.dumpDiagnostics(ns) })

	h.applyCache()
	h.installRacer()

	for _, kind := range []string{"deployment", "service", "serviceaccount"} {
		out, err := h.exec(time.Minute, nil, "kubectl", "-n", operatorNamespace, "get", kind,
			"racer-controller", "--ignore-not-found", "-o", "name")
		if err != nil || strings.TrimSpace(out) != "" {
			t.Fatalf("%s/racer-controller must not exist: err=%v out=%q", kind, err, out)
		}
	}

	for i := range maxSeats {
		h.waitExists("lease", fmt.Sprintf("racer-seat-%d", i), time.Minute)
	}

	before := h.waitHealthy("seats for ready dataplane pods")
	if n := len(before.validSeats()); n > maxSeats {
		t.Fatalf("got %d valid seats, want at most %d", n, maxSeats)
	}

	stateUID, stateRV := h.waitStateSecret()
	h.waitWebhook()

	admissionNS := "racer-admission-failover"
	h.apply("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + admissionNS + "\n")
	h.assertWebhookAdmission(admissionNS)

	goalBefore, _ := h.waitConverged("all dataplanes to apply one goal state")
	t.Logf("goal state before failover: %s", goalBefore.accepted())

	oldPrimary, _ := before.primarySeat()
	t.Logf("deleting primary pod %s; %s", oldPrimary.Name, before)
	h.kubectl("-n", operatorNamespace, "delete", "pod", oldPrimary.Name, "--wait=false")

	var after controlView

	h.waitFor("primary to move to another valid seat", 5*time.Minute, func() (bool, string) {
		after = h.controlView()
		p, ok := after.primarySeat()

		return ok && p.UID != oldPrimary.UID && len(after.seatProblems()) == 0, after.String()
	})

	if after.Primary.Transitions <= before.Primary.Transitions {
		t.Fatalf("racer-primary leaseTransitions did not advance: before=%d after=%d",
			before.Primary.Transitions, after.Primary.Transitions)
	}

	// resourceVersion is opaque, so only equality is meaningful: the new
	// primary must write the state Secret, for example to fence the old
	// primary, without recreating it.
	h.waitFor("racer-state Secret written after failover", 5*time.Minute, func() (bool, string) {
		uid, rv := h.waitStateSecret()
		if uid != stateUID {
			t.Fatalf("racer-state Secret UID changed across failover: %s -> %s", stateUID, uid)
		}

		return rv != stateRV, "resourceVersion unchanged"
	})

	h.rollout("daemonset", "racer-dataplane", 10*time.Minute)
	h.waitHealthy("seats after failover")
	h.waitWebhook()
	h.assertWebhookAdmission(admissionNS)

	goalFailover, _ := h.waitConverged("all dataplanes to converge after failover")
	if err := checkMembershipAdvance(goalBefore, goalFailover); err != nil {
		t.Fatalf("goal state across failover: %v", err)
	}

	h.restartAllDataplanes(stateUID, goalFailover)
	h.assertWebhookAdmission(admissionNS)

	t.Run("object-read-after-failover", func(t *testing.T) {
		requireAzureEgress(t, containerURL+"/"+blobKey)

		child := h.forTest(t)
		child.rollout("daemonset", "racer-origin-"+cacheName, 5*time.Minute)
		child.kubectl("create", "namespace", ns)
		child.apply(fetchPod(ns, "s3"))
		child.waitFetch(ns)
	})
}

// restartAllDataplanes deletes every racer-dataplane pod at once, so no
// process that held issuer keys in memory survives, then waits for
// replacements to elect, become ready, and converge on a goal state no older
// than floor from the same racer-state Secret. Nodes are left alone.
func (h *harness) restartAllDataplanes(stateUID string, floor membership) {
	h.t.Helper()

	old := map[string]bool{}
	for _, p := range h.controlView().Pods {
		old[p.UID] = true
	}

	h.t.Logf("deleting all %d racer-dataplane pods", len(old))
	h.kubectl("-n", operatorNamespace, "delete", "pod", "-l", dataplaneLabel, "--wait=false")

	h.waitFor("every racer-dataplane pod replaced", 5*time.Minute, func() (bool, string) {
		v := h.controlView()
		desired := h.dataplaneDesired()

		for _, p := range v.Pods {
			if old[p.UID] {
				return false, "old pod " + p.Name + " still live"
			}
		}

		return desired > 0 && len(v.Pods) == desired && v.readyPods() == desired,
			fmt.Sprintf("ready new pods %d/%d of %d desired", v.readyPods(), len(v.Pods), desired)
	})

	h.rollout("daemonset", "racer-dataplane", 10*time.Minute)
	h.waitHealthy("election after restarting every dataplane")

	goal, v := h.waitConverged("all dataplanes to converge after full restart")
	for _, p := range v.Pods {
		if old[p.UID] {
			h.t.Fatalf("pre-restart pod %s reported the converged goal state", p.Name)
		}
	}

	if err := checkMembershipAdvance(floor, goal); err != nil {
		h.t.Fatalf("goal state across full restart: %v", err)
	}

	if uid, _ := h.waitStateSecret(); uid != stateUID {
		h.t.Fatalf("racer-state Secret UID changed across full restart: %s -> %s", stateUID, uid)
	}

	h.waitWebhook()
}

// assertWebhookAdmission exercises API-server TLS verification and injection
// without starting a pod or reading an origin. It also runs after failover.
func (h *harness) assertWebhookAdmission(namespace string) {
	h.t.Helper()

	for _, api := range []string{"s3", "azure"} {
		h.waitFor(api+" admission through elected seats", time.Minute, func() (bool, string) {
			out, err := h.exec(time.Minute, []byte(fetchPod(namespace, api)),
				"kubectl", "create", "--dry-run=server", "-f", "-", "-o", "json")
			if err != nil {
				return false, fmt.Sprintf("admission failed: %v: %s", err, out)
			}

			var pod corev1.Pod
			if err := json.Unmarshal([]byte(out), &pod); err != nil {
				return false, "admitted Pod could not be decoded"
			}

			for _, c := range pod.Spec.InitContainers {
				if c.Name == "racer-object" && c.RestartPolicy != nil &&
					*c.RestartPolicy == corev1.ContainerRestartPolicyAlways &&
					pod.Annotations["racer.unbounded-cloud.io/object-sidecar-injected"] == c.Image {
					return true, "sidecar injected"
				}
			}

			return false, "admission did not inject the native sidecar"
		})
	}
}

// waitHealthy waits for a seated primary and enough valid seats on distinct
// pods and nodes, and returns that view.
func (h *harness) waitHealthy(what string) controlView {
	h.t.Helper()

	var v controlView

	h.waitFor(what, 5*time.Minute, func() (bool, string) {
		v = h.controlView()

		return v.healthy(), v.String()
	})

	return v
}

// waitStateSecret waits for the racer-state Secret to hold data and returns
// its UID and resourceVersion. Only the key count is read, so Secret contents
// never reach logs or failure messages.
func (h *harness) waitStateSecret() (string, string) {
	h.t.Helper()

	var uid, rv string

	h.waitFor("racer-state Secret initialized", 5*time.Minute, func() (bool, string) {
		out, err := h.exec(time.Minute, nil, "kubectl", "-n", operatorNamespace, "get", "secret", racerStateSecret,
			"-o", `go-template={{.metadata.uid}} {{.metadata.resourceVersion}} {{len .data}}`)
		if err != nil {
			return false, "secret not readable"
		}

		f := strings.Fields(out)
		if len(f) != 3 {
			return false, "unexpected secret metadata"
		}

		uid, rv = f[0], f[1]

		return f[2] != "0", "keys=" + f[2]
	})

	return uid, rv
}

// waitWebhook waits for the selectorless racer-webhook Service to route only
// to valid seat holders on the webhook port, and for the injector to carry a
// caBundle.
func (h *harness) waitWebhook() {
	h.t.Helper()

	h.waitFor("racer-webhook endpoints on valid seats", 5*time.Minute, func() (bool, string) {
		sel, err := h.exec(time.Minute, nil, "kubectl", "-n", operatorNamespace, "get", "service", webhookName,
			"-o", "jsonpath={.spec.selector}")
		if err != nil {
			return false, "service not readable"
		}

		if strings.TrimSpace(sel) != "" {
			h.t.Fatalf("service/%s must be selectorless, got selector %s", webhookName, sel)
		}

		var slice struct {
			Ports []struct {
				Port *int32
			}
			Endpoints []struct {
				Addresses  []string
				Conditions struct{ Ready *bool }
			}
		}
		if !h.getJSON(&slice, "-n", operatorNamespace, "get", "endpointslice", webhookName) {
			return false, "endpointslice not readable"
		}

		portOK := false

		for _, p := range slice.Ports {
			if p.Port != nil && *p.Port == webhookPort {
				portOK = true
			}
		}

		seats := h.controlView().seatIPs()
		ready := 0

		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}

			for _, addr := range ep.Addresses {
				if !seats[addr] {
					return false, "ready endpoint " + addr + " is not a valid seat holder"
				}

				ready++
			}
		}

		return portOK && ready > 0, fmt.Sprintf("port%d=%t readyEndpoints=%d", webhookPort, portOK, ready)
	})
	h.waitFor("sidecar injector caBundle", 5*time.Minute, func() (bool, string) {
		out, err := h.exec(time.Minute, nil, "kubectl", "get", "mutatingwebhookconfiguration",
			"racer-object-sidecar-injector", "-o", "jsonpath={.webhooks[0].clientConfig.caBundle}")

		return err == nil && strings.TrimSpace(out) != "", "caBundle empty or unreadable"
	})
}
