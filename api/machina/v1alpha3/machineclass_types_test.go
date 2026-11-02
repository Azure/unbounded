// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

const machineClassCRD = "unbounded-cloud.io_machineclasses.yaml"

const validSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newMachineClassObject(t *testing.T) map[string]any {
	t.Helper()

	return parseObject(t, `
apiVersion: unbounded-cloud.io/v1alpha3
kind: MachineClass
metadata:
  name: general
spec:
  siteRef:
    name: site-a
  provider: AzureVM
  providerConfigRef:
    kind: ConfigMap
    namespace: unbounded-system
    name: azure-general
  configurationRef:
    name: default
    version: 1
`)
}

// lengthURL returns an https URL of exactly n characters.
func lengthURL(n int) string {
	const prefix = "https://h/"

	return prefix + strings.Repeat("a", n-len(prefix))
}

func TestMachineClassValidation(t *testing.T) {
	t.Parallel()

	v := newCRDValidator(t, machineClassCRD)

	withArtifact := func(url, sha string) func(*testing.T, map[string]any) {
		return func(t *testing.T, obj map[string]any) {
			setField(t, obj, map[string]any{"url": url, "sha256": sha}, "spec", "bootstrap", "agentArtifact")
		}
	}
	withURL := func(url string) func(*testing.T, map[string]any) {
		return withArtifact(url, validSHA256)
	}
	withSHA := func(sha string) func(*testing.T, map[string]any) {
		return withArtifact("https://host/a.tgz", sha)
	}
	set := func(value any, path ...string) func(*testing.T, map[string]any) {
		return func(t *testing.T, obj map[string]any) {
			setField(t, obj, value, path...)
		}
	}
	remove := func(path ...string) func(*testing.T, map[string]any) {
		return func(_ *testing.T, obj map[string]any) {
			unstructured.RemoveNestedField(obj, path...)
		}
	}

	const (
		kindMsg      = "providerConfigRef.kind must be ConfigMap"
		apiGroupMsg  = "providerConfigRef.apiGroup must be the core group"
		namespaceMsg = "providerConfigRef.namespace is required for a ConfigMap"
		urlPath      = "spec.bootstrap.agentArtifact.url"
		shaPath      = "spec.bootstrap.agentArtifact.sha256"
	)

	tests := []struct {
		name   string
		mutate func(*testing.T, map[string]any)
		want   []string
	}{
		{name: "minimal class", mutate: func(*testing.T, map[string]any) {}},
		{name: "empty apiGroup means core", mutate: set("", "spec", "providerConfigRef", "apiGroup")},
		{
			name:   "bootstrap with only filesRef",
			mutate: set(map[string]any{"namespace": "unbounded-system", "name": "bootstrap-files"}, "spec", "bootstrap", "filesRef"),
		},
		{name: "blob storage URL", mutate: withURL("https://account.blob.core.windows.net/agents/v1.2.3/unbounded-agent-linux-amd64.tar.gz")},
		{name: "s3 URL", mutate: withURL("https://bucket.s3.us-east-1.amazonaws.com/agent.tgz")},
		{name: "IPv4 host with port", mutate: withURL("https://10.0.0.5:8443/a/agent.tgz")},
		{name: "local file URL", mutate: withURL("file:///opt/unbounded/agent.tgz")},
		{name: "URL of 2048 characters", mutate: withURL(lengthURL(2048))},

		{name: "missing spec", mutate: remove("spec"), want: []string{"spec: Required value"}},
		{name: "missing siteRef", mutate: remove("spec", "siteRef"), want: []string{"spec.siteRef: Required value"}},
		{name: "empty provider", mutate: set("", "spec", "provider"), want: []string{"spec.provider: Invalid value", "at least 1 chars long"}},
		{name: "kind Secret", mutate: set("Secret", "spec", "providerConfigRef", "kind"), want: []string{kindMsg}},
		{name: "kind ConfigMaps", mutate: set("ConfigMaps", "spec", "providerConfigRef", "kind"), want: []string{kindMsg}},
		{name: "kind configmap", mutate: set("configmap", "spec", "providerConfigRef", "kind"), want: []string{kindMsg}},
		{name: "apiGroup apps", mutate: set("apps", "spec", "providerConfigRef", "apiGroup"), want: []string{apiGroupMsg}},
		{name: "apiGroup v1", mutate: set("v1", "spec", "providerConfigRef", "apiGroup"), want: []string{apiGroupMsg}},
		{
			name: "typed provider config kind",
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, map[string]any{"apiGroup": "karpenter.k8s.aws", "kind": "EC2NodeClass", "name": "default"}, "spec", "providerConfigRef")
			},
			want: []string{apiGroupMsg, kindMsg},
		},
		{name: "missing namespace", mutate: remove("spec", "providerConfigRef", "namespace"), want: []string{namespaceMsg}},
		{
			name:   "empty namespace",
			mutate: set("", "spec", "providerConfigRef", "namespace"),
			want:   []string{"spec.providerConfigRef.namespace: Invalid value", "at least 1 chars long"},
		},
		{
			name:   "empty name",
			mutate: set("", "spec", "providerConfigRef", "name"),
			want:   []string{"spec.providerConfigRef.name: Invalid value", "at least 1 chars long"},
		},
		{
			name:   "empty kind",
			mutate: set("", "spec", "providerConfigRef", "kind"),
			want:   []string{"spec.providerConfigRef.kind: Invalid value", "at least 1 chars long"},
		},
		{name: "missing name", mutate: remove("spec", "providerConfigRef", "name"), want: []string{"spec.providerConfigRef.name: Required value"}},
		{name: "missing kind", mutate: remove("spec", "providerConfigRef", "kind"), want: []string{"spec.providerConfigRef.kind: Required value"}},
		{
			name:   "configurationRef without version",
			mutate: remove("spec", "configurationRef", "version"),
			want:   []string{"spec.configurationRef", "configurationRef.version is required"},
		},
		{name: "http URL", mutate: withURL("http://host/a.tgz"), want: []string{urlPath}},
		{name: "userinfo in URL", mutate: withURL("https://user:pw@host/a.tgz"), want: []string{urlPath}},
		{name: "query in URL", mutate: withURL("https://host/a.tgz?sig=x"), want: []string{urlPath}},
		{name: "fragment in URL", mutate: withURL("https://host/a.tgz#x"), want: []string{urlPath}},
		{name: "URL without path", mutate: withURL("https://host"), want: []string{urlPath}},
		{name: "URL with root path", mutate: withURL("https://host/"), want: []string{urlPath}},
		{name: "URL with trailing slash", mutate: withURL("https://host/a/"), want: []string{urlPath}},
		{name: "file URL with host", mutate: withURL("file://host/a.tgz"), want: []string{urlPath}},
		{name: "ftp URL", mutate: withURL("ftp://host/a.tgz"), want: []string{urlPath}},
		{name: "URL with space", mutate: withURL("https://host/a b.tgz"), want: []string{urlPath}},
		{name: "URL with newline", mutate: withURL("https://host/a.tgz\nrm"), want: []string{urlPath}},
		{name: "URL with single quote", mutate: withURL("https://host/a'.tgz"), want: []string{urlPath}},
		{name: "URL with dollar", mutate: withURL("https://host/$HOME.tgz"), want: []string{urlPath}},
		{name: "URL with backtick", mutate: withURL("https://host/a`id`.tgz"), want: []string{urlPath}},
		{name: "URL of 2049 characters", mutate: withURL(lengthURL(2049)), want: []string{urlPath, "Too long"}},
		{name: "uppercase sha256", mutate: withSHA(strings.ToUpper(validSHA256)), want: []string{shaPath}},
		{name: "sha256 of 63 characters", mutate: withSHA(validSHA256[:63]), want: []string{shaPath}},
		{name: "sha256 of 65 characters", mutate: withSHA(validSHA256 + "0"), want: []string{shaPath}},
		{name: "sha256 with g", mutate: withSHA("g" + validSHA256[1:]), want: []string{shaPath}},
		{
			name:   "missing url",
			mutate: set(map[string]any{"sha256": validSHA256}, "spec", "bootstrap", "agentArtifact"),
			want:   []string{urlPath + ": Required value"},
		},
		{
			name:   "missing sha256",
			mutate: set(map[string]any{"url": "https://host/a.tgz"}, "spec", "bootstrap", "agentArtifact"),
			want:   []string{shaPath + ": Required value"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			obj := newMachineClassObject(t)
			tt.mutate(t, obj)
			assertErrors(t, v.validateCreate(obj), tt.want)
		})
	}
}

func TestMachineClassSpecMutable(t *testing.T) {
	t.Parallel()

	v := newCRDValidator(t, machineClassCRD)

	tests := []struct {
		name   string
		stored func(*testing.T, map[string]any)
		mutate func(*testing.T, map[string]any)
		want   []string
	}{
		{
			name: "change configurationRef version",
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, int64(2), "spec", "configurationRef", "version")
			},
		},
		{
			name: "change configurationRef name",
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "gpu", "spec", "configurationRef", "name")
			},
		},
		{
			name: "change providerConfigRef name",
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "azure-general-v2", "spec", "providerConfigRef", "name")
			},
		},
		{
			name: "change providerConfigRef namespace",
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "other", "spec", "providerConfigRef", "namespace")
			},
		},
		{
			name: "add bootstrap",
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, map[string]any{"namespace": "unbounded-system", "name": "files"}, "spec", "bootstrap", "filesRef")
			},
		},
		{
			name: "change bootstrap",
			stored: func(t *testing.T, obj map[string]any) {
				setField(t, obj, map[string]any{"namespace": "unbounded-system", "name": "files"}, "spec", "bootstrap", "filesRef")
			},
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "files-v2", "spec", "bootstrap", "filesRef", "name")
				setField(t, obj, map[string]any{"url": "file:///opt/agent.tgz", "sha256": validSHA256}, "spec", "bootstrap", "agentArtifact")
			},
		},
		{
			name: "remove bootstrap",
			stored: func(t *testing.T, obj map[string]any) {
				setField(t, obj, map[string]any{"namespace": "unbounded-system", "name": "files"}, "spec", "bootstrap", "filesRef")
			},
			mutate: func(_ *testing.T, obj map[string]any) {
				unstructured.RemoveNestedField(obj, "spec", "bootstrap")
			},
		},
		{
			name: "change version with provider and siteRef unchanged",
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, int64(3), "spec", "configurationRef", "version")
				setField(t, obj, "AzureVM", "spec", "provider")
				setField(t, obj, "site-a", "spec", "siteRef", "name")
			},
		},
		{
			name: "identical spec with label added",
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "x", "metadata", "labels", "team")
			},
		},
		{
			name: "change provider",
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "AWSEC2", "spec", "provider")
			},
			want: []string{"provider is immutable"},
		},
		{
			name: "change siteRef name",
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "site-b", "spec", "siteRef", "name")
			},
			want: []string{"siteRef is immutable"},
		},
		{
			name: "change provider and siteRef",
			mutate: func(t *testing.T, obj map[string]any) {
				setField(t, obj, "AWSEC2", "spec", "provider")
				setField(t, obj, "site-b", "spec", "siteRef", "name")
			},
			want: []string{"provider is immutable", "siteRef is immutable"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stored := newMachineClassObject(t)
			if tt.stored != nil {
				tt.stored(t, stored)
			}

			if errs := v.validateCreate(stored); len(errs) > 0 {
				t.Fatalf("stored class is invalid: %v", errs.ToAggregate())
			}

			updated := runtime.DeepCopyJSON(stored)
			tt.mutate(t, updated)
			assertErrors(t, v.validateUpdate(updated, stored), tt.want)
		})
	}
}
