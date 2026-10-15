// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/kube"
	metalmanbootstrap "github.com/Azure/unbounded/internal/metalman/bootstrap"
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

type multiCSRApprover []daemoncred.CSRApproverEvaluator

func (a multiCSRApprover) Evaluate(
	ctx context.Context,
	csr *certificatesv1.CertificateSigningRequest,
) (daemoncred.CSRDecision, error) {
	for _, approver := range a {
		decision, err := approver.Evaluate(ctx, csr)
		if err != nil || !decision.Ignore {
			return decision, err
		}
	}

	return daemoncred.CSRDecision{Ignore: true}, nil
}

type bootstrapGroupApprover struct {
	group    string
	approver daemoncred.CSRApproverEvaluator
}

func (a bootstrapGroupApprover) Evaluate(
	ctx context.Context,
	csr *certificatesv1.CertificateSigningRequest,
) (daemoncred.CSRDecision, error) {
	if strings.HasPrefix(csr.Spec.Username, daemoncred.BootstrapUserPrefix) &&
		!slices.Contains(csr.Spec.Groups, a.group) {
		return daemoncred.CSRDecision{Ignore: true}, nil
	}

	return a.approver.Evaluate(ctx, csr)
}

func NewDaemonCSRApprover(
	c client.Client,
	kubeClient kubernetes.Interface,
) (*daemoncred.CSRApproverReconciler, error) {
	claims := &daemonCSRClaimChecker{Client: c}

	approver, err := daemoncred.NewCSRApprover(daemoncred.CSRApproverOptions{
		BootstrapGroup:     bootstrapGroup,
		DaemonGroup:        daemonGroup,
		AuthorizeBootstrap: claims.legacyBootstrapTokenMayClaimNode,
		AuthorizeRenewal:   claims.nodeHasMachineBinding,
	})
	if err != nil {
		return nil, err
	}

	metalmanDaemonApprover, err := daemoncred.NewCSRApprover(daemoncred.CSRApproverOptions{
		BootstrapGroup:     metalmanbootstrap.Group,
		DaemonGroup:        daemonGroup,
		AuthorizeBootstrap: claims.bootstrapTokenMayClaimNode,
		AuthorizeRenewal:   claims.nodeHasMachineBinding,
	})
	if err != nil {
		return nil, err
	}

	kubeletApprover, err := daemoncred.NewKubeletCSRApprover(daemoncred.KubeletCSRApproverOptions{
		BootstrapGroup:     metalmanbootstrap.Group,
		AuthorizeBootstrap: claims.bootstrapTokenMayClaimNode,
	})
	if err != nil {
		return nil, err
	}

	return daemoncred.NewCSRApproverReconciler(
		c,
		kubeClient,
		multiCSRApprover{
			bootstrapGroupApprover{group: bootstrapGroup, approver: approver},
			bootstrapGroupApprover{group: metalmanbootstrap.Group, approver: metalmanDaemonApprover},
			kubeletApprover,
		},
	)
}

func (c *daemonCSRClaimChecker) legacyBootstrapTokenMayClaimNode(
	ctx context.Context,
	csr *certificatesv1.CertificateSigningRequest,
	_ string,
) (bool, error) {
	tokenID := strings.TrimPrefix(csr.Spec.Username, daemoncred.BootstrapUserPrefix)
	if tokenID == "" {
		return false, nil
	}

	var token corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{
		Namespace: metav1.NamespaceSystem,
		Name:      bootstrapSecretPref + tokenID,
	}, &token); err != nil {
		return false, client.IgnoreNotFound(err)
	}

	if token.Type != corev1.SecretTypeBootstrapToken {
		return false, nil
	}

	siteName := strings.TrimSpace(token.Labels[unboundedv1alpha3.MachineSiteLabelKey])
	isDefaultToken := strings.EqualFold(strings.TrimSpace(token.Labels[defaultBootstrapTokenLabel]), "true")

	return siteName != "" || isDefaultToken, nil
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

	if expiresAt := kube.BootstrapTokenExpiration(&token); !expiresAt.IsZero() && !time.Now().Before(expiresAt) {
		return false, nil
	}

	if machineName := strings.TrimSpace(token.Annotations[unboundedv1alpha3.MachineBootstrapTokenAnnotationKey]); machineName != "" {
		var machine unboundedv1alpha3.Machine
		if err := c.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
			return false, client.IgnoreNotFound(err)
		}

		return machine.NodeName() == nodeName, nil
	}

	var machines unboundedv1alpha3.MachineList
	if err := c.List(ctx, &machines); err != nil {
		return false, fmt.Errorf("list Machines for bootstrap claim check: %w", err)
	}

	for i := range machines.Items {
		machine := &machines.Items[i]
		if machine.Spec.Kubernetes == nil || machine.Spec.Kubernetes.BootstrapTokenRef == nil {
			continue
		}

		if machine.Spec.Kubernetes.BootstrapTokenRef.Name != secretName {
			continue
		}

		if machine.NodeName() == nodeName {
			return true, nil
		}
	}

	return false, nil
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
