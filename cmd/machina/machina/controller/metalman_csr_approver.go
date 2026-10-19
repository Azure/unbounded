// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	// metalmanCSRCacheGrace is how long a CSR with no matching Machine is
	// retried instead of denied. Metalman records the credential ID before it
	// returns the token, but the controller cache may not have observed that
	// write yet when the CSR arrives, and a denial is terminal.
	metalmanCSRCacheGrace = 2 * time.Minute
)

// metalmanBootstrapUsername returns the CSR requester username of tokens
// minted by metalman for attested machines.
func metalmanBootstrapUsername() string {
	return "system:serviceaccount:" + unbounded.SystemNamespace() + ":" + metalmanBootstrapServiceAccount
}

type metalmanCSRClaimChecker struct {
	client.Client

	// apiReader reads directly from the API server. The cache index finds the
	// candidate Machine, but the binding is confirmed against the live object
	// so a stale cache cannot authorize a superseded credential.
	apiReader client.Reader

	// clock returns the current time. Defaults to time.Now.
	clock func() time.Time
}

func (c *metalmanCSRClaimChecker) now() time.Time {
	if c.clock != nil {
		return c.clock()
	}

	return time.Now()
}

// NewMetalmanCSRApprover returns a reconciler that approves kubelet client
// CSRs submitted with a metalman bootstrap token, but only for the node name
// of the single Machine the token was issued to.
func NewMetalmanCSRApprover(
	c client.Client,
	apiReader client.Reader,
	kubeClient kubernetes.Interface,
) (*daemoncred.CSRApproverReconciler, error) {
	if apiReader == nil {
		return nil, fmt.Errorf("metalman CSR approver requires an API reader")
	}

	claims := &metalmanCSRClaimChecker{Client: c, apiReader: apiReader}

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
	csr *certificatesv1.CertificateSigningRequest,
	nodeName, credentialID string,
) (bool, string, error) {
	var machines unboundedv1alpha3.MachineList
	if err := c.List(ctx, &machines, client.MatchingFields{machineBootstrapCredentialIDField: credentialID}); err != nil {
		return false, "", fmt.Errorf("list Machines by bootstrap credential ID: %w", err)
	}

	switch len(machines.Items) {
	case 0:
		if c.now().Sub(csr.CreationTimestamp.Time) < metalmanCSRCacheGrace {
			return false, "", fmt.Errorf("no Machine records credential %q yet; retrying while the cache catches up", credentialID)
		}

		return false, "no Machine was issued this credential", nil
	case 1:
	default:
		return false, fmt.Sprintf("credential is recorded on %d Machines", len(machines.Items)), nil
	}

	// The cached Machine only locates the candidate. Its node name may lag a
	// spec.kubernetes.nodeRef change, so the live Machine decides the match.
	return c.confirmLiveBinding(ctx, &machines.Items[0], nodeName, credentialID)
}

// confirmLiveBinding re-reads the cached candidate Machine from the API server
// and approves only if it is the same object and still binds the credential to
// the node. Re-attestation replaces the credential ID, so any change means the
// CSR's token was superseded. Transient read errors are returned for retry.
func (c *metalmanCSRClaimChecker) confirmLiveBinding(
	ctx context.Context,
	cached *unboundedv1alpha3.Machine,
	nodeName, credentialID string,
) (bool, string, error) {
	var live unboundedv1alpha3.Machine
	if err := c.apiReader.Get(ctx, client.ObjectKeyFromObject(cached), &live); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Sprintf("Machine %q no longer exists", cached.Name), nil
		}

		return false, "", fmt.Errorf("get live Machine %q: %w", cached.Name, err)
	}

	if live.UID != cached.UID {
		return false, fmt.Sprintf("Machine %q was recreated", cached.Name), nil
	}

	if live.Status.Bootstrap == nil || live.Status.Bootstrap.CredentialID != credentialID {
		return false, fmt.Sprintf("credential was superseded on Machine %q", cached.Name), nil
	}

	if live.NodeName() != nodeName {
		return false, fmt.Sprintf("credential was issued to Machine %q", live.Name), nil
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
