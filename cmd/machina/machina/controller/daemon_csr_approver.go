// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

const (
	bootstrapSecretPref        = "bootstrap-token-"
	defaultBootstrapTokenLabel = "unbounded-cloud.io/default-bootstrap-token"
	daemonGroup                = "unbounded-agent-daemons"
	bootstrapGroup             = "system:bootstrappers:unbounded-agent-daemons"

	machineNodeRefNameField = "spec.kubernetes.nodeRef.name"
)

type daemonCSRClaimChecker struct {
	client.Client
}

func NewDaemonCSRApprover(
	c client.Client,
	kubeClient kubernetes.Interface,
) (*daemoncred.CSRApproverReconciler, error) {
	claims := &daemonCSRClaimChecker{Client: c}

	approver, err := daemoncred.NewCSRApprover(daemoncred.CSRApproverOptions{
		BootstrapGroup:     bootstrapGroup,
		DaemonGroup:        daemonGroup,
		AuthorizeBootstrap: claims.bootstrapTokenMayClaimNode,
		AuthorizeRenewal:   claims.nodeHasMachineBinding,
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
