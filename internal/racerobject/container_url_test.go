// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"strings"
	"testing"
)

func TestParseAzureContainerURL(t *testing.T) {
	for _, tc := range []struct{ value, credential, account, container string }{
		{"https://account.blob.core.windows.net/models", "default", "https://account.blob.core.windows.net", "models"},
		{"https://account.blob.core.windows.net:443/a-1", "anonymous", "https://account.blob.core.windows.net:443", "a-1"},
		{"http://127.0.0.1:10000/devstoreaccount1/container", "anonymous", "http://127.0.0.1:10000/devstoreaccount1", "container"},
		{"https://[::1]/account/abc", "default", "https://[::1]/account", "abc"},
		{"https://host/" + strings.Repeat("a", 63), "default", "https://host", strings.Repeat("a", 63)},
	} {
		account, container, err := ParseAzureContainerURL(tc.value, tc.credential)
		if err != nil || account != tc.account || container != tc.container {
			t.Errorf("%q: got %q %q %v", tc.value, account, container, err)
		}
	}

	for _, value := range []string{
		"", "models", "https://account.blob.core.windows.net", "https://account.blob.core.windows.net/",
		"https://account.blob.core.windows.net/models/", "https://host/account/models/extra", "ftp://host/models",
		"https://user:private-token@host/models", "https://host/models?sig=private-token", "https://host/models#private-token",
		"https://host/Models", "https://host/ab", "https://host/" + strings.Repeat("a", 64), "https://host/-ab", "https://host/ab-",
		"https://host/a--b", "https://host/a_b", "https://host/a.b", "https://host/%61bc", "https://host//models", "https:///models",
		"https://host/acc%2Fount/models", "https://host:0/models",
	} {
		for _, credential := range []string{"default", "anonymous"} {
			_, _, err := ParseAzureContainerURL(value, credential)
			if err == nil || strings.Contains(err.Error(), "private-token") {
				t.Errorf("invalid URL accepted or leaked: %q (%s): %v", value, credential, err)
			}
		}
	}

	if _, _, err := ParseAzureContainerURL("http://127.0.0.1:10000/devstoreaccount1/container", "default"); err == nil {
		t.Error("accepted default credential over HTTP")
	}

	if _, _, err := ParseAzureContainerURL("https://host/models", "workload"); err == nil {
		t.Error("accepted unknown credential")
	}
}
