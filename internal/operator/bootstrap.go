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

	// crdEstablishedTimeout bounds how long BootstrapCRDs waits for every applied
	// CRD to be served by the apiserver before giving up.
	crdEstablishedTimeout = 2 * time.Minute

	// crdEstablishedPoll is the poll interval while waiting for CRDs to become
	// Established.
	crdEstablishedPoll = time.Second
)

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
	logger := log.FromContext(ctx).WithName("crd-bootstrap")

	var names []string

	for _, manifests := range bootstrapManifestSets() {
		applied, err := applyCRDsFromFS(ctx, logger, c, manifests)
		if err != nil {
			return err
		}

		names = append(names, applied...)
	}

	if len(names) == 0 {
		return nil
	}

	if err := waitForCRDsEstablished(ctx, c, names); err != nil {
		return err
	}

	logger.Info("CRDs installed and established", "count", len(names))

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
// condition or the timeout elapses.
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

			if !crdEstablished(crd) {
				return false, nil
			}
		}

		return true, nil
	})
}

// crdEstablished reports whether a CRD has the Established=True condition.
func crdEstablished(crd *apiextensionsv1.CustomResourceDefinition) bool {
	for _, cond := range crd.Status.Conditions {
		if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
			return true
		}
	}

	return false
}
