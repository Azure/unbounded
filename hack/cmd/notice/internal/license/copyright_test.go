// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package license

import (
	"reflect"
	"testing"
)

func TestExtractCopyright(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{
			name: "single line",
			input: `MIT License

Copyright (c) 2013 Steve Francia

Permission is hereby granted...`,
			want: []string{"Copyright (c) 2013 Steve Francia"},
		},
		{
			name: "multiple distinct lines",
			input: `Copyright (c) 2017 Nathan Sweet
Copyright (c) 2018, 2019 Cloudflare
Copyright (c) 2019 Authors of Cilium

The above copyright notice and this permission notice shall be included...`,
			want: []string{
				"Copyright (c) 2017 Nathan Sweet",
				"Copyright (c) 2018, 2019 Cloudflare",
				"Copyright (c) 2019 Authors of Cilium",
			},
		},
		{
			name: "deduplicates",
			input: `Copyright (c) 2020 Foo
Copyright (c) 2020 Foo`,
			want: []string{"Copyright (c) 2020 Foo"},
		},
		{
			name: "rejects boilerplate phrases",
			input: `The above COPYRIGHT NOTICE shall be included.
Copyright HOLDER must not be held liable.
Copyright (c) 2021 Real Author`,
			want: []string{"Copyright (c) 2021 Real Author"},
		},
		{
			name: "rejects lowercase boilerplate fragments",
			input: `License grants you a copyright license to reproduce.
Copyright 2024 Real Org`,
			want: []string{"Copyright 2024 Real Org"},
		},
		{
			name:    "no copyright line",
			input:   "Apache License\nVersion 2.0",
			wantErr: true,
		},
		{
			name:  "with copyright symbol",
			input: `Copyright © 2010-2025 three.js authors`,
			want:  []string{"Copyright © 2010-2025 three.js authors"},
		},
		{
			name: "rejects section heading without year or symbol",
			input: `Copyright Licenses:

The TCG grants a license...
Copyright (c) 2020 Real Author`,
			want: []string{"Copyright (c) 2020 Real Author"},
		},
		{
			name:  "accepts bare year form",
			input: `Copyright 2020 Foo Corporation`,
			want:  []string{"Copyright 2020 Foo Corporation"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractCopyright([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestExtractCopyrightFromDir_FallbackChain(t *testing.T) {
	dir := t.TempDir()

	if err := writeFile(dir, "AUTHORS", "Copyright (c) 2024 Sibling Authors\n"); err != nil {
		t.Fatal(err)
	}

	got, err := ExtractCopyrightFromDir(dir, []byte("Apache License Version 2.0\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"Copyright (c) 2024 Sibling Authors"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestExtractCopyrightFromDir_Placeholder(t *testing.T) {
	dir := t.TempDir()

	got, err := ExtractCopyrightFromDir(dir, []byte("no copyright here"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"See LICENSE file"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestFindFile(t *testing.T) {
	dir := t.TempDir()
	if err := writeFile(dir, "LICENSE.md", "x"); err != nil {
		t.Fatal(err)
	}

	got, err := FindFile(dir)
	if err != nil {
		t.Fatalf("FindFile: %v", err)
	}

	if got == "" {
		t.Errorf("expected non-empty path")
	}
}

func TestFindFile_Missing(t *testing.T) {
	if _, err := FindFile(t.TempDir()); err == nil {
		t.Errorf("expected error for empty dir")
	}
}
