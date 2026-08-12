// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package racerctrl

import "testing"

func TestMembershipConfigMapName(t *testing.T) {
	if got := MembershipConfigMapName(7, 3); got != "racer-u7-z3" {
		t.Fatalf("name %q, want racer-u7-z3", got)
	}

	// The watch matches on the prefix alone, so it has to be one.
	if got := MembershipConfigMapName(1, 1); len(got) <= len(MembershipConfigMapPrefix) {
		t.Fatalf("name %q does not extend the prefix", got)
	}
}

func TestMembershipLabelsRoundTrip(t *testing.T) {
	universe, zone, ok := ParseMembershipLabels(MembershipLabels(4, 9))
	if !ok {
		t.Fatal("labels this package wrote were not recognised")
	}

	if universe != 4 || zone != 9 {
		t.Fatalf("read universe %d zone %d, want 4 and 9", universe, zone)
	}
}

func TestParseMembershipLabelsRejectsOtherConfigMaps(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
	}{
		{name: "empty", labels: nil},
		{name: "no zone", labels: map[string]string{MembershipUniverseLabel: "1"}},
		{name: "no universe", labels: map[string]string{MembershipZoneLabel: "1"}},
		{
			name:   "zero universe",
			labels: map[string]string{MembershipUniverseLabel: "0", MembershipZoneLabel: "1"},
		},
		{
			name:   "zero zone",
			labels: map[string]string{MembershipUniverseLabel: "1", MembershipZoneLabel: "0"},
		},
		{
			name:   "unparseable",
			labels: map[string]string{MembershipUniverseLabel: "one", MembershipZoneLabel: "1"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, ok := ParseMembershipLabels(test.labels); ok {
				t.Fatal("labels were accepted, want rejected")
			}
		})
	}
}

func TestParseMembershipEpoch(t *testing.T) {
	epoch, err := ParseMembershipEpoch(map[string]string{MembershipEpochKey: "12"})
	if err != nil {
		t.Fatalf("parse epoch: %v", err)
	}

	if epoch != 12 {
		t.Fatalf("epoch %d, want 12", epoch)
	}

	// A map written before the epoch travelled with the membership reads as
	// zero, which is what tells the caller to fall back to the class.
	if epoch, err := ParseMembershipEpoch(map[string]string{MembershipDataKey: "1"}); err != nil || epoch != 0 {
		t.Fatalf("undated membership read as %d (%v), want 0 and no error", epoch, err)
	}

	if _, err := ParseMembershipEpoch(map[string]string{MembershipEpochKey: "later"}); err == nil {
		t.Fatal("an unparseable epoch was accepted")
	}
}
