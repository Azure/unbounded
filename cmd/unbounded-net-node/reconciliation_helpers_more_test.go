// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	unboundednetv1alpha1 "github.com/Azure/unbounded-kube/internal/net/apis/unboundednet/v1alpha1"
	"github.com/Azure/unbounded-kube/internal/net/healthcheck"
)

// TestParseGatewayPoolPeering tests ParseGatewayPoolPeering.
func TestParseGatewayPoolPeering(t *testing.T) {
	src := &unboundednetv1alpha1.GatewayPoolPeering{
		ObjectMeta: metav1.ObjectMeta{Name: "peer-a"},
		Spec: unboundednetv1alpha1.GatewayPoolPeeringSpec{
			GatewayPools: []string{"pool-a", "pool-b"},
		},
	}

	data, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal gateway pool peering: %v", err)
	}

	obj := make(map[string]interface{})
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("unmarshal gateway pool peering map: %v", err)
	}

	parsed, err := parseGatewayPoolPeering(&unstructured.Unstructured{Object: obj})
	if err != nil {
		t.Fatalf("parseGatewayPoolPeering returned error: %v", err)
	}

	if parsed.Name != "peer-a" || len(parsed.Spec.GatewayPools) != 2 {
		t.Fatalf("unexpected parsed peering: %#v", parsed)
	}
}

// TestParseSiteGatewayPoolAssignmentAndParseErrors tests ParseSiteGatewayPoolAssignmentAndParseErrors.
func TestParseSiteGatewayPoolAssignmentAndParseErrors(t *testing.T) {
	assignment := &unboundednetv1alpha1.SiteGatewayPoolAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: "asg-a"},
		Spec: unboundednetv1alpha1.SiteGatewayPoolAssignmentSpec{
			Sites:        []string{"site-a"},
			GatewayPools: []string{"pool-a"},
		},
	}

	data, err := json.Marshal(assignment)
	if err != nil {
		t.Fatalf("marshal assignment: %v", err)
	}

	obj := make(map[string]interface{})
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatalf("unmarshal assignment map: %v", err)
	}

	parsed, err := parseSiteGatewayPoolAssignment(&unstructured.Unstructured{Object: obj})
	if err != nil {
		t.Fatalf("parseSiteGatewayPoolAssignment returned error: %v", err)
	}

	if parsed.Name != "asg-a" || len(parsed.Spec.GatewayPools) != 1 {
		t.Fatalf("unexpected parsed assignment: %#v", parsed)
	}

	bad := &unstructured.Unstructured{Object: map[string]interface{}{"spec": "not-a-map"}}
	if _, err := parseSitePeering(bad); err == nil {
		t.Fatalf("expected parseSitePeering to fail on invalid payload")
	}

	if _, err := parseSiteGatewayPoolAssignment(bad); err == nil {
		t.Fatalf("expected parseSiteGatewayPoolAssignment to fail on invalid payload")
	}

	if _, err := parseGatewayPoolPeering(bad); err == nil {
		t.Fatalf("expected parseGatewayPoolPeering to fail on invalid payload")
	}
}

// TestCopyStringIntMapAndNormalizeRouteDstForKey tests CopyStringIntMapAndNormalizeRouteDstForKey.
func TestCopyStringIntMapAndNormalizeRouteDstForKey(t *testing.T) {
	input := map[string]int{"a": 1, "b": 2}

	copyMap := copyStringIntMap(input)
	if !reflect.DeepEqual(copyMap, input) {
		t.Fatalf("copied map mismatch: got %#v want %#v", copyMap, input)
	}

	copyMap["a"] = 7

	if input["a"] != 1 {
		t.Fatalf("copy should not alias original map, got original=%#v", input)
	}
}

// TestStringMapAndBFDProfileMapHelpers tests StringMapAndBFDProfileMapHelpers.
func TestStringMapAndBFDProfileMapHelpers(t *testing.T) {
	stringMap := map[string]string{"a": "one", "b": "two"}

	stringMapCopy := copyStringMap(stringMap)
	if !reflect.DeepEqual(stringMapCopy, stringMap) {
		t.Fatalf("copyStringMap mismatch: got %#v want %#v", stringMapCopy, stringMap)
	}

	stringMapCopy["a"] = "changed"

	if stringMap["a"] != "one" {
		t.Fatalf("copyStringMap aliased source map: source=%#v", stringMap)
	}

	if !stringMapEqual(stringMap, map[string]string{"a": "one", "b": "two"}) {
		t.Fatalf("stringMapEqual should return true for equal maps")
	}

	if stringMapEqual(stringMap, map[string]string{"a": "one"}) {
		t.Fatalf("stringMapEqual should return false for different maps")
	}

	profiles := map[string]healthcheck.HealthCheckSettings{
		"p1": {DetectMultiplier: 3, ReceiveInterval: 300 * time.Millisecond, TransmitInterval: 300 * time.Millisecond},
	}

	profilesCopy := copyHealthCheckProfileMap(profiles)
	if !reflect.DeepEqual(profilesCopy, profiles) {
		t.Fatalf("copyHealthCheckProfileMap mismatch: got %#v want %#v", profilesCopy, profiles)
	}

	profile := profilesCopy["p1"]
	profile.ReceiveInterval = 999
	profilesCopy["p1"] = profile

	if profiles["p1"].ReceiveInterval != 300*time.Millisecond {
		t.Fatalf("copyHealthCheckProfileMap aliased source map: source=%#v", profiles)
	}

	if !healthCheckProfileMapEqual(profiles, map[string]healthcheck.HealthCheckSettings{"p1": {DetectMultiplier: 3, ReceiveInterval: 300 * time.Millisecond, TransmitInterval: 300 * time.Millisecond}}) {
		t.Fatalf("healthCheckProfileMapEqual should return true for equal maps")
	}

	if healthCheckProfileMapEqual(profiles, map[string]healthcheck.HealthCheckSettings{"p1": {DetectMultiplier: 3, ReceiveInterval: 301 * time.Millisecond, TransmitInterval: 300 * time.Millisecond}}) {
		t.Fatalf("healthCheckProfileMapEqual should return false for different maps")
	}
}
