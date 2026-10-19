// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	certificatesv1 "k8s.io/api/certificates/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/unbounded"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

const (
	// metalmanBootstrapServiceAccount is the ServiceAccount whose tokens
	// metalman mints for TPM-attested machines.
	metalmanBootstrapServiceAccount = "metalman-bootstrap"

	machineBootstrapCredentialIDField = "status.bootstrap.credentialID"
)

// metalmanBootstrapUsername returns the CSR requester username of tokens
// minted by metalman for attested machines.
func metalmanBootstrapUsername() string {
	return "system:serviceaccount:" + unbounded.SystemNamespace() + ":" + metalmanBootstrapServiceAccount
}

type metalmanCSRClaimChecker struct {
	client.Client
}

// NewMetalmanCSRApprover returns a reconciler that approves kubelet client
// CSRs submitted with a metalman bootstrap token, but only for the node name
// of the single Machine the token was issued to.
func NewMetalmanCSRApprover(
	c client.Client,
	kubeClient kubernetes.Interface,
) (*daemoncred.CSRApproverReconciler, error) {
	claims := &metalmanCSRClaimChecker{Client: c}

	approver, err := daemoncred.NewKubeletClientCSRApprover(daemoncred.KubeletClientCSRApproverOptions{
		Requester:     metalmanBootstrapUsername(),
		AuthorizeNode: claims.credentialMayClaimNode,
	})
	if err != nil {
		return nil, err
	}

	r, err := daemoncred.NewCSRApproverReconciler(c, kubeClient, approver)
	if err != nil {
		return nil, err
	}

	r.Name = "metalman-csr-approver"

	return r, nil
}

func (c *metalmanCSRClaimChecker) credentialMayClaimNode(
	ctx context.Context,
	_ *certificatesv1.CertificateSigningRequest,
	nodeName, credentialID string,
) (bool, string, error) {
	var machines unboundedv1alpha3.MachineList
	if err := c.List(ctx, &machines, client.MatchingFields{machineBootstrapCredentialIDField: credentialID}); err != nil {
		return false, "", fmt.Errorf("list Machines by bootstrap credential ID: %w", err)
	}

	switch len(machines.Items) {
	case 0:
		return false, "no Machine was issued this credential", nil
	case 1:
	default:
		return false, fmt.Sprintf("credential is recorded on %d Machines", len(machines.Items)), nil
	}

	machine := &machines.Items[0]
	if machine.NodeName() != nodeName {
		return false, fmt.Sprintf("credential was issued to Machine %q", machine.Name), nil
	}

	return true, "", nil
}

func machineBootstrapCredentialID(obj client.Object) []string {
	machine, ok := obj.(*unboundedv1alpha3.Machine)
	if !ok || machine.Status.Bootstrap == nil || machine.Status.Bootstrap.CredentialID == "" {
		return nil
	}

	return []string{machine.Status.Bootstrap.CredentialID}
}
