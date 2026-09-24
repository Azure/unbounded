//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/yaml"

	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func validateFixtureCache(cache *racerapi.ClusterCache) error {
	if cache.Namespace != "" {
		return fmt.Errorf("ClusterCache must be cluster scoped")
	}

	// These are pre-create resources. Socket identity is assigned by the API.
	if problems := validation.IsDNS1123Label(cache.Name); len(problems) != 0 {
		return fmt.Errorf("invalid cache name: %v", problems)
	}

	_, err := meta.LabelSelectorAsSelector(&cache.Spec.SiteSelector)

	return err
}

func TestFixtureCacheSiteSelectors(t *testing.T) {
	for _, site := range []string{primarySite, "racer-b", "default"} {
		cache := cacheResource("volume", site)
		if err := validateFixtureCache(cache); err != nil {
			t.Fatal(err)
		}

		if selected, err := racermeta.CacheSelectsSite(cache, testSite(site)); err != nil || !selected {
			t.Fatalf("Site not selected: %v", err)
		}

		if selected, err := racermeta.CacheSelectsSite(cache, testSite("foreign")); err != nil || selected {
			t.Fatalf("foreign Site selected: %v", err)
		}
	}

	count := 0

	for _, name := range []string{"volume", "loadgen"} {
		f, err := os.Open(filepath.Join(repository(t), "e2e/racer/examples", name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()

		decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)

		for {
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}

			var typ meta.TypeMeta
			if err := json.Unmarshal(raw, &typ); err != nil {
				t.Fatal(err)
			}

			if typ.Kind != "ClusterCache" {
				if name == "loadgen" && typ.Kind == "DaemonSet" {
					var ds appsv1.DaemonSet
					if err := json.Unmarshal(raw, &ds); err != nil {
						t.Fatal(err)
					}

					container := ds.Spec.Template.Spec.Containers[0]
					if container.Args[0] != "-cache-uid=$(RACER_CACHE_UID)" || container.Env[0].Name != "RACER_CACHE_UID" || container.Env[0].ValueFrom == nil || container.Env[0].ValueFrom.ConfigMapKeyRef == nil || container.Env[0].ValueFrom.ConfigMapKeyRef.Name != "racer-loadgen-cache" || container.Env[0].ValueFrom.ConfigMapKeyRef.Key != "uid" {
						t.Fatal("loadgen must use an externally supplied API-assigned UID")
					}

					mount := container.VolumeMounts[0]
					if mount.MountPath != racermeta.SocketRoot || mount.SubPath != "" || ds.Spec.Template.Spec.Volumes[0].HostPath.Path != racermeta.SocketRoot {
						t.Fatal("loadgen must mount the restart-safe socket root")
					}
				}

				continue
			}

			var cache racerapi.ClusterCache
			if err := json.Unmarshal(raw, &cache); err != nil {
				t.Fatal(err)
			}

			if err := validateFixtureCache(&cache); err != nil {
				t.Fatal(err)
			}

			if selected, err := racermeta.CacheSelectsSite(&cache, testSite(primarySite)); err != nil || !selected {
				t.Fatalf("example does not select Site: %v", err)
			}

			count++
		}
	}

	if count != 2 {
		t.Fatalf("checked %d ClusterCache examples, want 2", count)
	}
}
