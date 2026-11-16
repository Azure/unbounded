// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package manifests renders deployment assets and embeds CRDs from the pinned upstream module.
package manifests

import (
	"bytes"
	_ "embed"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/karpenter/pkg/apis"
)

//go:embed assets/controller.yaml.tmpl
var controllerTemplate string

var imageReference = regexp.MustCompile(
	`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|@sha256:[a-f0-9]{64})$`,
)

// Options are the values the deployment manifests are rendered with.
type Options struct {
	// Namespace is the controller's namespace.
	Namespace string
	// Image is the explicit controller image reference.
	Image string
	// PriceCurrency is the ISO 4217 currency whose offering prices are used
	// as Karpenter prices. Empty means USD.
	PriceCurrency string
}

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

// Render returns the Karpenter CRDs of the pinned upstream module followed by
// the plugin's service account, roles, admission policy and Deployment.
func Render(options Options) ([]byte, error) {
	if errors := validation.IsDNS1123Label(options.Namespace); len(errors) != 0 {
		return nil, fmt.Errorf("invalid namespace: %s", strings.Join(errors, ", "))
	}

	if !imageReference.MatchString(options.Image) {
		return nil, fmt.Errorf("controller image must be an explicit image reference")
	}

	if options.PriceCurrency == "" {
		options.PriceCurrency = "USD"
	}

	if !currencyCode.MatchString(options.PriceCurrency) {
		return nil, fmt.Errorf("price currency must be an ISO 4217 code")
	}

	tmpl, err := template.New("controller").Option("missingkey=error").Parse(controllerTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse controller manifest template: %w", err)
	}

	output := new(bytes.Buffer)
	for _, crd := range [][]byte{
		apis.NodePoolCRD,
		apis.NodeClaimCRD,
		apis.NodeOverlayCRD,
		apis.CapacityBufferCRD,
	} {
		output.Write(crd)
		output.WriteString("\n---\n")
	}

	if err := tmpl.Execute(output, options); err != nil {
		return nil, fmt.Errorf("render controller manifests: %w", err)
	}

	return output.Bytes(), nil
}
