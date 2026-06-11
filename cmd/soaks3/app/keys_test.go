// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import "testing"

func TestNormalizePrefix(t *testing.T) {
	cases := map[string]string{
		"soaks3/":    "soaks3/",
		"soaks3":     "soaks3/",
		"/soaks3":    "soaks3/",
		"/soaks3///": "soaks3/",
		"  a/b  ":    "a/b/",
		"":           "",
		"/":          "",
	}

	for in, want := range cases {
		if got := normalizePrefix(in); got != want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeyForIndex(t *testing.T) {
	m, err := newKeyModel("soaks3/", 1000)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := m.keyForIndex(123), "soaks3/obj-0000000123"; got != want {
		t.Errorf("keyForIndex(123) = %q, want %q", got, want)
	}

	if got, want := m.keyForIndex(0), "soaks3/obj-0000000000"; got != want {
		t.Errorf("keyForIndex(0) = %q, want %q", got, want)
	}
}

func TestKeyForIndexDeterministic(t *testing.T) {
	m1, _ := newKeyModel("p/", 10)
	m2, _ := newKeyModel("p", 10)

	for i := int64(0); i < 10; i++ {
		if m1.keyForIndex(i) != m2.keyForIndex(i) {
			t.Errorf("index %d: %q != %q", i, m1.keyForIndex(i), m2.keyForIndex(i))
		}
	}
}

func TestRelPathForIndex(t *testing.T) {
	m, _ := newKeyModel("soaks3/", 10)
	if got, want := m.relPathForIndex(5), "soaks3/obj-0000000005"; got != want {
		t.Errorf("relPathForIndex(5) = %q, want %q", got, want)
	}
}

func TestDeriveCount(t *testing.T) {
	cases := []struct {
		name       string
		count      int64
		totalSize  int64
		objectSize int64
		want       int64
		wantErr    bool
	}{
		{name: "explicit count", count: 42, want: 42},
		{name: "total size divides", totalSize: 40, objectSize: 4, want: 10},
		{name: "total size truncates", totalSize: 43, objectSize: 4, want: 10},
		{name: "both set", count: 5, totalSize: 40, objectSize: 4, wantErr: true},
		{name: "neither set", wantErr: true},
		{name: "negative count", count: -1, wantErr: true},
		{name: "negative total", totalSize: -1, wantErr: true},
		{name: "total smaller than object", totalSize: 3, objectSize: 4, wantErr: true},
		{name: "total with zero object size", totalSize: 40, objectSize: 0, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deriveCount(tc.count, tc.totalSize, tc.objectSize)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got count=%d", got)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got != tc.want {
				t.Errorf("deriveCount = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestNewKeyModelNegativeCount(t *testing.T) {
	if _, err := newKeyModel("p/", -1); err == nil {
		t.Fatal("expected error for negative count")
	}
}
