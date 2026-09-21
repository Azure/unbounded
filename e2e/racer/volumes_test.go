//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"

	racermeta "github.com/Azure/unbounded/internal/racer"
)

// Origin Services are not volumes. Any fixture declaring an origin or selecting
// Racer dataplanes must carry the explicit annotation required by the controller.
func validateFixtureVolume(s *core.Service) error {
	if s.Annotations[racermeta.OriginServiceAnnotationKey] == "" && s.Spec.Selector[racermeta.DataplaneLabelKey] != "true" {
		return nil
	}

	universe := s.Annotations[racermeta.UniverseKey]
	if universe == "" || universe != s.Spec.Selector[racermeta.UniverseKey] {
		return fmt.Errorf("volume %s/%s requires explicit %s annotation matching its selector: annotation=%q selector=%q", s.Namespace, s.Name, racermeta.UniverseKey, universe, s.Spec.Selector[racermeta.UniverseKey])
	}

	return nil
}

func TestFixtureVolumeUniverses(t *testing.T) {
	for _, site := range []string{primarySite, "racer-b", "default", strings.Repeat("a", 63) + ".long"} {
		t.Run("builder/"+site, func(t *testing.T) {
			s := volumeService("volume", "origin", site)
			if err := validateFixtureVolume(s); err != nil {
				t.Fatal(err)
			}

			if s.Annotations[racermeta.UniverseKey] != racermeta.UniverseForSite(site) {
				t.Fatal("volume must use the mapped Site universe")
			}
		})
	}

	volumes := 0

	err := filepath.WalkDir(filepath.Join(repository(t), "e2e", "racer", "examples"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".yml") {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)

		for {
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err == io.EOF {
				break
			} else if err != nil {
				return err
			}

			if len(raw) == 0 || string(raw) == "null" {
				continue
			}

			var typ meta.TypeMeta
			if err := json.Unmarshal(raw, &typ); err != nil {
				return err
			}

			if typ.Kind != "Service" {
				continue
			}

			var s core.Service
			if err := json.Unmarshal(raw, &s); err != nil {
				return err
			}

			if s.Spec.Selector[racermeta.DataplaneLabelKey] == "true" {
				volumes++
			}

			t.Run(filepath.Base(path)+"/"+s.Name, func(t *testing.T) {
				if err := validateFixtureVolume(&s); err != nil {
					t.Fatal(err)
				}
			})
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if volumes < 2 {
		t.Fatalf("checked %d example volumes, want at least volume and loadgen", volumes)
	}

	for _, missing := range []string{"annotation", "selector", "mismatch"} {
		t.Run("reject/"+missing, func(t *testing.T) {
			s := volumeService("invalid", "origin", primarySite)
			s.Annotations[racermeta.UniverseKey] = racermeta.UniverseForSite(primarySite)

			switch missing {
			case "annotation":
				delete(s.Annotations, racermeta.UniverseKey)
			case "selector":
				delete(s.Spec.Selector, racermeta.UniverseKey)
			case "mismatch":
				s.Annotations[racermeta.UniverseKey] = "foreign"
			}

			if validateFixtureVolume(s) == nil {
				t.Fatal("invalid fixture universe accepted")
			}
		})
	}
}
