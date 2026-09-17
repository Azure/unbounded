// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"net/http"
	"reflect"
	"testing"
)

func assertStatusURL(t *testing.T, got, want, host, port string) {
	t.Helper()

	if got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}

	req, err := http.NewRequest(http.MethodGet, got, nil)
	if err != nil {
		t.Fatalf("create request for %q: %v", got, err)
	}

	if req.URL.Hostname() != host || req.URL.Port() != port {
		t.Fatalf("unexpected URL authority: hostname=%q port=%q", req.URL.Hostname(), req.URL.Port())
	}

	if port != "" {
		gotHost, gotPort, err := net.SplitHostPort(req.URL.Host)
		if err != nil || gotHost != host || gotPort != port {
			t.Fatalf("invalid dial authority %q: host=%q port=%q err=%v", req.URL.Host, gotHost, gotPort, err)
		}
	}
}

func TestStatusServiceHostURLs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		host      string
		authority string
	}{
		{"DNS", "controller.svc", "controller.svc"},
		{"IPv4", "10.96.0.1", "10.96.0.1"},
		{"IPv6", "fd00::1", "[fd00::1]"},
		{"IPv4-mapped IPv6", "::ffff:10.96.0.1", "[::ffff:10.96.0.1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", tc.host)
			t.Setenv("KUBERNETES_SERVICE_HOST", tc.host)

			for _, ports := range []struct {
				name       string
				controller string
				api        string
				wantDirect string
				wantAPI    string
			}{
				{"defaults", "", "", "9999", "443"},
				{"custom", "8443", "6443", "8443", "6443"},
			} {
				t.Run(ports.name, func(t *testing.T) {
					t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT", ports.controller)
					t.Setenv("KUBERNETES_SERVICE_PORT", ports.api)

					manager := newHMACTokenManager("node-a")
					if len(manager.tokenURLs) != 2 {
						t.Fatalf("expected direct and fallback token URLs, got %v", manager.tokenURLs)
					}

					directBase := "https://" + tc.authority + ":" + ports.wantDirect
					apiBase := "https://" + tc.authority + ":" + ports.wantAPI
					assertStatusURL(t, manager.tokenURLs[0], directBase+directHMACTokenPath, tc.host, ports.wantDirect)
					assertStatusURL(t, manager.tokenURLs[1], apiBase+hmacTokenEndpointPath, tc.host, ports.wantAPI)

					cfg := &config{}
					directWS := "wss://" + tc.authority + ":" + ports.wantDirect + "/status/nodews"
					apiWS := "wss://" + tc.authority + "/apis/status.net.unbounded-cloud.io/v1alpha1/status/nodews"
					assertStatusURL(t, resolveDirectStatusPushURL(cfg), directBase+"/status/push", tc.host, ports.wantDirect)
					assertStatusURL(t, resolveDirectStatusWebSocketURL(cfg), directWS, tc.host, ports.wantDirect)
					assertStatusURL(t, resolveStatusPushAPIServerURL(cfg),
						"https://"+tc.authority+"/apis/status.net.unbounded-cloud.io/v1alpha1/status/push", tc.host, "")

					urls := resolveStatusWebSocketURLs(cfg, true)
					if !reflect.DeepEqual(urls, []string{directWS, apiWS}) {
						t.Fatalf("unexpected websocket endpoints or preference: %v", urls)
					}

					assertStatusURL(t, urls[1], apiWS, tc.host, "")
				})
			}
		})
	}
}

func TestHMACTokenManagerMissingServiceHosts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		controller string
		api        string
		want       []string
	}{
		{"no controller", "", "fd00::2", []string{"https://[fd00::2]:443" + hmacTokenEndpointPath}},
		{"no API server", "fd00::1", "", []string{"https://[fd00::1]:9999" + directHMACTokenPath}},
		{"no hosts", "", "", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_HOST", tc.controller)
			t.Setenv("UNBOUNDED_NET_CONTROLLER_SERVICE_PORT", "")
			t.Setenv("KUBERNETES_SERVICE_HOST", tc.api)
			t.Setenv("KUBERNETES_SERVICE_PORT", "")

			if got := newHMACTokenManager("node-a").tokenURLs; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("token URLs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExpandKubernetesServiceHostIPv6(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "fd00::2")

	for _, template := range []string{
		"wss://$(KUBERNETES_SERVICE_HOST):6443/status/nodews",
		"wss://${KUBERNETES_SERVICE_HOST}:6443/status/nodews",
		"wss://[$(KUBERNETES_SERVICE_HOST)]:6443/status/nodews",
		"wss://[${KUBERNETES_SERVICE_HOST}]:6443/status/nodews",
	} {
		t.Run(template, func(t *testing.T) {
			assertStatusURL(t, expandKubernetesServiceHost(template),
				"wss://[fd00::2]:6443/status/nodews", "fd00::2", "6443")
			assertStatusURL(t, resolveStatusPushAPIServerURL(&config{StatusWSAPIServerURL: template}),
				"https://[fd00::2]:6443/status/push", "fd00::2", "6443")
		})
	}

	const explicit = "wss://[fd00::99]:7443/status/nodews"
	if got := expandKubernetesServiceHost(explicit); got != explicit {
		t.Fatalf("explicit URL changed: %q", got)
	}

	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	const unresolved = "wss://$(KUBERNETES_SERVICE_HOST)/status/nodews"
	if got := expandKubernetesServiceHost(unresolved); got != unresolved {
		t.Fatalf("missing host changed template: %q", got)
	}
}
