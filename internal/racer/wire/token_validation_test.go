// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTokenValidationRejectsBeforeDescending(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, suffix string
		typ                  reflect.Type
	}{
		{"unknown field", `{"unknown"`, `:[{"ignored":[]}]}`, reflect.TypeFor[Publication]()},
		{"case variant", `{"Schema_version"`, `:[{}]}`, reflect.TypeFor[Publication]()},
		{"escaped duplicate", `{"schema_version":1,"schema_versi\u006fn"`, `:[{}]}`, reflect.TypeFor[Publication]()},
		{"wrong root", `[`, `{"ignored":[]}]`, reflect.TypeFor[Publication]()},
		{"wrong collection", `{"members":{`, `"ignored":[]}}`, reflect.TypeFor[Publication]()},
		{"wrong element", `{"members":[[`, `{"ignored":[]}]]}`, reflect.TypeFor[Publication]()},
		{"wrong primitive", `{"schema_version":[`, `{"ignored":[]}]}`, reflect.TypeFor[Publication]()},
		{"wrong bytes", `{"csr_der":[`, `{"ignored":[]}]}`, reflect.TypeFor[BootstrapRequest]()},
		{"nested unknown", `[{"unknown"`, `:[{}]}]`, reflect.TypeFor[[]RDMANIC]()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := json.NewDecoder(strings.NewReader(tc.prefix + tc.suffix))
			d.UseNumber()

			if err := checkValue(d, tc.typ, false, 0); !errors.Is(err, InvalidRequest) {
				t.Fatalf("got %v, want invalid_request", err)
			}

			// InputOffset measures consumed tokens, not decoder read-ahead. This
			// pins early rejection without a heap threshold or a huge hostile tree.
			if got := d.InputOffset(); got != int64(len(tc.prefix)) {
				t.Fatalf("traversed rejected subtree: offset %d, want %d", got, len(tc.prefix))
			}
		})
	}
}

func TestTokenValidationCollectionLimitsBeforeNextElement(t *testing.T) {
	for _, tc := range []struct {
		name, element string
		typ           reflect.Type
		limit         int
	}{
		{"members", `{"node":"","shares":0,"peer_endpoint":"","rdma_nics":[],"site":""}`, reflect.TypeFor[[]Member](), MaxMembers},
		{"NICs", `{"device":"a","port":1,"rail":0}`, reflect.TypeFor[[]RDMANIC](), MaxRDMANICs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := "[" + strings.Repeat(tc.element+",", tc.limit-1) + tc.element
			for _, suffix := range []string{"]", `,{"unvisited":[{}]}]`} {
				d := json.NewDecoder(strings.NewReader(prefix + suffix))
				d.UseNumber()

				err := checkValue(d, tc.typ, false, 0)
				if suffix == "]" {
					if err != nil {
						t.Fatalf("exact limit: %v", err)
					}

					continue
				}

				if !errors.Is(err, TooLarge) || d.InputOffset() != int64(len(prefix)) {
					t.Fatalf("consumed excess element: error=%v offset=%d, want %d", err, d.InputOffset(), len(prefix))
				}
			}
		})
	}
}

func TestTokenValidationPrimitiveContract(t *testing.T) {
	type primitives struct {
		Count uint64  `json:"count,string"`
		Byte  uint8   `json:"byte"`
		Flag  bool    `json:"flag"`
		Text  string  `json:"text"`
		Bytes []byte  `json:"bytes"`
		Opt   *uint32 `json:"opt,omitempty"`
	}

	const valid = `{"count":"18446744073709551615","byte":255,"flag":true,"text":"\ud83d\ude00","bytes":"AA=="}`
	for _, tc := range []struct{ old, replacement string }{
		{`"count":"18446744073709551615"`, `"count":"18446744073709551616"`},
		{`"count":"18446744073709551615"`, `"count":1`},
		{`"count":"18446744073709551615"`, `"count":"01"`},
		{`"count":"18446744073709551615"`, `"count":"+1"`},
		{`255`, `256`},
		{`255`, `-0`},
		{`255`, `1.0`},
		{`255`, `1e0`},
		{`255`, `"1"`},
		{`true`, `1`},
		{`true`, `null`},
		{`"\ud83d\ude00"`, `"\ud83d"`},
		{`"\ud83d\ude00"`, `"\ude00"`},
		{`"\ud83d\ude00"`, `null`},
		{`"\ud83d\ude00"`, `"` + "\xff" + `"`},
		{`"AA=="`, `"AB=="`},
		{`"AA=="`, `"AA"`},
		{`"AA=="`, `"AA==\n"`},
		{`"AA=="`, `null`},
		{`"AA=="`, `[]`},
		{`"flag":true,`, ``},
		{`"bytes":"AA=="`, `"bytes":"AA==","opt":null`},
		{`"bytes":"AA=="`, `"bytes":"AA==","opt":4294967296`},
	} {
		raw := strings.Replace(valid, tc.old, tc.replacement, 1)

		var v primitives
		if err := decode(strings.NewReader(raw), 4096, &v); !errors.Is(err, InvalidRequest) {
			t.Errorf("accepted %s: %v", raw, err)
		}
	}

	for _, raw := range []string{valid, `{"bytes":"AA==","text":"\ud83d\ude00","flag":true,"byte":255,"count":"18446744073709551615","opt":0}`} {
		var v primitives
		if err := decode(strings.NewReader(raw), 4096, &v); err != nil || v.Count != ^uint64(0) || v.Text != "😀" {
			t.Fatalf("valid reordered/optional primitives: %+v, %v", v, err)
		}

		for _, suffix := range []string{`{}`, `null`, `!`} {
			if err := decode(strings.NewReader(raw+suffix), 4096, &v); !errors.Is(err, InvalidRequest) {
				t.Fatalf("accepted trailing document %q: %v", suffix, err)
			}
		}
	}
}

func TestTokenValidationDepthBoundary(t *testing.T) {
	for _, depth := range []int{64, 65} {
		typ := reflect.TypeFor[string]()
		for range depth {
			typ = reflect.SliceOf(typ)
		}

		raw := strings.Repeat("[", depth) + `"leaf"` + strings.Repeat("]", depth)

		err := decode(strings.NewReader(raw), 4096, reflect.New(typ).Interface())
		if depth == 64 && err != nil || depth == 65 && !errors.Is(err, InvalidRequest) {
			t.Fatalf("depth %d: %v", depth, err)
		}
	}
}
