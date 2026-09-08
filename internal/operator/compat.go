// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/components/machina"
	"github.com/Azure/unbounded/internal/operator/components/metalman"
	"github.com/Azure/unbounded/internal/operator/components/storage"
)

// Component name identifiers. These mirror each component's Name() and are used
// by the legacy reaper (migrate.go) to gate per-component migration.
const (
	ComponentNet      = "net"
	ComponentMachina  = "machina"
	ComponentMetalman = "metalman"
	ComponentStorage  = "storage"
)

// Per-site resource name helpers, sourced from the component packages so the
// reaper and the components agree on the object names.
var (
	metalmanDeploymentName = metalman.DeploymentName
	storageConfigName      = storage.SiteConfigName
	storageDaemonSetName   = storage.SiteDaemonSetName
)

// componentEnabled reports whether a Site enables the named component. It is used
// by the legacy reaper to decide whether a component's per-site resources should
// exist in the target namespace.
func componentEnabled(site *unboundedv1alpha3.Site, name string) bool {
	switch name {
	case ComponentMachina:
		return machina.EnabledFor(site)
	case ComponentMetalman:
		return metalman.Component{}.Enabled(site)
	case ComponentStorage:
		return storage.Component{}.Enabled(site)
	default:
		return false
	}
}
