// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"encoding/base64"
	"testing"
)

func TestDiscoverKubernetesOIDCConfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  string
		override string
		wantAud  string
		wantErr  bool
	}{
		{name: "issuer and API audience", payload: `{"iss":"https://issuer.example/cluster/","aud":["https://kubernetes.default.svc"]}`, wantAud: "https://kubernetes.default.svc"},
		{name: "explicit audience", payload: `{"iss":"https://issuer.example/cluster/","aud":["api"]}`, override: "custom", wantAud: "custom"},
		{name: "explicit audience resolves ambiguity", payload: `{"iss":"https://issuer.example/cluster/","aud":["api","other"]}`, override: "custom", wantAud: "custom"},
		{name: "missing issuer", payload: `{"aud":["api"]}`, wantErr: true},
		{name: "legacy issuer", payload: `{"iss":"kubernetes/serviceaccount","aud":["api"]}`, wantErr: true},
		{name: "insecure issuer", payload: `{"iss":"http://issuer.example","aud":["api"]}`, wantErr: true},
		{name: "URL credentials", payload: `{"iss":"https://user:password@issuer.example","aud":["api"]}`, wantErr: true},
		{name: "invalid URL", payload: `{"iss":"https://%","aud":["api"]}`, wantErr: true},
		{name: "missing audience", payload: `{"iss":"https://issuer.example/cluster/"}`, wantErr: true},
		{name: "empty audience", payload: `{"iss":"https://issuer.example/cluster/","aud":[""]}`, wantErr: true},
		{name: "ambiguous audience", payload: `{"iss":"https://issuer.example/cluster/","aud":["api","other"]}`, wantErr: true},
		{name: "malformed claims", payload: `{`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := "header." + base64.RawURLEncoding.EncodeToString([]byte(tc.payload)) + ".signature"

			issuer, audience, err := DiscoverKubernetesOIDCConfig(token, tc.override)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error = %v", err, tc.wantErr)
			}

			if err == nil && (issuer != "https://issuer.example/cluster/" || audience != tc.wantAud) {
				t.Fatalf("unexpected discovered config: issuer=%q audience=%q", issuer, audience)
			}
		})
	}
}
