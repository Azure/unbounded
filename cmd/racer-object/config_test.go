// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func isolateOriginAWSConfig(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	for key, value := range map[string]string{
		"AWS_CONFIG_FILE": filepath.Join(dir, "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"),
		"AWS_PROFILE": "", "AWS_DEFAULT_PROFILE": "", "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_ACCESS_KEY_ID": "test-access", "AWS_SECRET_ACCESS_KEY": "test-secret", "AWS_SESSION_TOKEN": "test-session",
		"AWS_ENDPOINT_URL": "", "AWS_ENDPOINT_URL_S3": "", "AWS_IGNORE_CONFIGURED_ENDPOINT_URLS": "false",
		"AWS_CA_BUNDLE": "",
	} {
		t.Setenv(key, value)
	}

	return dir
}

func TestOriginEffectiveEndpoint(t *testing.T) {
	for _, source := range []string{"global-env", "service-env", "global-shared", "service-shared"} {
		t.Run(source, func(t *testing.T) {
			for _, endpoint := range []string{
				"", "https://s3.example", "https://s3.example/", "http://localhost:9000",
				"https://private-user:private-password@s3.example", "https://s3.example/private-path",
				"https://s3.example?private-query=secret", "https://s3.example#private-fragment", "https://s3.example:99999",
				"https://s3.example/%zz",
			} {
				t.Run(endpoint, func(t *testing.T) {
					dir := isolateOriginAWSConfig(t)
					setOriginEndpointSource(t, dir, source, endpoint)

					o := options{region: "us-east-1", requestTimeout: time.Minute, pathStyle: true}

					cfg, err := originAWSConfig(t.Context(), o)
					if err != nil {
						t.Fatal(err)
					}

					client, err := newOriginS3Client(cfg, o)
					if validateEndpoint(endpoint) != nil {
						if err == nil || !strings.HasPrefix(err.Error(), "invalid effective S3 endpoint:") || strings.Contains(err.Error(), endpoint) {
							t.Fatalf("error = %v, want redacted endpoint error", err)
						}

						if err := runOrigin(t.Context(), o); err == nil || !strings.HasPrefix(err.Error(), "invalid effective S3 endpoint:") {
							t.Fatalf("startup error = %v, want endpoint rejection before directory/socket setup", err)
						}

						return
					}

					if err != nil {
						t.Fatal(err)
					}

					if got := aws.ToString(client.Options().BaseEndpoint); got != endpoint || !client.Options().UsePathStyle {
						t.Fatalf("endpoint = %q, want %q with path style", got, endpoint)
					}
				})
			}
		})
	}
}

func setOriginEndpointSource(t *testing.T, dir, source, endpoint string) {
	t.Helper()

	switch source {
	case "global-env":
		t.Setenv("AWS_ENDPOINT_URL", endpoint)
	case "service-env":
		t.Setenv("AWS_ENDPOINT_URL_S3", endpoint)
	case "global-shared", "service-shared":
		text := "[default]\nendpoint_url = " + endpoint + "\n"
		if source == "service-shared" {
			text = "[default]\nservices = local\n[services local]\ns3 =\n  endpoint_url = " + endpoint + "\n"
		}

		if err := os.WriteFile(filepath.Join(dir, "config"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown endpoint source")
	}
}

func TestOriginEndpointOverride(t *testing.T) {
	for _, source := range []string{"global-env", "service-env", "global-shared", "service-shared"} {
		t.Run(source, func(t *testing.T) {
			dir := isolateOriginAWSConfig(t)
			setOriginEndpointSource(t, dir, source, "https://private-user:private-password@s3.example/private-path?secret=value")

			o := options{region: "us-east-1", requestTimeout: time.Minute, endpoint: "https://override.example"}

			cfg, err := originAWSConfig(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}

			client, err := newOriginS3Client(cfg, o)
			if err != nil {
				t.Fatal(err)
			}

			if got := aws.ToString(client.Options().BaseEndpoint); got != o.endpoint {
				t.Fatalf("endpoint = %q, want flag override %q", got, o.endpoint)
			}
		})
	}
}
