// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRacerTargets(t *testing.T) {
	requireBash(t)
	t.Parallel()

	for _, tc := range []struct {
		name, sites, account, deployment string
		want                             []string
		fail                             bool
	}{
		{name: "absent", sites: `{"items":[]}`},
		{name: "not explicitly enabled", sites: `{"items":[{}, {"spec":{"components":{"racer":{}}}}, {"spec":{"components":{"racer":{"enabled":false}}}}]}`},
		{name: "retained account", sites: `{"items":[]}`, account: `{}`, want: []string{"deploy/racer-controlplane"}},
		{name: "retained deployment", sites: `{"items":[]}`, deployment: `{}`, want: []string{"deploy/racer-controlplane"}},
		{name: "enabled before creation", sites: `{"items":[{"metadata":{"name":"edge"},"spec":{"components":{"racer":{"enabled":true}}}}]}`, want: []string{"deploy/racer-controlplane", "ds/racer-edge"}},
		{name: "malformed sites", sites: `{}`, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.set("getjson-sites.unbounded-cloud.io", replyOf(tc.sites))
			f.set("getjson-serviceaccount_racer-controlplane", replyOf(tc.account))
			f.set("getjson-deploy_racer-controlplane", replyOf(tc.deployment))

			output, code := f.runScript("racer-targets.sh", nil)
			if tc.fail {
				if code == 0 {
					t.Fatalf("unexpected success: %s", output)
				}

				return
			}

			requireCode(t, code, 0, output)

			var targets []string

			for _, line := range strings.Split(output, "\n") {
				if strings.HasPrefix(line, "deploy/") || strings.HasPrefix(line, "ds/") {
					targets = append(targets, line)
				}
			}

			if strings.Join(targets, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("targets = %v, want %v", targets, tc.want)
			}
		})
	}
}

func TestRacerTargetNameEncodingAndQueryFailures(t *testing.T) {
	requireBash(t)
	t.Parallel()

	for _, name := range []string{"edge.a", strings.Repeat("a", 49), strings.Repeat("a", 57), strings.Repeat("a", 58), "edge-1"} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.set("getjson-sites.unbounded-cloud.io", replyOf(fmt.Sprintf(`{"items":[{"metadata":{"name":%q},"spec":{"components":{"racer":{"enabled":true}}}}]}`, name)))
			output, code := f.runScript("racer-targets.sh", nil)
			requireCode(t, code, 0, output)

			want := "ds/racer-" + name
			if strings.Contains(name, ".") || len(want)-3 > 63 {
				digest := sha256.Sum256([]byte(name))
				want = fmt.Sprintf("ds/racer-site.%x", digest[:16])
			}

			requireContains(t, output, want)
		})
	}

	for _, key := range []string{"sites.unbounded-cloud.io", "serviceaccount_racer-controlplane", "deploy_racer-controlplane"} {
		t.Run(key, func(t *testing.T) {
			f := newFake(t)
			f.set("getjson-sites.unbounded-cloud.io", replyOf(`{"items":[]}`))
			f.set("getjson-"+key, reply{exit: 1, stderr: "Forbidden"})
			output, code := f.runScript("racer-targets.sh", nil)
			requireCode(t, code, 1, output)
			requireNotContains(t, output, "no rollout targets")
		})
	}
}

func racerPod(t *testing.T, name, ready string) map[string]any {
	t.Helper()

	var p map[string]any

	err := json.Unmarshal([]byte(fmt.Sprintf(`{
	  "metadata":{"name":%q,"labels":{"racer.unbounded-cloud.io/component":"racer-controlplane"},
	    "ownerReferences":[{"uid":"rs","kind":"ReplicaSet","controller":true}]},
	  "spec":{"nodeName":"node-a","serviceAccountName":"racer-controlplane","containers":[{
	    "name":"controller","image":"ghcr.io/azure/racer-controlplane:nightly-abc",
	    "ports":[{"name":"subscription","containerPort":8443},{"name":"health","containerPort":8081}],
	    "readinessProbe":{"httpGet":{"path":"/readyz","port":"health"}},
	    "livenessProbe":{"httpGet":{"path":"/healthz","port":"health"}}
	  }]},
	  "status":{"phase":"Running","conditions":[{"type":"Ready","status":%q},
	    {"type":"Initialized","status":"True"},{"type":"PodScheduled","status":"True"}],
	    "containerStatuses":[{"name":"controller","started":true,"ready":false,"state":{"running":{}}}]}
	}`, name, ready)), &p)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func racerFixture(t *testing.T, mutate func(map[string]any)) *fake {
	t.Helper()
	f := newFake(t)

	leader, standby := racerPod(t, "leader", "True"), racerPod(t, "standby", "False")
	if mutate != nil {
		mutate(standby)
	}

	f.set("get-namespace", replyOf("namespace/unbounded-system"))
	f.set("getjson-nodes", replyOf(smokeNodes()))
	f.set("pods", replyOf(marshal(map[string]any{"items": []any{leader, standby}})))
	f.set("getjson-replicasets", replyOf(`{"items":[{"metadata":{"uid":"rs","ownerReferences":[{"kind":"Deployment","uid":"deployment","controller":true}]}}]}`))
	f.set("getjson-deploy_racer-controlplane", replyOf(marshal(map[string]any{
		"metadata": map[string]any{"uid": "deployment", "generation": 2},
		"spec":     map[string]any{"replicas": 2, "template": map[string]any{"spec": leader["spec"]}},
		"status":   map[string]any{"observedGeneration": 2, "replicas": 2, "updatedReplicas": 2, "readyReplicas": 1},
	})))

	return f
}

func TestRacerStandbyHealth(t *testing.T) {
	requireBash(t)
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		fail   bool
	}{
		{name: "healthy standby"},
		{name: "crash loop", fail: true, mutate: func(p map[string]any) {
			p["status"].(map[string]any)["containerStatuses"] = []any{map[string]any{"name": "controller", "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}}}
		}},
		{name: "missing status", fail: true, mutate: func(p map[string]any) { delete(p["status"].(map[string]any), "containerStatuses") }},
		{name: "pending", fail: true, mutate: func(p map[string]any) { p["status"].(map[string]any)["phase"] = "Pending" }},
		{name: "terminating", fail: true, mutate: func(p map[string]any) { p["metadata"].(map[string]any)["deletionTimestamp"] = "now" }},
		{name: "foreign owner with matching labels", fail: true, mutate: func(p map[string]any) { delete(p["metadata"].(map[string]any), "ownerReferences") }},
		{name: "arbitrary unready pod", fail: true, mutate: func(p map[string]any) { delete(p["metadata"].(map[string]any), "labels") }},
		{name: "unready sidecar", fail: true, mutate: func(p map[string]any) {
			spec := p["spec"].(map[string]any)
			spec["containers"] = append(spec["containers"].([]any), map[string]any{"name": "sidecar"})
		}},
		{name: "extra readiness gate", fail: true, mutate: func(p map[string]any) {
			p["spec"].(map[string]any)["readinessGates"] = []any{map[string]any{"conditionType": "extra"}}
		}},
		{name: "wrong readiness port", fail: true, mutate: func(p map[string]any) {
			p["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["readinessProbe"] = map[string]any{"httpGet": map[string]any{"path": "/readyz", "port": "subscription"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := racerFixture(t, tc.mutate)
			output, code := f.runScript("smoke/core-namespaces-ready.sh", map[string]string{"TAG": releaseTag})

			want := 0
			if tc.fail {
				want = 1
			}

			requireCode(t, code, want, output)

			if !tc.fail {
				requireContains(t, output, "healthy Racer standby")
				requireContains(t, f.calls(), "pods/http:leader:8081/proxy/readyz")
				requireContains(t, f.calls(), "pods/http:leader:8081/proxy/healthz")
				requireContains(t, f.calls(), "pods/http:standby:8081/proxy/healthz")
				requireNotContains(t, f.calls(), "pods/http:standby:8081/proxy/readyz")
				requireNotContains(t, f.calls(), "services/http:")
			}
		})
	}
}

func TestRacerRolloutAndLiveProbeFailures(t *testing.T) {
	requireBash(t)
	t.Parallel()

	for _, mode := range []string{"smoke", "rollout"} {
		for _, failure := range []string{"", "racer-readyz", "racer-healthz", "getjson-replicasets"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				f := racerFixture(t, nil)
				if failure != "" {
					f.set(failure, reply{exit: 1, stderr: "unavailable"})
				}

				env := racerRolloutEnv()

				var (
					output string
					code   int
				)

				if mode == "smoke" {
					output, code = f.runScript("smoke/core-namespaces-ready.sh", env)
				} else {
					output, code = f.run(env, "deploy/racer-controlplane")
				}

				want := 0
				if failure != "" {
					want = 1
				}

				requireCode(t, code, want, output)
				requireNotContains(t, f.calls(), "rollout status")
			})
		}
	}
}

func TestRacerRolloutRejectsStalePodsAndBootstrap(t *testing.T) {
	requireBash(t)
	t.Parallel()

	for _, init := range []bool{false, true} {
		t.Run(fmt.Sprint(init), func(t *testing.T) {
			f := racerFixture(t, func(p map[string]any) {
				spec := p["spec"].(map[string]any)
				if init {
					spec["initContainers"] = []any{map[string]any{"name": "bootstrap", "image": "ghcr.io/azure/racer-controlplane:old"}}
					p["status"].(map[string]any)["initContainerStatuses"] = []any{map[string]any{"name": "bootstrap", "state": map[string]any{"terminated": map[string]any{"exitCode": 0}}}}
				} else {
					spec["containers"].([]any)[0].(map[string]any)["image"] = "ghcr.io/azure/racer-controlplane:old"
				}
			})
			output, code := f.run(racerRolloutEnv(), "deploy/racer-controlplane")
			requireCode(t, code, 1, output)
			requireNotContains(t, f.calls(), "rollout status")
		})
	}
}

func racerRolloutEnv() map[string]string {
	return map[string]string{
		"TAG":                           releaseTag,
		"EXPECTED_IMAGE_TAG":            releaseTag,
		"EXPECTED_IMAGE_REGISTRY":       releaseRegistry,
		"RACER_ROLLOUT_TIMEOUT_SECONDS": "0",
	}
}

func TestRacerDataplaneBootstrapVersionGate(t *testing.T) {
	requireBash(t)
	t.Parallel()
	f := newFake(t)
	target := "ds/racer-edge"
	current := workloadWithInit("racer.unbounded-cloud.io/universe=edge", releaseRegistry+"/racer-dataplane:"+releaseTag, releaseRegistry+"/racer-controlplane:"+releaseTag)
	stale := strings.ReplaceAll(current, "/racer-controlplane:"+releaseTag, "/racer-controlplane:"+previousTag)
	f.set("getjson-ds_racer-edge", replyOf(current))
	f.setNth("getjson-ds_racer-edge", 1, replyOf(stale))
	f.set("pods", replyOf(`{"items":[]}`))
	output, code := f.run(racerRolloutEnv(), target)
	requireCode(t, code, 0, output)
	requireContains(t, output, "does not reference :"+releaseTag+" yet")
	requireContains(t, f.calls(), "rollout status ds/racer-edge")
}

func TestRacerRolloutRequiresReplacementAndLeader(t *testing.T) {
	requireBash(t)
	t.Parallel()

	for _, tc := range []struct {
		name, key, payload string
	}{
		{"no leader", "pods", marshal(map[string]any{"items": []any{racerPod(t, "one", "False"), racerPod(t, "two", "False")}})},
		{"missing replica", "pods", marshal(map[string]any{"items": []any{racerPod(t, "one", "True")}})},
		{"old replica still present", "pods", marshal(map[string]any{"items": []any{racerPod(t, "one", "True"), racerPod(t, "two", "False"), racerPod(t, "old", "False")}})},
		{"foreign replicaset", "getjson-replicasets", `{"items":[{"metadata":{"uid":"rs","ownerReferences":[{"kind":"Deployment","uid":"foreign","controller":true}]}}]}`},
		{"malformed pod list", "pods", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := racerFixture(t, nil)
			f.set(tc.key, replyOf(tc.payload))
			output, code := f.run(racerRolloutEnv(), "deploy/racer-controlplane")
			requireCode(t, code, 1, output)
			requireNotContains(t, f.calls(), "rollout status")
		})
	}
}

func TestRacerRolloutWaitsForObservedReplacement(t *testing.T) {
	requireBash(t)
	t.Parallel()

	for _, field := range []string{"observedGeneration", "updatedReplicas", "replicas"} {
		t.Run(field, func(t *testing.T) {
			f := racerFixture(t, nil)
			leader := racerPod(t, "leader", "True")
			status := map[string]any{"observedGeneration": 2, "updatedReplicas": 2, "replicas": 2}
			status[field] = 1
			f.setNth("getjson-deploy_racer-controlplane", 2, replyOf(marshal(map[string]any{
				"metadata": map[string]any{"uid": "deployment", "generation": 2},
				"spec":     map[string]any{"replicas": 2, "template": map[string]any{"spec": leader["spec"]}},
				"status":   status,
			})))
			output, code := f.run(racerRolloutEnv(), "deploy/racer-controlplane")
			requireCode(t, code, 1, output)
		})
	}
}
