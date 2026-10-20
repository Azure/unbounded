// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package attestation

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	certificatesv1 "k8s.io/api/certificates/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/indexing"
	"github.com/Azure/unbounded/internal/unbounded"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

// BootstrapUsername returns the CSR requester username of the bootstrap
// tokens that Attest issues.
func BootstrapUsername() string {
	return "system:serviceaccount:" + unbounded.SystemNamespace() + ":" + BootstrapSAName
}

// CSRApprover approves kubelet client CSRs submitted with a bootstrap token
// issued by Attest, but only for the node name recorded with that token's
// credential on the single Machine it was issued to.
//
// Each metalman instance sees only the Machines of its own Site, so a
// credential it has no record of is left undecided for the instance that
// issued it rather than denied. A CSR evaluated before the cache observed its
// Machine is evaluated again when the Machine's credential is recorded.
type CSRApprover struct {
	// Client reads CSRs and Machines from the cache. Machines must be indexed
	// by indexing.IndexMachineByBootstrapCredentialID.
	Client client.Client

	// APIReader reads directly from the API server. The cache index finds the
	// candidate Machine, but the binding is confirmed against the live object
	// so a stale cache cannot authorize a superseded credential.
	APIReader client.Reader

	// KubeClient updates the CSR approval subresource.
	KubeClient kubernetes.Interface
}

func (a *CSRApprover) reconciler() (*daemoncred.CSRApproverReconciler, error) {
	if a.APIReader == nil {
		return nil, fmt.Errorf("metalman CSR approver requires an API reader")
	}

	approver, err := daemoncred.NewKubeletClientCSRApprover(daemoncred.KubeletClientCSRApproverOptions{
		Requester:     BootstrapUsername(),
		AuthorizeNode: a.credentialMayClaimNode,
	})
	if err != nil {
		return nil, err
	}

	return daemoncred.NewCSRApproverReconciler(a.Client, a.KubeClient, approver)
}

// SetupWithManager registers the approver with mgr.
func (a *CSRApprover) SetupWithManager(mgr ctrl.Manager) error {
	r, err := a.reconciler()
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("metalman-csr-approver").
		For(&certificatesv1.CertificateSigningRequest{}, builder.WithPredicates(bootstrapCSRPredicate())).
		Watches(&v1alpha3.Machine{}, handler.EnqueueRequestsFromMapFunc(a.csrsForMachine), builder.WithPredicates(credentialRecordedPredicate())).
		Complete(r)
}

func isBootstrapCSR(obj client.Object) bool {
	csr, ok := obj.(*certificatesv1.CertificateSigningRequest)

	return ok && csr.Spec.SignerName == daemoncred.KubeletClientSignerName && csr.Spec.Username == BootstrapUsername()
}

func bootstrapCSRPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return isBootstrapCSR(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return isBootstrapCSR(e.ObjectNew) },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return isBootstrapCSR(e.Object) },
	}
}

func bootstrapCredentialID(obj client.Object) string {
	machine, ok := obj.(*v1alpha3.Machine)
	if !ok || machine.Status.Bootstrap == nil {
		return ""
	}

	return machine.Status.Bootstrap.CredentialID
}

// credentialRecordedPredicate passes Machine events that make a bootstrap
// credential visible in the cache.
func credentialRecordedPredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return bootstrapCredentialID(e.Object) != "" },
		UpdateFunc: func(e event.UpdateEvent) bool {
			id := bootstrapCredentialID(e.ObjectNew)
			return id != "" && id != bootstrapCredentialID(e.ObjectOld)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// csrsForMachine enqueues the undecided bootstrap CSRs that carry the
// Machine's recorded credential.
func (a *CSRApprover) csrsForMachine(ctx context.Context, obj client.Object) []reconcile.Request {
	credentialID := bootstrapCredentialID(obj)
	if credentialID == "" {
		return nil
	}

	var csrs certificatesv1.CertificateSigningRequestList
	if err := a.Client.List(ctx, &csrs); err != nil {
		slog.ErrorContext(ctx, "listing CSRs for bootstrap credential", "machine", obj.GetName(), "err", err)
		return nil
	}

	var requests []reconcile.Request

	for i := range csrs.Items {
		csr := &csrs.Items[i]
		if !isBootstrapCSR(csr) || daemoncred.HasApprovalDecision(csr) {
			continue
		}

		if slices.Contains(csr.Spec.Extra[daemoncred.CredentialIDExtraKey], credentialID) {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: csr.Name}})
		}
	}

	return requests
}

func (a *CSRApprover) credentialMayClaimNode(
	ctx context.Context,
	_ *certificatesv1.CertificateSigningRequest,
	nodeName, credentialID string,
) (bool, string, error) {
	var machines v1alpha3.MachineList
	if err := a.Client.List(ctx, &machines, client.MatchingFields{indexing.IndexMachineByBootstrapCredentialID: credentialID}); err != nil {
		return false, "", fmt.Errorf("list Machines by bootstrap credential ID: %w", err)
	}

	switch len(machines.Items) {
	case 0:
		return false, "", fmt.Errorf("no Machine in this Site records credential %q: %w", credentialID, daemoncred.ErrUnknownCredential)
	case 1:
	default:
		return false, fmt.Sprintf("credential is recorded on %d Machines", len(machines.Items)), nil
	}

	// The cached Machine only locates the candidate. Its status may lag, so
	// the live Machine decides the match.
	return a.confirmLiveBinding(ctx, &machines.Items[0], nodeName, credentialID)
}

// confirmLiveBinding re-reads the cached candidate Machine from the API server
// and approves only if it is the same object and still binds the credential to
// the node. Re-attestation replaces the credential ID, so any change means the
// CSR's token was superseded. Transient read errors are returned for retry.
func (a *CSRApprover) confirmLiveBinding(
	ctx context.Context,
	cached *v1alpha3.Machine,
	nodeName, credentialID string,
) (bool, string, error) {
	var live v1alpha3.Machine
	if err := a.APIReader.Get(ctx, client.ObjectKeyFromObject(cached), &live); err != nil {
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

	// Match the node name recorded with the credential, not the live
	// spec.kubernetes.nodeRef, so the approved name is the one the host was
	// configured with. An empty record fails closed.
	if live.Status.Bootstrap.NodeName == "" || live.Status.Bootstrap.NodeName != nodeName {
		return false, fmt.Sprintf("credential was issued to Machine %q", live.Name), nil
	}

	return true, "", nil
}
