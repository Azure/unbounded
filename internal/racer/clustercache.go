// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"path/filepath"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerapi "github.com/Azure/unbounded/api/racer/v1alpha1"
)

const (
	SocketRoot        = "/run/racer"
	SlotCount  uint32 = 262144
)

// CacheSockets derives filesystem sockets from the exact metadata UID, never the
// resource name. A UID must be a nonempty lowercase ASCII alphanumeric component
// with optional interior hyphens, at most 63 bytes. Linux sockaddr_un reserves one byte
// for the terminating NUL; abstract sockets and relative roots are not supported.
func CacheSockets(root, uid string) (cache, origin string, err error) {
	if !filepath.IsAbs(root) || strings.ContainsRune(root, 0) || len(validation.IsDNS1123Label(uid)) != 0 {
		return "", "", fmt.Errorf("socket root must be absolute and cache UID must be a lowercase DNS label of at most 63 bytes")
	}

	cache = filepath.Join(root, uid, "cache")

	origin = filepath.Join(root, uid, "origin")
	if len(cache) > 107 || len(origin) > 107 {
		return "", "", fmt.Errorf("derived socket path exceeds 107 bytes")
	}

	return cache, origin, nil
}

// CacheSelectsSite evaluates Site object labels, independently of Node labels.
func CacheSelectsSite(cache *racerapi.ClusterCache, site *machina.Site) (bool, error) {
	selector, err := metav1.LabelSelectorAsSelector(&cache.Spec.SiteSelector)
	if err != nil {
		return false, err
	}

	return cache.DeletionTimestamp == nil && site != nil && site.DeletionTimestamp == nil && selector.Matches(labels.Set(site.Labels)), nil
}
