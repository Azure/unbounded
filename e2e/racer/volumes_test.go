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

	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func validateFixtureCache(cache *racerapi.P2PCache) error {
	if cache.Namespace != "" {
		return fmt.Errorf("P2PCache must be cluster scoped")
	}

	if _, _, err := racermeta.CacheSockets(racermeta.SocketRoot, cache.Name); err != nil {
		return err
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

			if typ.Kind != "P2PCache" {
				continue
			}

			var cache racerapi.P2PCache
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
		t.Fatalf("checked %d P2PCache examples, want 2", count)
	}
}
