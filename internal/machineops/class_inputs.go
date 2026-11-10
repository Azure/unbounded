// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machineconfigs"
)

// classInputFailure is a MachineClass input that did not resolve. It makes
// the class not Ready.
type classInputFailure struct {
	Reason  string
	Message string
}

// classInputs are the resolved inputs of a MachineClass.
type classInputs struct {
	providerConfigData map[string]string
	auth               *OperationAuth
	revision           string
	providerConfig     *unboundedv1alpha3.ObjectContentSnapshot
	bootstrapFiles     *unboundedv1alpha3.ObjectContentSnapshot
	configuration      *unboundedv1alpha3.ConfigurationContentSnapshot
	site               *unboundedv1alpha3.UIDReference
	credential         *unboundedv1alpha3.UIDReference
}

// classRevision is everything resolvedRevision covers, in a fixed order. It
// excludes offerings, timestamps, conditions, the class generation, name and
// UID, and Secret content.
type classRevision struct {
	Provider       string                                          `json:"provider"`
	Site           *unboundedv1alpha3.UIDReference                 `json:"site"`
	Credential     *unboundedv1alpha3.UIDReference                 `json:"credential"`
	ProviderConfig *unboundedv1alpha3.ObjectContentSnapshot        `json:"providerConfig"`
	BootstrapFiles *unboundedv1alpha3.ObjectContentSnapshot        `json:"bootstrapFiles"`
	Configuration  *unboundedv1alpha3.ConfigurationContentSnapshot `json:"configuration"`
	AgentArtifact  *unboundedv1alpha3.AgentArtifact                `json:"agentArtifact"`
}

// classConfigMapRef names a ConfigMap a class references and the reasons its
// checks report.
type classConfigMapRef struct {
	field        string
	namespace    string
	name         string
	forbidden    string
	notFound     string
	notImmutable string
	invalid      string
}

// resolveClassInputs resolves the class inputs in order and stops at the
// first failure. A read error other than NotFound is returned as an error.
func (r *MachineClassReconciler) resolveClassInputs(
	ctx context.Context,
	class *unboundedv1alpha3.MachineClass,
) (*classInputs, *classInputFailure, error) {
	inputs := &classInputs{}

	site, failure, err := r.resolveClassSite(ctx, class)
	if failure != nil || err != nil {
		return nil, failure, err
	}

	inputs.site = &unboundedv1alpha3.UIDReference{Name: site.Name, UID: site.UID}

	providerConfig, failure, err := r.resolveClassProviderConfig(ctx, class)
	if failure != nil || err != nil {
		return nil, failure, err
	}

	inputs.providerConfigData = maps.Clone(providerConfig.Data)

	inputs.providerConfig, err = configMapSnapshot(providerConfig)
	if err != nil {
		return nil, nil, err
	}

	if class.Spec.Bootstrap != nil && class.Spec.Bootstrap.FilesRef != nil {
		bootstrapFiles, bootstrapFailure, err := r.resolveClassBootstrapFiles(ctx, class)
		if bootstrapFailure != nil || err != nil {
			return nil, bootstrapFailure, err
		}

		inputs.bootstrapFiles, err = configMapSnapshot(bootstrapFiles)
		if err != nil {
			return nil, nil, err
		}
	}

	inputs.configuration, failure, err = r.resolveClassConfiguration(ctx, class)
	if failure != nil || err != nil {
		return nil, failure, err
	}

	credential, auth, authFailure, err := resolveTargetAuth(ctx, r.Client, r.SecretReader, r.CredentialSecretNamespace, operationAuthTarget{
		SiteName: class.Spec.SiteRef.Name,
		Provider: class.Spec.Provider,
	})
	if err != nil {
		return nil, nil, err
	}

	if authFailure != nil {
		return nil, &classInputFailure{Reason: authFailure.Reason, Message: authFailure.Message}, nil
	}

	inputs.credential = &unboundedv1alpha3.UIDReference{Name: credential.Name, UID: credential.UID}
	inputs.auth = auth

	var agentArtifact *unboundedv1alpha3.AgentArtifact
	if class.Spec.Bootstrap != nil {
		agentArtifact = class.Spec.Bootstrap.AgentArtifact
	}

	inputs.revision, err = contentHash(classRevision{
		Provider:       class.Spec.Provider,
		Site:           inputs.site,
		Credential:     inputs.credential,
		ProviderConfig: inputs.providerConfig,
		BootstrapFiles: inputs.bootstrapFiles,
		Configuration:  inputs.configuration,
		AgentArtifact:  agentArtifact,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("hash MachineClass %s revision: %w", class.Name, err)
	}

	return inputs, nil, nil
}

func (r *MachineClassReconciler) resolveClassSite(
	ctx context.Context,
	class *unboundedv1alpha3.MachineClass,
) (*unboundedv1alpha3.Site, *classInputFailure, error) {
	name := class.Spec.SiteRef.Name

	var site unboundedv1alpha3.Site
	if err := r.Get(ctx, client.ObjectKey{Name: name}, &site); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &classInputFailure{
				Reason:  classReasonSiteNotFound,
				Message: fmt.Sprintf("Site %s referenced by MachineClass %s was not found", name, class.Name),
			}, nil
		}

		return nil, nil, fmt.Errorf("get Site %s: %w", name, err)
	}

	if !site.DeletionTimestamp.IsZero() {
		return nil, &classInputFailure{
			Reason:  classReasonSiteDeleting,
			Message: fmt.Sprintf("Site %s referenced by MachineClass %s is being deleted", name, class.Name),
		}, nil
	}

	return &site, nil, nil
}

func (r *MachineClassReconciler) resolveClassProviderConfig(
	ctx context.Context,
	class *unboundedv1alpha3.MachineClass,
) (*corev1.ConfigMap, *classInputFailure, error) {
	ref := class.Spec.ProviderConfigRef
	if ref.APIGroup != "" || ref.Kind != "ConfigMap" || ref.Namespace == "" {
		return nil, &classInputFailure{
			Reason: classReasonProviderConfigInvalid,
			Message: fmt.Sprintf("MachineClass %s spec.providerConfigRef must name a core-group ConfigMap with a namespace, not apiGroup %q kind %q namespace %q",
				class.Name, ref.APIGroup, ref.Kind, ref.Namespace),
		}, nil
	}

	return r.resolveClassConfigMap(ctx, class, classConfigMapRef{
		field:        "spec.providerConfigRef",
		namespace:    ref.Namespace,
		name:         ref.Name,
		forbidden:    classReasonProviderConfigForbidden,
		notFound:     classReasonProviderConfigNotFound,
		notImmutable: classReasonProviderConfigNotImmutable,
		invalid:      classReasonProviderConfigInvalid,
	})
}

func (r *MachineClassReconciler) resolveClassBootstrapFiles(
	ctx context.Context,
	class *unboundedv1alpha3.MachineClass,
) (*corev1.ConfigMap, *classInputFailure, error) {
	ref := class.Spec.Bootstrap.FilesRef

	configMap, failure, err := r.resolveClassConfigMap(ctx, class, classConfigMapRef{
		field:        "spec.bootstrap.filesRef",
		namespace:    ref.Namespace,
		name:         ref.Name,
		forbidden:    classReasonBootstrapFilesForbidden,
		notFound:     classReasonBootstrapFilesNotFound,
		notImmutable: classReasonBootstrapFilesNotImmutable,
		invalid:      classReasonBootstrapFilesInvalid,
	})
	if failure != nil || err != nil {
		return nil, failure, err
	}

	if strings.TrimSpace(configMap.Data[unboundedv1alpha3.MachineClassBootstrapRunKey]) == "" {
		return nil, &classInputFailure{
			Reason: classReasonBootstrapFilesInvalid,
			Message: fmt.Sprintf("ConfigMap %s/%s referenced by MachineClass %s spec.bootstrap.filesRef must have a non-blank %q key",
				ref.Namespace, ref.Name, class.Name, unboundedv1alpha3.MachineClassBootstrapRunKey),
		}, nil
	}

	return configMap, nil, nil
}

// resolveClassConfigMap reads a referenced ConfigMap through ConfigMapReader
// after checking its namespace, and requires it to be immutable and to have
// no binaryData.
func (r *MachineClassReconciler) resolveClassConfigMap(
	ctx context.Context,
	class *unboundedv1alpha3.MachineClass,
	ref classConfigMapRef,
) (*corev1.ConfigMap, *classInputFailure, error) {
	if ref.namespace != r.ProviderConfigNamespace {
		return nil, &classInputFailure{
			Reason: ref.forbidden,
			Message: fmt.Sprintf("MachineClass %s %s names ConfigMap %s/%s outside the allowed namespace %s",
				class.Name, ref.field, ref.namespace, ref.name, r.ProviderConfigNamespace),
		}, nil
	}

	var configMap corev1.ConfigMap
	if err := r.ConfigMapReader.Get(ctx, client.ObjectKey{Namespace: ref.namespace, Name: ref.name}, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &classInputFailure{
				Reason:  ref.notFound,
				Message: fmt.Sprintf("ConfigMap %s/%s referenced by MachineClass %s %s was not found", ref.namespace, ref.name, class.Name, ref.field),
			}, nil
		}

		return nil, nil, fmt.Errorf("get ConfigMap %s/%s: %w", ref.namespace, ref.name, err)
	}

	if configMap.Immutable == nil || !*configMap.Immutable {
		return nil, &classInputFailure{
			Reason:  ref.notImmutable,
			Message: fmt.Sprintf("ConfigMap %s/%s referenced by MachineClass %s %s must set immutable to true", ref.namespace, ref.name, class.Name, ref.field),
		}, nil
	}

	if len(configMap.BinaryData) > 0 {
		return nil, &classInputFailure{
			Reason:  ref.invalid,
			Message: fmt.Sprintf("ConfigMap %s/%s referenced by MachineClass %s %s must not have binaryData", ref.namespace, ref.name, class.Name, ref.field),
		}, nil
	}

	return &configMap, nil, nil
}

func (r *MachineClassReconciler) resolveClassConfiguration(
	ctx context.Context,
	class *unboundedv1alpha3.MachineClass,
) (*unboundedv1alpha3.ConfigurationContentSnapshot, *classInputFailure, error) {
	ref := class.Spec.ConfigurationRef
	if ref.Version == nil {
		return nil, &classInputFailure{
			Reason:  classReasonConfigurationInvalid,
			Message: fmt.Sprintf("MachineClass %s spec.configurationRef.version is required", class.Name),
		}, nil
	}

	versionName := unboundedv1alpha3.MachineConfigurationVersionName(ref.Name, *ref.Version)

	version, err := machineconfigs.ResolveVersionFromRef(ctx, r.Client, &ref)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &classInputFailure{
				Reason:  classReasonConfigurationNotFound,
				Message: fmt.Sprintf("MachineConfigurationVersion %s referenced by MachineClass %s was not found", versionName, class.Name),
			}, nil
		}

		return nil, nil, err
	}

	if version.Spec.Version != *ref.Version || version.Labels[unboundedv1alpha3.MCVConfigurationLabelKey] != ref.Name {
		return nil, &classInputFailure{
			Reason: classReasonConfigurationMismatch,
			Message: fmt.Sprintf("MachineConfigurationVersion %s has spec.version %d and label %s=%q, but MachineClass %s references configuration %s version %d",
				version.Name, version.Spec.Version, unboundedv1alpha3.MCVConfigurationLabelKey, version.Labels[unboundedv1alpha3.MCVConfigurationLabelKey],
				class.Name, ref.Name, *ref.Version),
		}, nil
	}

	hash, err := contentHash(version.Spec.Template)
	if err != nil {
		return nil, nil, fmt.Errorf("hash MachineConfigurationVersion %s template: %w", version.Name, err)
	}

	return &unboundedv1alpha3.ConfigurationContentSnapshot{
		Name:        ref.Name,
		Version:     *ref.Version,
		VersionName: version.Name,
		UID:         version.UID,
		Hash:        hash,
	}, nil, nil
}

func configMapSnapshot(configMap *corev1.ConfigMap) (*unboundedv1alpha3.ObjectContentSnapshot, error) {
	hash, err := configMapDataHash(configMap.Data)
	if err != nil {
		return nil, fmt.Errorf("hash ConfigMap %s/%s: %w", configMap.Namespace, configMap.Name, err)
	}

	return &unboundedv1alpha3.ObjectContentSnapshot{
		Namespace: configMap.Namespace,
		Name:      configMap.Name,
		UID:       configMap.UID,
		Hash:      hash,
	}, nil
}

// configMapDataHash hashes ConfigMap data. A nil map hashes like an empty one.
func configMapDataHash(data map[string]string) (string, error) {
	if data == nil {
		data = map[string]string{}
	}

	return contentHash(data)
}

// contentHash is the lowercase hex SHA-256 of the JSON encoding of value.
func contentHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(encoded)

	return hex.EncodeToString(sum[:]), nil
}
