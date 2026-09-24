//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane_test

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/unbounded/e2e/racer/fixture"
)

func (c *campaign) replicaClaims(r *replica) {
	c.t.Helper()
	c.await("replica certificate carries fresh signed Pod/boot claims", 30*time.Second, func() error {
		pod, err := c.kube.CoreV1().Pods(namespace).Get(c.ctx, r.pod.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		var (
			request struct {
				Boot string `json:"boot"`
			}
			response struct {
				Boot        string `json:"boot"`
				Certificate string `json:"certificate"`
			}
		)

		if err := json.Unmarshal([]byte(pod.Annotations[prefix+"pki-request"]), &request); err != nil {
			return err
		}

		if err := json.Unmarshal([]byte(pod.Annotations[prefix+"pki-response"]), &response); err != nil {
			return err
		}

		if request.Boot == r.previousBoot || response.Boot != request.Boot {
			return fmt.Errorf("waiting for fresh boot certificate")
		}

		block, _ := pem.Decode([]byte(response.Certificate))
		if block == nil {
			return fmt.Errorf("missing replica certificate")
		}

		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}

		if len(leaf.URIs) != 1 {
			return fmt.Errorf("invalid replica SAN count")
		}

		claims, err := fixture.ParseClaims(leaf.URIs[0].String())
		if err != nil {
			return err
		}

		if claims.Namespace != namespace || claims.Identity.Kind != "controlplane" || claims.Identity.PodUID != string(pod.UID) || claims.Identity.PodName != pod.Name || claims.Identity.BootID != request.Boot {
			return fmt.Errorf("replica claims do not bind current Pod and boot")
		}

		bundle, _, err := c.trust()
		if err != nil {
			return err
		}

		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM([]byte(bundle.Certificates))
		_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "racer-controlplane." + namespace + ".svc", KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})

		return err
	})
}

func (c *campaign) convergedRevision(after uint64, workers ...*dataplane) uint64 {
	c.t.Helper()

	var revision uint64

	c.await("dataplanes install newer common revision", 60*time.Second, func() error {
		revision = 0

		for _, d := range workers {
			var status localStatus
			if err := c.getJSON("http://"+d.metrics+"/status", &status); err != nil {
				return err
			}

			if !status.Ready || status.ActiveRevision <= after || (revision != 0 && revision != status.ActiveRevision) {
				return fmt.Errorf("%s ready=%v revision=%d, need >%d and common=%d", d.node.Name, status.Ready, status.ActiveRevision, after, revision)
			}

			revision = status.ActiveRevision
		}

		return nil
	})

	return revision
}

func (c *campaign) checkpoint() *corev1.ConfigMap {
	c.t.Helper()
	cm, err := c.kube.CoreV1().ConfigMaps(namespace).Get(c.ctx, "racer-runtime-revisions", metav1.GetOptions{})
	require(c.t, err)

	if len(cm.Data) != 3 || cm.Data["format"] != "1" || cm.Data["fence"] == "" || len(cm.Data["fence"]) > 320 || len(cm.BinaryData) != 0 {
		c.t.Fatalf("invalid fixed-size checkpoint: %+v", cm.Data)
	}

	return cm
}

func (c *campaign) checkNewReservation(before *corev1.ConfigMap) {
	c.t.Helper()
	after := c.checkpoint()
	oldHigh, err := strconv.ParseUint(before.Data["high-water"], 10, 64)
	require(c.t, err)
	newHigh, err := strconv.ParseUint(after.Data["high-water"], 10, 64)
	require(c.t, err)

	if after.UID != before.UID || newHigh <= oldHigh || after.Data["fence"] == before.Data["fence"] {
		c.t.Fatal("restart must reserve a newer range in the same checkpoint with a fresh fence")
	}

	_, err = c.kube.CoreV1().ConfigMaps(namespace).Update(c.ctx, before, metav1.UpdateOptions{})
	if !apierrors.IsConflict(err) {
		c.t.Fatalf("stale checkpoint CAS: %v", err)
	}

	c.footprint()
}

// Check payload sizes as well as names: a single object containing fleet-sized
// arrays is not constant state. Do not log private CA bytes on failure.
func (c *campaign) footprint() {
	c.t.Helper()
	cms, err := c.kube.CoreV1().ConfigMaps(namespace).List(c.ctx, metav1.ListOptions{})
	require(c.t, err)

	seen := map[string]bool{}

	for _, cm := range cms.Items {
		if cm.Name == "kube-root-ca.crt" {
			continue
		}

		if cm.Name != "racer-trust" && cm.Name != "racer-runtime-revisions" {
			c.t.Fatalf("unexpected durable ConfigMap %s", cm.Name)
		}

		seen[cm.Name] = true
		raw, err := json.Marshal(cm.Data)
		require(c.t, err)

		if len(raw) > 16*1024 || len(cm.BinaryData) != 0 {
			c.t.Fatalf("unbounded ConfigMap %s: %d bytes", cm.Name, len(raw))
		}
	}

	if len(seen) != 2 {
		c.t.Fatalf("missing fixed ConfigMaps: %v", seen)
	}

	secrets, err := c.kube.CoreV1().Secrets(namespace).List(c.ctx, metav1.ListOptions{})
	require(c.t, err)

	if len(secrets.Items) != 1 || secrets.Items[0].Name != "racer-ca" {
		c.t.Fatalf("expected one CA Secret, got %d Secrets", len(secrets.Items))
	}

	data := secrets.Items[0].Data
	if len(data) != 1 || len(data["state.json"]) > 16*1024 {
		c.t.Fatal("CA state exceeds its bounded single-key schema")
	}

	var state struct {
		Version        int               `json:"version"`
		Namespace      string            `json:"namespace"`
		Fence          string            `json:"fence"`
		Generation     uint64            `json:"generation"`
		Active         string            `json:"active"`
		Phase          string            `json:"phase"`
		Authorities    []json.RawMessage `json:"authorities"`
		RotationNonce  string            `json:"rotation_nonce"`
		PublishedAt    *int64            `json:"published_at"`
		OverlapDelay   int64             `json:"overlap_delay"`
		RetirementSkew int64             `json:"retirement_skew"`
	}

	decoder := json.NewDecoder(strings.NewReader(string(data["state.json"])))
	decoder.DisallowUnknownFields()

	if decoder.Decode(&state) != nil || state.Version != 1 || len(state.Authorities) < 1 || len(state.Authorities) > 2 {
		c.t.Fatal("CA state contains unsupported fields or authorities")
	}

	c.checkpoint()
	pods, err := c.kube.CoreV1().Pods(namespace).List(c.ctx, metav1.ListOptions{})
	require(c.t, err)

	for _, pod := range pods.Items {
		for key, value := range pod.Annotations {
			if strings.HasPrefix(key, prefix+"pki-") && ((key != prefix+"pki-request" && key != prefix+"pki-response") || len(value) > 16*1024) {
				c.t.Fatalf("unbounded replica annotation %s on %s", key, pod.Name)
			}
		}
	}
}

func (c *campaign) scaleFootprint(independent *dataplane) {
	c.t.Helper()

	var before localStatus
	require(c.t, c.getJSON("http://"+independent.metrics+"/status", &before))

	const count = 64

	workers := make([]*dataplane, 0, count)
	for i := 3; i < 3+count; i++ {
		workers = append(workers, c.worker(i, "independent"))
	}

	c.await("scaled inventory is selected without invented readiness", 90*time.Second, func() error { return c.cacheReady("independent", count+1, 1) })
	revision := c.convergedRevision(before.ActiveRevision, independent)
	c.footprint()

	zero := int64(0)
	for _, d := range workers {
		require(c.t, c.kube.CoreV1().Pods(namespace).Delete(c.ctx, d.pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &zero}))
		require(c.t, c.kube.CoreV1().Nodes().Delete(c.ctx, d.node.Name, metav1.DeleteOptions{}))
	}

	c.await("scaled inventory removed", 90*time.Second, func() error { return c.cacheReady("independent", 1, 1) })
	c.convergedRevision(revision, independent)
	c.footprint()
	c.t.Logf("constant durable object footprint across %d selected Node/Pod additions and removals", count)
}
