// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/unbounded"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

const (
	bootstrapSecretPref        = "bootstrap-token-"
	defaultBootstrapTokenLabel = "unbounded-cloud.io/default-bootstrap-token"
	daemonGroup                = "unbounded-agent-daemons"
	bootstrapGroup             = "system:bootstrappers:unbounded-agent-daemons"

	machineNodeRefNameField = "spec.kubernetes.nodeRef.name"

	metalmanBootstrapServiceAccount = "metalman-bootstrap"
)

type daemonCSRClaimChecker struct {
	client.Client
	APIReader client.Reader
}

func NewDaemonCSRApprover(
	c client.Client,
	apiReader client.Reader,
	kubeClient kubernetes.Interface,
) (*daemoncred.CSRApproverReconciler, error) {
	if apiReader == nil {
		return nil, fmt.Errorf("daemon CSR approver requires an API reader")
	}

	claims := &daemonCSRClaimChecker{Client: c, APIReader: apiReader}

	approver, err := daemoncred.NewCSRApprover(daemoncred.CSRApproverOptions{
		BootstrapGroup:               bootstrapGroup,
		DaemonGroup:                  daemonGroup,
		AuthorizeBootstrap:           claims.bootstrapTokenMayClaimNode,
		CredentialBootstrapRequester: metalmanBootstrapUsername(),
		AuthorizeCredentialBootstrap: claims.bootstrapCredentialMayClaimNode,
		AuthorizeRenewal:             claims.nodeHasMachineBinding,
	})
	if err != nil {
		return nil, err
	}

	return daemoncred.NewCSRApproverReconciler(c, kubeClient, approver)
}

func (c *daemonCSRClaimChecker) bootstrapTokenMayClaimNode(
	ctx context.Context,
	csr *certificatesv1.CertificateSigningRequest,
	nodeName string,
) (bool, error) {
	tokenID := strings.TrimPrefix(csr.Spec.Username, daemoncred.BootstrapUserPrefix)
	if tokenID == "" {
		return false, nil
	}

	secretName := bootstrapSecretPref + tokenID

	var token corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: secretName}, &token); err != nil {
		return false, client.IgnoreNotFound(err)
	}

	if token.Type != corev1.SecretTypeBootstrapToken {
		return false, nil
	}

	siteName := strings.TrimSpace(token.Labels[unboundedv1alpha3.MachineSiteLabelKey])

	isDefaultToken := strings.EqualFold(strings.TrimSpace(token.Labels[defaultBootstrapTokenLabel]), "true")
	if siteName == "" && !isDefaultToken {
		return false, nil
	}

	// Follow-up: consider removing default-token authorization so daemon
	// bootstrap is always explicitly site-scoped.
	// Bootstrap tokens are site-scoped credentials, not single-Machine leases.
	// During the token's valid time window, multiple nodes in that site may use it
	// to obtain daemon-controller credentials and create or bind their Machines.
	return true, nil
}

func (c *daemonCSRClaimChecker) bootstrapCredentialMayClaimNode(
	ctx context.Context,
	_ *certificatesv1.CertificateSigningRequest,
	nodeName, credentialID string,
) (bool, error) {
	var machines unboundedv1alpha3.MachineList
	if err := c.APIReader.List(ctx, &machines); err != nil {
		return false, fmt.Errorf("list live Machines for bootstrap credential claim check: %w", err)
	}

	var credentialMachine *unboundedv1alpha3.Machine

	for i := range machines.Items {
		machine := &machines.Items[i]
		if machine.Status.Bootstrap == nil || machine.Status.Bootstrap.CredentialID != credentialID {
			continue
		}

		if credentialMachine != nil {
			return false, nil
		}

		credentialMachine = machine
	}

	return credentialMachine != nil &&
		credentialMachine.Status.Bootstrap.NodeName != "" &&
		credentialMachine.Status.Bootstrap.NodeName == nodeName, nil
}

func (c *daemonCSRClaimChecker) nodeHasMachineBinding(
	ctx context.Context,
	_ *certificatesv1.CertificateSigningRequest,
	nodeName string,
) (bool, error) {
	// Renewal requires an explicit Machine -> Node binding. Do not fall back to
	// Machine name: clearing NodeRef must sever daemon-controller renewal.
	var machines unboundedv1alpha3.MachineList
	if err := c.List(ctx, &machines, client.MatchingFields{machineNodeRefNameField: nodeName}); err != nil {
		return false, fmt.Errorf("list Machines by node ref for renewal claim check: %w", err)
	}

	return len(machines.Items) > 0, nil
}

func metalmanBootstrapUsername() string {
	return "system:serviceaccount:" + unbounded.SystemNamespace() + ":" + metalmanBootstrapServiceAccount
}
