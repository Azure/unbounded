// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"github.com/Azure/unbounded/internal/operator/components/machina"
	netcomponent "github.com/Azure/unbounded/internal/operator/components/net"
	"github.com/Azure/unbounded/internal/operator/components/storage"
)

// The legacy reaper (migrate.go) verifies component rollout by comparing the
// pod-template config-hash annotations the components stamp, and migrates the
// machina config using the same endpoint-merge logic. These aliases keep that
// single source of truth in the component packages while the reaper references
// the historical operator-local names.
const (
	netConfigHashAnnotation     = netcomponent.ConfigHashAnnotation
	machinaConfigHashAnnotation = machina.ConfigHashAnnotation
	storageConfigHashAnnotation = storage.ConfigHashAnnotation
)

// setMachinaAPIServerEndpoint forwards to the machina component's endpoint merge
// so the reaper and the component write machina config identically.
func setMachinaAPIServerEndpoint(config, endpoint string) (string, error) {
	return machina.SetAPIServerEndpoint(config, endpoint)
}
