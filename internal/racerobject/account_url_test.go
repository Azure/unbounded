// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"strings"
	"testing"
)

func TestAzureAccountURLValidation(t *testing.T) {
	for _, value := range []string{
		"https://account.blob.core.windows.net", "https://account/", "https://account:443",
		"https://localhost:10000/devstoreaccount1", "https://localhost:10000/devstoreaccount1/",
		"https://[::1]:443/account", "https://[::1]/account", "https://host/account-name_1",
	} {
		for _, credential := range []string{"default", "anonymous"} {
			if err := ValidateAzureAccountURL(value, credential); err != nil {
				t.Errorf("rejected %q (%s): %v", value, credential, err)
			}
		}
	}

	for _, value := range []string{"http://localhost:10000/devstoreaccount1", "http://127.0.0.1/", "http://[::1]:80"} {
		if err := ValidateAzureAccountURL(value, "anonymous"); err != nil {
			t.Errorf("rejected anonymous %q: %v", value, err)
		}

		if err := ValidateAzureAccountURL(value, "default"); err == nil {
			t.Errorf("accepted default credential over HTTP: %q", value)
		}
	}

	for _, value := range []string{
		"", "account", "ftp://account", "https://", "https:///account", "https:account",
		"https://user:private-token@account", "https://account?sig=private-token", "https://account?", "https://account#private-token", "https://account#",
		"https://account:0", "https://account:65536", "https://account:abc", "https://account:", "https://account:-1", "https://account:+80", "https://account:99999999999999999999999",
		"https://::1", "https://[host]:80", "https://[::1%25eth0]:80", "https://host:80:90",
		"https://host//", "https://host//account", "https://host/account//", "https://host/account/container", "https://host/.", "https://host/..",
		"https://host/%2e%2e", "https://host/%2f", "https://host/account%2Fcontainer", "https://host/%61ccount", "https://host/%252f", "https://host/account%5ccontainer",
		"https://$(STORAGE_HOST)", "https://host$(X).net", "https://ho_st", "https://host/account\\container", "https://host/account;param", "https://host/account name", "https://host/雪", "https://host/%00", "https://host/%zz",
	} {
		for _, credential := range []string{"default", "anonymous"} {
			err := ValidateAzureAccountURL(value, credential)
			if err == nil || strings.Contains(err.Error(), "private-token") {
				t.Errorf("invalid URL accepted or leaked: %q (%s): %v", value, credential, err)
			}
		}
	}
}

func TestAzureAccountURLRejectsUnknownCredential(t *testing.T) {
	if err := ValidateAzureAccountURL("https://account.blob.core.windows.net", "workload"); err == nil {
		t.Fatal("accepted unknown credential")
	}
}

func TestValidateBucket(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bucket string
		ok     bool
	}{
		{name: "simple", bucket: "models", ok: true},
		{name: "dots", bucket: "data.v1", ok: true},
		{name: "underscore and dash", bucket: "a_b-c", ok: true},
		{name: "max length", bucket: strings.Repeat("b", 255), ok: true},
		{name: "dot", bucket: "."},
		{name: "dot dot", bucket: ".."},
		{name: "slash", bucket: "a/b"},
		{name: "space", bucket: "a b"},
		{name: "url", bucket: "https://account.blob.core.windows.net"},
		{name: "empty", bucket: ""},
		{name: "too long", bucket: strings.Repeat("b", 256)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBucket(tc.bucket)
			if tc.ok && err != nil {
				t.Fatalf("rejected: %v", err)
			}

			if !tc.ok && err == nil {
				t.Fatal("accepted invalid bucket")
			}

			if _, reqErr := NewRequest(tc.bucket, "key", ""); (reqErr == nil) != tc.ok {
				t.Fatalf("NewRequest bucket agreement: %v", reqErr)
			}
		})
	}
}
