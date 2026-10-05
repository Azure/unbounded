// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/unbounded/internal/racer/members"
)

func workloadConfig(t *testing.T) members.Config {
	t.Helper()

	return members.Config{
		Cluster: "11111111-1111-1111-1111-111111111111", Namespace: "racer",
		ControlURL: "https://racer-controller.racer.svc:8443", DataplaneImage: "racer:test",
		BootstrapTrustConfigMap: "racer-bootstrap-trust", PeerPort: 8082,
		DataplaneServiceAccount: "racer-dataplane", DaemonSetName: "racer-dataplane",
	}
}

func TestWorkloadPeerMembership(t *testing.T) {
	for _, port := range []uint16{8082, 7443, 9090, 9091, 65535} {
		t.Run(strconv.Itoa(int(port)), func(t *testing.T) {
			cfg := workloadConfig(t)
			cfg.PeerPort = port

			ds, err := members.DesiredDaemonSet(cfg)
			if err != nil {
				t.Fatal(err)
			}

			assertWorkloadPeerMembership(t, ds, port)
		})
	}
}

func TestWorkloadDefaultsAgreeWithController(t *testing.T) {
	values := map[string]string{
		"RACER_CLUSTER_ID":  "11111111-1111-1111-1111-111111111111",
		"RACER_CONTROL_URL": "https://controller:8443", "RACER_DATAPLANE_IMAGE": "racer:test",
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }

	cfg, err := members.ConfigFromLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}

	runtime, err := ConfigFromLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Namespace != runtime.Namespace || cfg.PeerPort != runtime.PeerPort || cfg.DaemonSetName != runtime.DaemonSetName || cfg.DataplaneServiceAccount != runtime.DataplaneServiceAccount || cfg.BootstrapTrustConfigMap != "racer-bootstrap-trust" {
		t.Fatalf("workload defaults disagree with runtime: %+v", cfg)
	}
}

// Exercise the builder's ordered downward-API expansion and membership contract together.
func assertWorkloadPeerMembership(t *testing.T, ds *appsv1.DaemonSet, peerPort uint16) {
	t.Helper()

	for _, ips := range [][]string{{"192.0.2.1"}, {"2001:db8::1"}, {"192.0.2.1", "2001:db8::1"}, {"2001:db8::1", "192.0.2.1"}} {
		pod := memberPod("peer", 1, ips[0])
		for _, ip := range ips {
			pod.Status.PodIPs = append(pod.Status.PodIPs, corev1.PodIP{IP: ip})
		}

		podIP, listen := "", ""

		for _, env := range ds.Spec.Template.Spec.Containers[0].Env {
			switch env.Name {
			case "RACER_POD_IP":
				if podIP != "" || env.Value != "" || !reflect.DeepEqual(env.ValueFrom, &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}) {
					t.Fatal("peer bind address must come from status.podIP")
				}

				podIP = pod.Status.PodIP
			case "RACER_PEER_LISTEN":
				if podIP == "" || listen != "" || env.ValueFrom != nil || env.Value != "[$(RACER_POD_IP)]:"+strconv.Itoa(int(peerPort)) {
					t.Fatal("peer listener must expand the preceding Pod IP and configured peer port")
				}

				listen = strings.ReplaceAll(env.Value, "$(RACER_POD_IP)", podIP)
			}
		}

		host, port, err := net.SplitHostPort(listen)
		if err != nil {
			t.Fatalf("expanded peer listener %q: %v", listen, err)
		}

		ip, err := netip.ParseAddr(host)
		if err != nil || ip.IsUnspecified() || port != strconv.Itoa(int(peerPort)) {
			t.Fatalf("peer listener must bind the exact Pod IP and peer port: %q", listen)
		}

		candidate, diagnostics, err := reconcileMembers([]corev1.Node{memberNode()}, map[string][]corev1.Pod{pod.Spec.NodeName: {pod}}, memberOwnership(t, testDaemonSetUID), nil, peerPort)
		if err != nil || len(diagnostics) != 0 || len(candidate) != 1 {
			t.Fatalf("unready Pod with IPs %v must be published: %v, %v", ips, diagnostics, err)
		}

		if endpoint := candidate[testNodeUID].PeerEndpoint; endpoint != netip.AddrPortFrom(ip, peerPort).String() {
			t.Fatalf("membership endpoint %q disagrees with listener %q for Pod IPs %v", endpoint, listen, ips)
		}
	}
}
