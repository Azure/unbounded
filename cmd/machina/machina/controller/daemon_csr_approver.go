// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machinestatus"
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

	reconciler, err := daemoncred.NewCSRApproverReconciler(c, kubeClient, approver)
	if err != nil {
		return nil, err
	}

	reconciler.OnDecision = claims.recordDecision

	return reconciler, nil
}

func (c *daemonCSRClaimChecker) recordDecision(
	ctx context.Context,
	_ *certificatesv1.CertificateSigningRequest,
	decision daemoncred.CSRDecision,
) error {
	if decision.NodeName == "" {
		return nil
	}

	machineName, ok, err := c.machineNameForNode(ctx, decision.NodeName)
	if err != nil || !ok {
		return err
	}

	status := metav1.ConditionTrue
	reason := "Approved"

	if !decision.Approve {
		status = metav1.ConditionFalse
		reason = "Denied"
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var machine unboundedv1alpha3.Machine
		if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
			return client.IgnoreNotFound(err)
		}

		meta.SetStatusCondition(&machine.Status.Conditions, machinestatus.Condition(
			unboundedv1alpha3.MachineConditionDaemonCredentialReady,
			status,
			reason,
			decision.Message,
			machine.Generation,
		))

		return c.Status().Update(ctx, &machine)
	})
}

func (c *daemonCSRClaimChecker) machineNameForNode(ctx context.Context, nodeName string) (string, bool, error) {
	var machines unboundedv1alpha3.MachineList
	if err := c.List(ctx, &machines, client.MatchingFields{machineNodeRefNameField: nodeName}); err != nil {
		return "", false, fmt.Errorf("list Machines by node ref for CSR decision: %w", err)
	}

	if len(machines.Items) > 0 {
		return machines.Items[0].Name, true, nil
	}

	var machine unboundedv1alpha3.Machine
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &machine); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "", false, nil
		}

		return "", false, err
	}

	return machine.Name, true, nil
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
