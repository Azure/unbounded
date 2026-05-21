// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	netv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

var machineSiteLabelKeys = []string{unboundedv1alpha3.MachineSiteLabelKey, netv1alpha1.SiteLabelKey}

const (
	authReasonAmbiguous       = "AuthAmbiguous"
	authReasonInvalid         = "AuthInvalid"
	authReasonNotFound        = "AuthNotFound"
	authReasonSecretForbidden = "AuthSecretForbidden"
	authReasonSecretNotFound  = "AuthSecretNotFound"
)

type authResolutionFailure struct {
	Reason  string
	Message string
}

type operationAuthTarget struct {
	SiteName string
	Provider string
}

func (r *MachineOperationReconciler) resolveOperationAuth(ctx context.Context, machine *unboundedv1alpha3.Machine) (*OperationAuth, *authResolutionFailure, error) {
	target, failure := operationAuthTargetFor(machine)
	if failure != nil {
		return nil, failure, nil
	}

	credential, failure, err := r.machineOperationCredentialFor(ctx, target)
	if failure != nil || err != nil {
		return nil, failure, err
	}
	if credential == nil {
		return nil, &authResolutionFailure{
			Reason:  authReasonNotFound,
			Message: fmt.Sprintf("no MachineOperationCredential matches site %q and provider %q", target.SiteName, target.Provider),
		}, nil
	}

	return r.authFromCredential(ctx, credential)
}

func operationAuthTargetFor(machine *unboundedv1alpha3.Machine) (operationAuthTarget, *authResolutionFailure) {
	if canonical, legacy, conflict := conflictingSiteLabels(machine.Labels); conflict {
		return operationAuthTarget{}, &authResolutionFailure{
			Reason:  authReasonInvalid,
			Message: fmt.Sprintf("Machine %s has conflicting site labels %q=%q and %q=%q", machine.Name, unboundedv1alpha3.MachineSiteLabelKey, canonical, netv1alpha1.SiteLabelKey, legacy),
		}
	}

	target := operationAuthTarget{
		SiteName: siteNameFromLabels(machine.Labels),
		Provider: strings.TrimSpace(machine.Spec.Provider),
	}
	if target.SiteName == "" {
		return operationAuthTarget{}, &authResolutionFailure{
			Reason:  authReasonInvalid,
			Message: fmt.Sprintf("Machine %s is missing site label %q", machine.Name, unboundedv1alpha3.MachineSiteLabelKey),
		}
	}
	if target.Provider == "" {
		return operationAuthTarget{}, &authResolutionFailure{
			Reason:  authReasonInvalid,
			Message: fmt.Sprintf("Machine %s is missing spec.provider", machine.Name),
		}
	}

	return target, nil
}

func (r *MachineOperationReconciler) machineOperationCredentialFor(
	ctx context.Context,
	target operationAuthTarget,
) (*unboundedv1alpha3.MachineOperationCredential, *authResolutionFailure, error) {
	var credentials unboundedv1alpha3.MachineOperationCredentialList
	if err := r.List(ctx, &credentials); err != nil {
		return nil, nil, fmt.Errorf("list MachineOperationCredentials: %w", err)
	}

	var match *unboundedv1alpha3.MachineOperationCredential
	for i := range credentials.Items {
		credential := &credentials.Items[i]
		if credential.Spec.SiteName != target.SiteName || credential.Spec.Provider != target.Provider {
			continue
		}

		if match != nil {
			return nil, &authResolutionFailure{
				Reason:  authReasonAmbiguous,
				Message: fmt.Sprintf("multiple MachineOperationCredentials match site %q and provider %q", target.SiteName, target.Provider),
			}, nil
		}

		match = credential
	}

	return match, nil, nil
}

func siteNameFromLabels(labels map[string]string) string {
	for _, key := range machineSiteLabelKeys {
		if value := strings.TrimSpace(labels[key]); value != "" {
			return value
		}
	}

	return ""
}

func conflictingSiteLabels(labels map[string]string) (string, string, bool) {
	canonical := strings.TrimSpace(labels[unboundedv1alpha3.MachineSiteLabelKey])
	legacy := strings.TrimSpace(labels[netv1alpha1.SiteLabelKey])

	return canonical, legacy, canonical != "" && legacy != "" && canonical != legacy
}

func (r *MachineOperationReconciler) authFromCredential(
	ctx context.Context,
	credential *unboundedv1alpha3.MachineOperationCredential,
) (*OperationAuth, *authResolutionFailure, error) {
	auth := &OperationAuth{Mode: credential.Spec.Auth.Mode}
	if auth.Mode == "" {
		return nil, &authResolutionFailure{
			Reason:  authReasonInvalid,
			Message: fmt.Sprintf("MachineOperationCredential %s has an empty spec.auth.mode", credential.Name),
		}, nil
	}

	if credential.Spec.Auth.Mode == unboundedv1alpha3.MachineOperationCredentialAuthExternalPlugin && credential.Spec.Auth.SecretRef == nil {
		return nil, &authResolutionFailure{
			Reason:  authReasonInvalid,
			Message: fmt.Sprintf("MachineOperationCredential %s uses ExternalPlugin mode without spec.auth.secretRef", credential.Name),
		}, nil
	}

	if credential.Spec.Auth.SecretRef == nil {
		return auth, nil, nil
	}

	secret, failure, err := r.credentialSecret(ctx, credential)
	if failure != nil || err != nil {
		return nil, failure, err
	}

	auth.SecretData = make(map[string]string, len(secret.Data))
	for key, value := range secret.Data {
		auth.SecretData[key] = string(value)
	}

	return auth, nil, nil
}

func (r *MachineOperationReconciler) credentialSecret(
	ctx context.Context,
	credential *unboundedv1alpha3.MachineOperationCredential,
) (*corev1.Secret, *authResolutionFailure, error) {
	ref := credential.Spec.Auth.SecretRef
	if strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.Namespace) == "" {
		return nil, &authResolutionFailure{
			Reason:  authReasonInvalid,
			Message: fmt.Sprintf("MachineOperationCredential %s has an incomplete spec.auth.secretRef", credential.Name),
		}, nil
	}

	if r.CredentialSecretNamespace != "" && ref.Namespace != r.CredentialSecretNamespace {
		return nil, &authResolutionFailure{
			Reason:  authReasonSecretForbidden,
			Message: fmt.Sprintf("MachineOperationCredential %s references Secret %s/%s outside the allowed namespace %s", credential.Name, ref.Namespace, ref.Name, r.CredentialSecretNamespace),
		}, nil
	}

	var secret corev1.Secret
	secretKey := client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}
	if err := r.Get(ctx, secretKey, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &authResolutionFailure{
				Reason:  authReasonSecretNotFound,
				Message: fmt.Sprintf("Secret %s/%s referenced by MachineOperationCredential %s was not found", ref.Namespace, ref.Name, credential.Name),
			}, nil
		}

		return nil, nil, fmt.Errorf("get MachineOperationCredential secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	return &secret, nil, nil
}
