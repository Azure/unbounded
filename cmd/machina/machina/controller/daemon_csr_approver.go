// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	bootstrapSecretPref        = "bootstrap-token-"
	defaultBootstrapTokenLabel = "unbounded-cloud.io/default-bootstrap-token"
	daemonGroup                = "unbounded-agent-daemons"
	bootstrapGroup             = "system:bootstrappers:unbounded-agent-daemons"
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

	siteName := strings.TrimSpace(token.Labels[unboundedv1alpha3.MachineSiteLabelKey])
	isDefaultToken := strings.EqualFold(strings.TrimSpace(token.Labels[defaultBootstrapTokenLabel]), "true")
	if siteName == "" && !isDefaultToken {
		return false, nil
	}

	var machines unboundedv1alpha3.MachineList
	if err := c.List(ctx, &machines); err != nil {
		return false, fmt.Errorf("list Machines for bootstrap token claim check: %w", err)
	}

	for _, machine := range machines.Items {
		if machine.Spec.Kubernetes == nil {
			continue
		}
		if machine.Spec.Kubernetes.BootstrapTokenRef.Name != secretName {
			continue
		}

		// During early bootstrap, the Machine may not exist yet. When a Machine
		// already references this token, enforce token-to-Machine-to-Node binding.
		if siteName != "" && machine.Labels[unboundedv1alpha3.MachineSiteLabelKey] != siteName {
			return false, nil
		}

		return machineNodeName(&machine) == nodeName, nil
	}

	// No Machine references this token yet. Allow initial issuance based on a
	// trusted bootstrap token label; the daemon may create the Machine afterward.
	return true, nil
}

func (c *daemonCSRClaimChecker) nodeHasMachineBinding(
	ctx context.Context,
	_ *certificatesv1.CertificateSigningRequest,
	nodeName string,
) (bool, error) {
	var machine unboundedv1alpha3.Machine
	if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &machine); err == nil {
		return machineNodeName(&machine) == nodeName, nil
	} else if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("get Machine %s for renewal claim check: %w", nodeName, err)
	}

	var machines unboundedv1alpha3.MachineList
	if err := c.List(ctx, &machines); err != nil {
		return false, fmt.Errorf("list Machines for renewal claim check: %w", err)
	}
	for _, machine := range machines.Items {
		if machineNodeName(&machine) == nodeName {
			return true, nil
		}
	}

	return false, nil
}

func machineNodeName(machine *unboundedv1alpha3.Machine) string {
	if machine.Spec.Kubernetes != nil && machine.Spec.Kubernetes.NodeRef != nil && machine.Spec.Kubernetes.NodeRef.Name != "" {
		return machine.Spec.Kubernetes.NodeRef.Name
	}

	return machine.Name
}
