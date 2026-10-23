//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// diagnosticsPort is the fixed racer-dataplane diagnostics port that serves
// /debug/membership.
const diagnosticsPort = 9090

// errMembershipUnavailable is returned for the "unavailable" line a
// dataplane serves before its membership state can be read. It is a valid
// transient answer, unlike a malformed line.
var errMembershipUnavailable = errors.New("membership unavailable")

// membershipFields lists the /debug/membership keys in the order
// MembershipDiagnostic::write in cmd/racer-dataplane/src/telemetry.rs
// prints them.
var membershipFields = []string{
	"accepted_sequence", "accepted_membership", "accepted_membership_hash",
	"pending_sequence", "pending_membership", "expected_workers", "matching_workers", "fully_applied",
}

// membership is one dataplane's /debug/membership report.
type membership struct {
	AcceptedSequence   uint64
	AcceptedMembership uint64
	AcceptedHash       string
	PendingSequence    uint64
	PendingMembership  uint64
	ExpectedWorkers    uint64
	MatchingWorkers    uint64
	FullyApplied       bool
}

// accepted is the goal state every dataplane must agree on.
func (m membership) accepted() string {
	return fmt.Sprintf("sequence=%d membership=%d hash=%s", m.AcceptedSequence, m.AcceptedMembership, m.AcceptedHash)
}

// parseMembership parses a /debug/membership body. It returns
// errMembershipUnavailable for the "unavailable fully_applied=0" answer and
// another error for anything that does not match the documented format,
// including a fully_applied flag that contradicts the other fields.
func parseMembership(body string) (membership, error) {
	line := strings.TrimSuffix(body, "\n")
	if line == "unavailable fully_applied=0" {
		return membership{}, errMembershipUnavailable
	}

	fields := strings.Split(line, " ")
	if len(fields) != len(membershipFields) {
		return membership{}, fmt.Errorf("want %d fields, got %d", len(membershipFields), len(fields))
	}

	values := make([]string, len(fields))

	for i, f := range fields {
		key, value, ok := strings.Cut(f, "=")
		if !ok || key != membershipFields[i] || value == "" {
			return membership{}, fmt.Errorf("field %d: want %s=<value>, got %q", i, membershipFields[i], f)
		}

		values[i] = value
	}

	var (
		m    membership
		errs []error
	)

	num := func(i int, dst *uint64) {
		v, err := strconv.ParseUint(values[i], 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", membershipFields[i], err))
		}

		*dst = v
	}

	num(0, &m.AcceptedSequence)
	num(1, &m.AcceptedMembership)
	num(3, &m.PendingSequence)
	num(4, &m.PendingMembership)
	num(5, &m.ExpectedWorkers)
	num(6, &m.MatchingWorkers)

	m.AcceptedHash = values[2]
	if len(m.AcceptedHash) != 64 || strings.Trim(m.AcceptedHash, "0123456789abcdef") != "" {
		errs = append(errs, fmt.Errorf("accepted_membership_hash: want 64 lowercase hex digits, got %q", m.AcceptedHash))
	}

	switch values[7] {
	case "0":
	case "1":
		m.FullyApplied = true
	default:
		errs = append(errs, fmt.Errorf("fully_applied: want 0 or 1, got %q", values[7]))
	}

	if err := errors.Join(errs...); err != nil {
		return membership{}, err
	}

	want := m.AcceptedSequence != 0 && m.PendingSequence == 0 &&
		m.ExpectedWorkers != 0 && m.MatchingWorkers == m.ExpectedWorkers
	if m.FullyApplied != want {
		return membership{}, fmt.Errorf("fully_applied=%t contradicts the reported counters", m.FullyApplied)
	}

	return m, nil
}

// convergedMembership returns the shared goal state when every pod in pods
// reported, every report is fully applied, and all agree on the accepted
// sequence, membership, and hash.
func convergedMembership(pods []string, reports map[string]membership) (membership, error) {
	if len(pods) == 0 {
		return membership{}, errors.New("no dataplane pods")
	}

	var first membership

	for i, pod := range pods {
		m, ok := reports[pod]
		if !ok {
			return membership{}, fmt.Errorf("no report from %s", pod)
		}

		if !m.FullyApplied {
			return membership{}, fmt.Errorf("%s not fully applied: %s pending_sequence=%d matching=%d/%d",
				pod, m.accepted(), m.PendingSequence, m.MatchingWorkers, m.ExpectedWorkers)
		}

		if i == 0 {
			first = m
			continue
		}

		if m.accepted() != first.accepted() {
			return membership{}, fmt.Errorf("%s has %s, %s has %s", pods[0], first.accepted(), pod, m.accepted())
		}
	}

	return first, nil
}

// checkMembershipAdvance verifies that after did not move behind before:
// the sequence and membership never decrease, the same membership keeps the
// same hash, and the same sequence names the same membership.
func checkMembershipAdvance(before, after membership) error {
	switch {
	case after.AcceptedSequence < before.AcceptedSequence:
		return fmt.Errorf("accepted sequence went back: %d -> %d", before.AcceptedSequence, after.AcceptedSequence)
	case after.AcceptedMembership < before.AcceptedMembership:
		return fmt.Errorf("accepted membership went back: %d -> %d", before.AcceptedMembership, after.AcceptedMembership)
	case after.AcceptedMembership == before.AcceptedMembership && after.AcceptedHash != before.AcceptedHash:
		return fmt.Errorf("membership %d changed hash: %s -> %s", after.AcceptedMembership, before.AcceptedHash, after.AcceptedHash)
	case after.AcceptedSequence == before.AcceptedSequence && after.accepted() != before.accepted():
		return fmt.Errorf("sequence %d names different state: %s -> %s", after.AcceptedSequence, before.accepted(), after.accepted())
	}

	return nil
}

// podMembership reads one pod's /debug/membership through the API server
// pod proxy. Transport errors and the unavailable answer return ok=false so
// the caller retries; a malformed answer fails the test.
func (h *harness) podMembership(pod string) (membership, bool, string) {
	h.t.Helper()

	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy/debug/membership", operatorNamespace, pod, diagnosticsPort)

	out, err := h.exec(time.Minute, nil, "kubectl", "get", "--raw", path)
	if err != nil {
		return membership{}, false, pod + ": diagnostics not reachable"
	}

	m, err := parseMembership(out)
	if errors.Is(err, errMembershipUnavailable) {
		return membership{}, false, pod + ": " + err.Error()
	}

	if err != nil {
		h.t.Fatalf("malformed /debug/membership from %s: %v: %q", pod, err, out)
	}

	return m, true, ""
}

// dataplaneDesired returns the DaemonSet's desired pod count, or 0 if it
// cannot be read.
func (h *harness) dataplaneDesired() int {
	h.t.Helper()

	out, err := h.exec(time.Minute, nil, "kubectl", "-n", operatorNamespace, "get", "daemonset", "racer-dataplane",
		"-o", "jsonpath={.status.desiredNumberScheduled}")
	if err != nil {
		return 0
	}

	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}

	return n
}

// waitConverged waits until every desired dataplane pod is live and ready
// and all report the same fully applied goal state, and returns it with the
// view it was read from.
func (h *harness) waitConverged(what string) (membership, controlView) {
	h.t.Helper()

	var (
		goal membership
		v    controlView
	)

	h.waitFor(what, 5*time.Minute, func() (bool, string) {
		v = h.controlView()

		desired := h.dataplaneDesired()
		if desired == 0 || len(v.Pods) != desired || v.readyPods() != desired {
			return false, fmt.Sprintf("ready pods %d/%d of %d desired", v.readyPods(), len(v.Pods), desired)
		}

		names := make([]string, 0, len(v.Pods))
		reports := map[string]membership{}

		for _, p := range v.Pods {
			names = append(names, p.Name)

			m, ok, detail := h.podMembership(p.Name)
			if !ok {
				return false, detail
			}

			reports[p.Name] = m
		}

		var err error

		goal, err = convergedMembership(names, reports)
		if err != nil {
			return false, err.Error()
		}

		return true, ""
	})

	return goal, v
}
