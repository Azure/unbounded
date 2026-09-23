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
	SocketRoot        = "/dev/racer"
	SlotCount  uint32 = 262144
)

// CacheSockets derives filesystem sockets. Linux sockaddr_un reserves one byte
// for the terminating NUL; abstract sockets and relative roots are not supported.
func CacheSockets(root, name string) (cache, origin string, err error) {
	if !filepath.IsAbs(root) || strings.ContainsRune(root, 0) || len(validation.IsDNS1123Label(name)) != 0 {
		return "", "", fmt.Errorf("socket root must be absolute and cache name must be a DNS label")
	}

	cache = filepath.Join(root, name, "cache")

	origin = filepath.Join(root, name, "origin")
	if len(cache) > 107 || len(origin) > 107 {
		return "", "", fmt.Errorf("derived socket path exceeds 107 bytes")
	}

	return cache, origin, nil
}

// CacheSelectsSite evaluates Site object labels, independently of Node labels.
func CacheSelectsSite(cache *racerapi.P2PCache, site *machina.Site) (bool, error) {
	selector, err := metav1.LabelSelectorAsSelector(&cache.Spec.SiteSelector)
	if err != nil {
		return false, err
	}

	return cache.DeletionTimestamp == nil && site != nil && site.DeletionTimestamp == nil && selector.Matches(labels.Set(site.Labels)), nil
}
