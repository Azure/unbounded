// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/go-logr/logr"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	netmanifests "github.com/Azure/unbounded/deploy/net"
)

const (
	// crdKind is the Kind of a CustomResourceDefinition object in the embedded
	// manifests.
	crdKind = "CustomResourceDefinition"

	// crdEstablishedPoll is the poll interval while waiting for CRDs to become
	// Established.
	crdEstablishedPoll = time.Second

	// crdEstablishedTimeout preserves the full establishment window after the
	// apply phase while CRDBootstrapTimeout bounds the complete operation.
	crdEstablishedTimeout = 2 * time.Minute

	defaultCRDMaintenanceInterval = time.Minute
)

// CRDBootstrapTimeout bounds the complete CRD bootstrap, including manifest
// applies and waiting for every CRD to be served by the apiserver. The operator
// health server only binds after bootstrap, so the startupProbe budget in
// deploy/unbounded-operator/04-deployment.yaml.tmpl must exceed this timeout.
const CRDBootstrapTimeout = 4 * time.Minute

// RequiredCRDNames is the complete set of CRDs owned and bootstrapped by the
// unbounded-operator.
var RequiredCRDNames = [...]string{
	"machines.unbounded-cloud.io",
	"machineoperations.unbounded-cloud.io",
	"sites.unbounded-cloud.io",
	"machineconfigurations.unbounded-cloud.io",
	"machineoperationcredentials.unbounded-cloud.io",
	"machineconfigurationversions.unbounded-cloud.io",
	"sitenodeslices.net.unbounded-cloud.io",
	"gatewaypools.net.unbounded-cloud.io",
	"gatewaypoolnodes.net.unbounded-cloud.io",
	"sitegatewaypoolassignments.net.unbounded-cloud.io",
	"sitepeerings.net.unbounded-cloud.io",
	"gatewaypoolpeerings.net.unbounded-cloud.io",
}

// bootstrapManifestSets returns the embedded manifest filesystems whose CRDs the
// operator installs at startup. The operator owns CRD lifecycle so a cluster can
// be maintained by applying the operator manifests alone; the reconcile loop no
// longer applies CRDs.
func bootstrapManifestSets() []fs.FS {
	return []fs.FS{machinamanifests.Manifests, netmanifests.Manifests}
}

// BootstrapCRDs server-side applies every CustomResourceDefinition embedded in
// the machina and net manifests and waits for each to become Established. It is
// idempotent (safe to run on every operator start) and must run before the
// manager starts, because the typed Site informer cannot sync until the Site CRD
// is served.
func BootstrapCRDs(ctx context.Context, c client.Client) error {
	return bootstrapCRDs(ctx, c, CRDBootstrapTimeout)
}

// CRDMaintainer periodically reapplies the operator-owned CRDs using an
// uncached client. Maintenance failures are logged and retried on the next
// interval; they never stop the manager. CRDs that are already established stay
// served by the apiserver regardless of the operator's liveness, so stopping on
// maintenance failures would needlessly take down the Site reconciler and the
// migration reaper for what is typically a transient apiserver blip.
type CRDMaintainer struct {
	Client    client.Client
	Interval  time.Duration
	Bootstrap func(context.Context, client.Client) error
}

// NeedLeaderElection ensures only the elected operator replica maintains CRDs.
func (*CRDMaintainer) NeedLeaderElection() bool { return true }

// Start runs CRD maintenance until the manager stops (context cancellation).
// Maintenance failures are logged and retried on the next interval; they do not
// stop the manager.
func (m *CRDMaintainer) Start(ctx context.Context) error {
	interval := m.Interval
	if interval <= 0 {
		interval = defaultCRDMaintenanceInterval
	}

	bootstrap := m.Bootstrap
	if bootstrap == nil {
		bootstrap = BootstrapCRDs
	}

	logger := log.FromContext(ctx).WithName("crd-maintainer")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	failures := 0

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := bootstrap(ctx, m.Client); err != nil {
				if ctx.Err() != nil {
					return nil
				}

				failures++

				logger.Error(err, "CRD maintenance failed; will retry on the next interval", "consecutiveFailures", failures)

				continue
			}

			failures = 0
		}
	}
}

func bootstrapCRDs(ctx context.Context, c client.Client, timeout time.Duration) error {
	bootstrapCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ctx = bootstrapCtx
	logger := log.FromContext(ctx).WithName("crd-bootstrap")

	appliedCount := 0

	for _, manifests := range bootstrapManifestSets() {
		applied, err := applyCRDsFromFS(ctx, logger, c, manifests)
		if err != nil {
			return err
		}

		appliedCount += len(applied)
	}

	if appliedCount == 0 {
		return nil
	}

	if err := waitForCRDsEstablished(ctx, c, RequiredCRDNames[:]); err != nil {
		return err
	}

	logger.Info("CRDs installed and established", "count", appliedCount)

	return nil
}

// applyCRDsFromFS applies every CRD found in the given manifest filesystem and
// returns the names it applied.
func applyCRDsFromFS(ctx context.Context, logger logr.Logger, c client.Client, manifests fs.FS) ([]string, error) {
	files, err := yamlFiles(manifests)
	if err != nil {
		return nil, err
	}

	var names []string

	for _, file := range files {
		data, err := fs.ReadFile(manifests, file)
		if err != nil {
			return nil, fmt.Errorf("read manifest %s: %w", file, err)
		}

		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

		for {
			obj := &unstructured.Unstructured{}
			if err := decoder.Decode(obj); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				return nil, fmt.Errorf("decode %s: %w", file, err)
			}

			if obj.Object == nil || obj.GetKind() != crdKind {
				continue
			}

			applyCfg := client.ApplyConfigurationFromUnstructured(obj)
			if err := c.Apply(ctx, applyCfg, client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
				return nil, fmt.Errorf("apply CRD %s: %w", obj.GetName(), err)
			}

			logger.Info("applied CRD", "name", obj.GetName())
			names = append(names, obj.GetName())
		}
	}

	return names, nil
}

// waitForCRDsEstablished blocks until every named CRD reports the Established
// condition or its context ends.
func waitForCRDsEstablished(ctx context.Context, c client.Client, names []string) error {
	waitCtx, cancel := context.WithTimeout(ctx, crdEstablishedTimeout)
	defer cancel()

	return wait.PollUntilContextCancel(waitCtx, crdEstablishedPoll, true, func(ctx context.Context) (bool, error) {
		for _, name := range names {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := c.Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}

				return false, err
			}

			if crd.DeletionTimestamp != nil {
				return false, fmt.Errorf("customresourcedefinition %s is being deleted", name)
			}

			if !crdEstablished(crd) {
				return false, nil
			}
		}

		return true, nil
	})
}

// crdEstablished reports whether a CRD has the Established=True condition.
func crdEstablished(crd *apiextensionsv1.CustomResourceDefinition) bool {
	if crd.DeletionTimestamp != nil {
		return false
	}

	for _, cond := range crd.Status.Conditions {
		if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
			return true
		}
	}

	return false
}
