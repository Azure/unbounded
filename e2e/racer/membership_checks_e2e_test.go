//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"errors"
	"strings"
	"testing"
)

const testHash = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func membershipLine(seq, mem, pendingSeq, expected, matching, applied string) string {
	return "accepted_sequence=" + seq + " accepted_membership=" + mem + " accepted_membership_hash=" + testHash +
		" pending_sequence=" + pendingSeq + " pending_membership=0 expected_workers=" + expected +
		" matching_workers=" + matching + " fully_applied=" + applied + "\n"
}

func TestParseMembership(t *testing.T) {
	good := membershipLine("7", "3", "0", "3", "3", "1")

	m, err := parseMembership(good)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	want := membership{
		AcceptedSequence: 7, AcceptedMembership: 3, AcceptedHash: testHash,
		ExpectedWorkers: 3, MatchingWorkers: 3, FullyApplied: true,
	}
	if m != want {
		t.Fatalf("got %+v, want %+v", m, want)
	}

	if m, err := parseMembership(membershipLine("7", "3", "8", "3", "2", "0")); err != nil || m.FullyApplied {
		t.Fatalf("pending report: got %+v, %v", m, err)
	}

	if _, err := parseMembership("unavailable fully_applied=0\n"); !errors.Is(err, errMembershipUnavailable) {
		t.Fatalf("unavailable: got %v", err)
	}

	malformed := map[string]string{
		"empty":               "",
		"unavailable variant": "unavailable fully_applied=1\n",
		"missing field":       strings.Replace(good, " pending_membership=0", "", 1),
		"extra field":         strings.TrimSuffix(good, "\n") + " extra=1\n",
		"reordered":           strings.Replace(good, "accepted_sequence=7 accepted_membership=3", "accepted_membership=3 accepted_sequence=7", 1),
		"no value":            strings.Replace(good, "accepted_sequence=7", "accepted_sequence=", 1),
		"negative":            strings.Replace(good, "accepted_sequence=7", "accepted_sequence=-1", 1),
		"overflow":            strings.Replace(good, "accepted_sequence=7", "accepted_sequence=18446744073709551616", 1),
		"short hash":          strings.Replace(good, testHash, "abcd", 1),
		"upper hash":          strings.Replace(good, testHash, strings.ToUpper(testHash), 1),
		"bad flag":            membershipLine("7", "3", "0", "3", "3", "2"),
		"flag contradicts":    membershipLine("7", "3", "0", "3", "2", "1"),
		"applied zero seq":    membershipLine("0", "0", "0", "3", "3", "1"),
		"unapplied but ready": membershipLine("7", "3", "0", "3", "3", "0"),
		"two lines":           good + good,
	}
	for name, body := range malformed {
		t.Run(name, func(t *testing.T) {
			_, err := parseMembership(body)
			if err == nil || errors.Is(err, errMembershipUnavailable) {
				t.Fatalf("want malformed error, got %v", err)
			}
		})
	}
}

func TestConvergedMembership(t *testing.T) {
	applied := membership{AcceptedSequence: 7, AcceptedMembership: 3, AcceptedHash: testHash, ExpectedWorkers: 3, MatchingWorkers: 3, FullyApplied: true}
	pods := []string{"a", "b", "c"}
	all := func(m membership) map[string]membership {
		return map[string]membership{"a": applied, "b": applied, "c": m}
	}

	if got, err := convergedMembership(pods, all(applied)); err != nil || got != applied {
		t.Fatalf("converged: got %+v, %v", got, err)
	}

	bad := map[string]map[string]membership{
		"missing report": {"a": applied, "b": applied},
		"not applied":    all(membership{AcceptedSequence: 7, AcceptedMembership: 3, AcceptedHash: testHash}),
		"other sequence": all(membership{AcceptedSequence: 8, AcceptedMembership: 3, AcceptedHash: testHash, FullyApplied: true}),
		"other members":  all(membership{AcceptedSequence: 7, AcceptedMembership: 4, AcceptedHash: testHash, FullyApplied: true}),
		"other hash":     all(membership{AcceptedSequence: 7, AcceptedMembership: 3, AcceptedHash: strings.Repeat("f", 64), FullyApplied: true}),
	}
	for name, reports := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := convergedMembership(pods, reports); err == nil {
				t.Fatal("want error")
			}
		})
	}

	if _, err := convergedMembership(nil, nil); err == nil {
		t.Fatal("no pods: want error")
	}
}

func TestCheckMembershipAdvance(t *testing.T) {
	base := membership{AcceptedSequence: 7, AcceptedMembership: 3, AcceptedHash: testHash}
	other := strings.Repeat("f", 64)

	ok := map[string]membership{
		"same":            base,
		"newer sequence":  {AcceptedSequence: 8, AcceptedMembership: 3, AcceptedHash: testHash},
		"newer members":   {AcceptedSequence: 8, AcceptedMembership: 4, AcceptedHash: other},
		"pending ignored": {AcceptedSequence: 7, AcceptedMembership: 3, AcceptedHash: testHash, PendingSequence: 9},
	}
	for name, after := range ok {
		if err := checkMembershipAdvance(base, after); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	bad := map[string]membership{
		"older sequence":          {AcceptedSequence: 6, AcceptedMembership: 3, AcceptedHash: testHash},
		"older members":           {AcceptedSequence: 8, AcceptedMembership: 2, AcceptedHash: testHash},
		"same members new hash":   {AcceptedSequence: 8, AcceptedMembership: 3, AcceptedHash: other},
		"same sequence new state": {AcceptedSequence: 7, AcceptedMembership: 4, AcceptedHash: other},
	}
	for name, after := range bad {
		if err := checkMembershipAdvance(base, after); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
