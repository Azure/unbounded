// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"

	signalv1alpha1 "github.com/Azure/unbounded/api/signal/v1alpha1"
	"github.com/Azure/unbounded/internal/signalplan"
)

var certificationTestResource = schema.GroupVersionResource{
	Group:    signalv1alpha1.GroupVersion.Group,
	Version:  signalv1alpha1.GroupVersion.Version,
	Resource: "certificationtests",
}

type bindingsOptions struct {
	filename    string
	configFlags *genericclioptions.ConfigFlags
}

func newBindingsCommand(configFlags *genericclioptions.ConfigFlags) *cobra.Command {
	options := &bindingsOptions{
		configFlags: configFlags,
	}
	cmd := &cobra.Command{
		Use:   "bindings -f PLAN",
		Short: "Capture accepted Test identities for Suite compilation",
		Long: "Read a CertificationPlan, resolve each generated CertificationTest from the current cluster, " +
			"and emit deterministic CertificationPlanBindings after verifying the live generation has an " +
			"Approved or Diagnostic trust identity.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			planName, testNames, err := signalplan.BindingTargets(options.filename)
			if err != nil {
				return err
			}

			config, err := options.configFlags.ToRESTConfig()
			if err != nil {
				return fmt.Errorf("load Kubernetes configuration: %w", err)
			}

			client, err := dynamic.NewForConfig(config)
			if err != nil {
				return fmt.Errorf("create Kubernetes client: %w", err)
			}

			references, err := resolveTestBindings(cmd.Context(), client, testNames)
			if err != nil {
				return err
			}

			rendered, err := signalplan.RenderBindings(planName, references)
			if err != nil {
				return err
			}

			if _, err := cmd.OutOrStdout().Write(rendered); err != nil {
				return fmt.Errorf("write CertificationPlanBindings: %w", err)
			}

			return nil
		},
	}
	cmd.Flags().StringVarP(&options.filename, "filename", "f", "", "CertificationPlan YAML file")
	configFlags.AddFlags(cmd.Flags())

	if err := cmd.MarkFlagRequired("filename"); err != nil {
		panic(err)
	}

	return cmd
}

func resolveTestBindings(
	ctx context.Context,
	client dynamic.Interface,
	testNames []string,
) (map[string]signalv1alpha1.CertificationTestReference, error) {
	references := make(map[string]signalv1alpha1.CertificationTestReference, len(testNames))

	for _, name := range testNames {
		test, err := client.Resource(certificationTestResource).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get CertificationTest %q: %w", name, err)
		}

		observedGeneration, found, err := unstructured.NestedInt64(
			test.Object,
			"status",
			"observedGeneration",
		)
		if err != nil {
			return nil, fmt.Errorf(
				"read CertificationTest %q status.observedGeneration: %w",
				name,
				err,
			)
		}

		if !found || observedGeneration != test.GetGeneration() {
			return nil, fmt.Errorf(
				"CertificationTest %q generation %d has not been observed",
				name,
				test.GetGeneration(),
			)
		}

		trustState, found, err := unstructured.NestedString(test.Object, "status", "trust", "state")
		if err != nil {
			return nil, fmt.Errorf("read CertificationTest %q trust state: %w", name, err)
		}

		if !found {
			return nil, fmt.Errorf("CertificationTest %q has no trust decision", name)
		}

		switch signalv1alpha1.CertificationTestTrustState(trustState) {
		case signalv1alpha1.CertificationTestTrustApproved,
			signalv1alpha1.CertificationTestTrustDiagnostic:
		case signalv1alpha1.CertificationTestTrustRejected,
			signalv1alpha1.CertificationTestTrustRevoked:
			return nil, fmt.Errorf(
				"CertificationTest %q trust state is %s",
				name,
				trustState,
			)
		default:
			return nil, fmt.Errorf(
				"CertificationTest %q has invalid trust state %q",
				name,
				trustState,
			)
		}

		contentDigest, found, err := unstructured.NestedString(
			test.Object,
			"status",
			"trust",
			"contentDigest",
		)
		if err != nil {
			return nil, fmt.Errorf("read CertificationTest %q content digest: %w", name, err)
		}

		if !found || !signalv1alpha1.IsSHA256Digest(contentDigest) {
			return nil, fmt.Errorf(
				"CertificationTest %q has no valid trusted content digest",
				name,
			)
		}

		if test.GetUID() == "" {
			return nil, errors.New("CertificationTest " + name + " has no UID")
		}

		references[name] = signalv1alpha1.CertificationTestReference{
			Name:          name,
			UID:           test.GetUID(),
			Generation:    test.GetGeneration(),
			ContentDigest: contentDigest,
		}
	}

	return references, nil
}
