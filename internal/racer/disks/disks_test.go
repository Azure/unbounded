// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package disks

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestParseReportFixture(t *testing.T) {
	b, err := os.ReadFile("../testdata/disk-report.json")
	if err != nil {
		t.Fatal(err)
	}

	r, err := ParseReport(string(b))
	if err != nil {
		t.Fatal(err)
	}

	if len(r.Disks) != 5 {
		t.Fatalf("got %d disks", len(r.Disks))
	}

	d := r.Disks[0]
	if d.ID != "wwn-0x5000c500a1b2c3d4" || d.Dev != "nvme0n1" || d.SizeBytes != 1000204886016 ||
		d.Model != "Samsung SSD 980" || d.Serial != "S64ANS0T123456" || d.State != StateClaimed || len(d.Aliases) != 2 {
		t.Fatalf("disk 0: %+v", d)
	}

	if r.Disks[2].Reason != "mounted" || r.Disks[4].State != StateMissing {
		t.Fatalf("unexpected: %+v", r.Disks)
	}

	got, ok := r.Find("nvme-Samsung_SSD_980_S64ANS0T123456")
	if !ok || got.ID != d.ID {
		t.Fatalf("alias lookup: %+v %v", got, ok)
	}

	if _, ok := r.Find("nope"); ok {
		t.Fatal("found unknown")
	}
}

func TestParseReportEdge(t *testing.T) {
	r, err := ParseReport(`{"shares":1}`)
	if err != nil || r.Disks != nil {
		t.Fatalf("%v %v", r, err)
	}

	if _, err := ParseReport(`{`); err == nil {
		t.Fatal("want error")
	}

	if _, err := ParseReport(`{"disks":[{"state":"claimed"}]}`); err == nil {
		t.Fatal("want missing id error")
	}

	var nilReport *Report
	if _, ok := nilReport.Find("x"); ok {
		t.Fatal("nil report found")
	}
}

func TestValidateID(t *testing.T) {
	for _, id := range []string{"wwn-0x1", "nvme-eui.00", "a"} {
		if err := ValidateID(id); err != nil {
			t.Errorf("%q: %v", id, err)
		}
	}

	for _, id := range []string{"", ".", "..", "a/b", "a,b", "a b", "a\tb", "\n"} {
		if err := ValidateID(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("%q: got %v", id, err)
		}
	}
}

func TestAnnotationRoundTrip(t *testing.T) {
	got := ParseAnnotation(" b , ,a,,b ")
	if strings.Join(got, "|") != "b|a|b" {
		t.Fatalf("%v", got)
	}

	v, err := FormatAnnotation(got)
	if err != nil || v != "a,b" {
		t.Fatalf("%q %v", v, err)
	}

	if _, err := FormatAnnotation([]string{"a/b"}); err == nil {
		t.Fatal("want invalid")
	}

	if _, err := FormatAnnotation([]string{strings.Repeat("x", MaxAnnotationBytes+1)}); err == nil {
		t.Fatal("want too long")
	}

	if ParseAnnotation("") != nil {
		t.Fatal("want nil")
	}
}
