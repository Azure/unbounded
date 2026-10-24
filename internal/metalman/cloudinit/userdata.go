// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package cloudinit resolves and validates MetalMan cloud-init configuration.
package cloudinit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const hillsboroImageRepository = "ghcr.io/azure/host-ubuntu2404-gb200-hillsboro"

type permanentError struct {
	err error
}

func (e permanentError) Error() string {
	return e.err.Error()
}

func (e permanentError) Unwrap() error {
	return e.err
}

// IsPermanentError reports whether retrying the same user-data cannot succeed
// without changing the referenced ConfigMap.
func IsPermanentError(err error) bool {
	var permanent permanentError

	return errors.As(err, &permanent)
}

// ValidateUserData rejects cloud-config that would replace image-baked users.
func ValidateUserData(reader io.Reader) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("read cloud-init user-data: %w", err)
	}

	firstLine, _, _ := bytes.Cut(data, []byte("\n"))
	templateHeader := strings.TrimSpace(strings.TrimPrefix(string(firstLine), "\uFEFF"))
	if strings.HasPrefix(strings.ToLower(templateHeader), "## template:") {
		return errors.New("cloud-init templating is not allowed for images with baked users")
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))

	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("cloud-init user-data must contain exactly one YAML mapping document")
		}

		return fmt.Errorf("parse cloud-init user-data: %w", err)
	}

	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return errors.New("cloud-init user-data must contain exactly one YAML mapping document")
	}

	var config map[string]any
	if err := document.Content[0].Decode(&config); err != nil {
		return fmt.Errorf("parse cloud-init user-data: %w", err)
	}

	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("cloud-init user-data must contain exactly one YAML document")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse trailing cloud-init user-data: %w", err)
	}

	if _, found := config["users"]; found {
		return errors.New("cloud-init user-data must not define top-level users; it would replace the image access policy")
	}

	return nil
}

// ResolveUserData returns configured Machine user-data and enforces any
// image-specific cloud-init contract.
func ResolveUserData(ctx context.Context, reader client.Reader, machine *v1alpha3.Machine) ([]byte, bool, error) {
	if machine == nil || machine.Spec.Netboot() == nil || machine.Spec.Netboot().CloudInit == nil ||
		machine.Spec.Netboot().CloudInit.UserDataConfigMapRef == nil {
		return nil, false, nil
	}

	ref := machine.Spec.Netboot().CloudInit.UserDataConfigMapRef

	var configMap corev1.ConfigMap
	if err := reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("getting ConfigMap %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	key := ref.Key
	if key == "" {
		key = "user-data"
	}

	var (
		data []byte
		ok   bool
	)
	if value, found := configMap.Data[key]; found {
		data, ok = []byte(value), true
	} else if value, found := configMap.BinaryData[key]; found {
		data, ok = value, true
	}

	if !ok {
		return nil, false, permanentError{
			err: fmt.Errorf("key %q not found in ConfigMap %s/%s", key, ref.Namespace, ref.Name),
		}
	}

	if PreservesImageUsers(machine.Spec.Netboot().Image) {
		if err := ValidateUserData(bytes.NewReader(data)); err != nil {
			return nil, false, permanentError{
				err: fmt.Errorf("validating ConfigMap %s/%s key %q: %w", ref.Namespace, ref.Name, key, err),
			}
		}
	}

	return data, true, nil
}

// PreservesImageUsers reports whether vendor-data and user-data must retain
// the users baked into the image.
func PreservesImageUsers(imageRef string) bool {
	repository := strings.TrimSpace(imageRef)
	if digest, _, found := strings.Cut(repository, "@"); found {
		repository = digest
	}
	if slash := strings.LastIndexByte(repository, '/'); slash >= 0 {
		if colon := strings.LastIndexByte(repository, ':'); colon > slash {
			repository = repository[:colon]
		}
	}

	return strings.EqualFold(repository, hillsboroImageRepository)
}
