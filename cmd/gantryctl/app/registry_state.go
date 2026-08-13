// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	gantryconfig "github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/noderoute"
)

type registryAuthRecord struct {
	Mode          string `json:"mode"`
	CredentialKey string `json:"credentialKey,omitempty"`
}

type registryAuthMetadata map[string]registryAuthRecord

type registryStore struct {
	agentConfig *gantryconfig.Config
	auth        registryAuthMetadata
	routes      noderoute.Config
	configData  map[string]string
	routesData  map[string]string
	secret      *corev1.Secret
}

type registrySnapshot struct {
	configData map[string]string
	routesData map[string]string
	secret     *corev1.Secret
}

func loadRegistryStore(ctx context.Context, kubeClient kubernetes.Interface, namespace string) (*registryStore, error) {
	configMap, err := kubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, agentConfigMapName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, errors.New("standalone Gantry is not installed; run gantryctl install first")
	}

	if err != nil {
		return nil, fmt.Errorf("get standalone Gantry config: %w", err)
	}

	if configMap.Labels["app.kubernetes.io/managed-by"] != fieldManager {
		return nil, fmt.Errorf("ConfigMap %s/%s is not owned by gantryctl", namespace, agentConfigMapName)
	}

	agentConfig := gantryconfig.NewDefault()
	if err := agentConfig.LoadYAML(strings.NewReader(configMap.Data["config.yaml"])); err != nil {
		return nil, fmt.Errorf("load standalone Gantry config: %w", err)
	}

	agentConfig.AllowNoUpstreamRegistries = true
	if err := agentConfig.Validate(); err != nil {
		return nil, fmt.Errorf("validate standalone Gantry config: %w", err)
	}

	auth := registryAuthMetadata{}
	if raw := configMap.Data["registry-auth.json"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &auth); err != nil {
			return nil, fmt.Errorf("decode registry authentication metadata: %w", err)
		}
	}

	routesMap, err := kubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, nodeRoutesConfigMapName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get standalone Gantry node routes: %w", err)
	}

	if routesMap.Labels["app.kubernetes.io/managed-by"] != fieldManager {
		return nil, fmt.Errorf("ConfigMap %s/%s is not owned by gantryctl", namespace, nodeRoutesConfigMapName)
	}

	var routes noderoute.Config
	if err := json.Unmarshal([]byte(routesMap.Data["registries.json"]), &routes); err != nil {
		return nil, fmt.Errorf("decode standalone Gantry node routes: %w", err)
	}

	if err := routes.Validate(); err != nil {
		return nil, fmt.Errorf("validate standalone Gantry node routes: %w", err)
	}

	secret, err := kubeClient.CoreV1().Secrets(namespace).Get(ctx, registryCredentialsName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		secret = nil
	} else if err != nil {
		return nil, fmt.Errorf("get standalone Gantry registry credentials: %w", err)
	} else if secret.Labels["app.kubernetes.io/managed-by"] != fieldManager {
		return nil, fmt.Errorf("secret %s/%s is not owned by gantryctl", namespace, registryCredentialsName)
	}

	return &registryStore{
		agentConfig: agentConfig,
		auth:        auth,
		routes:      routes,
		configData:  copyStringMap(configMap.Data),
		routesData:  copyStringMap(routesMap.Data),
		secret:      secret,
	}, nil
}

func (s *registryStore) snapshot() registrySnapshot {
	var secret *corev1.Secret
	if s.secret != nil {
		secret = s.secret.DeepCopy()
	}

	return registrySnapshot{
		configData: copyStringMap(s.configData),
		routesData: copyStringMap(s.routesData),
		secret:     secret,
	}
}

func (s *registryStore) encode() error {
	configYAML, err := yaml.Marshal(s.agentConfig)
	if err != nil {
		return fmt.Errorf("encode standalone Gantry config: %w", err)
	}

	authJSON, err := json.MarshalIndent(s.auth, "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry authentication metadata: %w", err)
	}

	routesJSON, err := json.Marshal(s.routes)
	if err != nil {
		return fmt.Errorf("encode standalone Gantry node routes: %w", err)
	}

	s.configData["config.yaml"] = string(configYAML)
	s.configData["registry-auth.json"] = string(append(authJSON, '\n'))
	s.routesData["registries.json"] = string(routesJSON)

	return nil
}

func (s *registryStore) sort() {
	sort.Slice(s.agentConfig.UpstreamRegistries, func(i, j int) bool {
		return s.agentConfig.UpstreamRegistries[i].Name < s.agentConfig.UpstreamRegistries[j].Name
	})
	sort.Slice(s.routes.Registries, func(i, j int) bool {
		return s.routes.Registries[i].Host < s.routes.Registries[j].Host
	})
}

func (s *registryStore) upsertRegistry(upstream gantryconfig.UpstreamRegistry, auth registryAuthRecord, route noderoute.Registry) {
	upstreamFound := false

	for index := range s.agentConfig.UpstreamRegistries {
		if s.agentConfig.UpstreamRegistries[index].Name == upstream.Name {
			s.agentConfig.UpstreamRegistries[index] = upstream
			upstreamFound = true

			break
		}
	}

	if !upstreamFound {
		s.agentConfig.UpstreamRegistries = append(s.agentConfig.UpstreamRegistries, upstream)
	}

	routeFound := false

	for index := range s.routes.Registries {
		if s.routes.Registries[index].Host == route.Host {
			s.routes.Registries[index] = route
			routeFound = true

			break
		}
	}

	if !routeFound {
		s.routes.Registries = append(s.routes.Registries, route)
	}

	s.auth[upstream.Name] = auth
	s.sort()
}

func (s *registryStore) removeRegistry(host string) (registryAuthRecord, bool) {
	auth, found := s.auth[host]
	if !found {
		return registryAuthRecord{}, false
	}

	delete(s.auth, host)

	upstreams := s.agentConfig.UpstreamRegistries[:0]
	for _, upstream := range s.agentConfig.UpstreamRegistries {
		if upstream.Name != host {
			upstreams = append(upstreams, upstream)
		}
	}

	s.agentConfig.UpstreamRegistries = upstreams

	routes := s.routes.Registries[:0]
	for _, route := range s.routes.Registries {
		if route.Host != host {
			routes = append(routes, route)
		}
	}

	s.routes.Registries = routes

	return auth, true
}

func (s *registryStore) removeRoute(host string) bool {
	found := false

	routes := s.routes.Registries[:0]
	for _, route := range s.routes.Registries {
		if route.Host == host {
			found = true
			continue
		}

		routes = append(routes, route)
	}

	s.routes.Registries = routes

	return found
}

func (s *registryStore) manageRouteReplacements(host string) bool {
	for index := range s.routes.Registries {
		if s.routes.Registries[index].Host == host {
			if s.routes.Registries[index].ManageReplacements {
				return false
			}

			s.routes.Registries[index].ManageReplacements = true

			return true
		}
	}

	return false
}

func applyAgentState(ctx context.Context, kubeClient kubernetes.Interface, namespace string, store *registryStore) error {
	if err := store.encode(); err != nil {
		return err
	}

	if err := setAgentPayload(ctx, kubeClient, namespace, store.configData, store.secret); err != nil {
		return err
	}

	return rollDaemonSet(ctx, kubeClient, namespace, agentDaemonSetName, "gantryctl.io/config-hash", agentStateHash(store.configData, store.secret))
}

func setAgentPayload(ctx context.Context, kubeClient kubernetes.Interface, namespace string, configData map[string]string, secret *corev1.Secret) error {
	if secret != nil {
		staged, err := stagedSecret(ctx, kubeClient, namespace, secret)
		if err != nil {
			return err
		}

		if err := setSecret(ctx, kubeClient, namespace, staged); err != nil {
			return err
		}
	}

	if err := setConfigMapData(ctx, kubeClient, namespace, agentConfigMapName, configData); err != nil {
		return err
	}

	return setSecret(ctx, kubeClient, namespace, secret)
}

func stagedSecret(ctx context.Context, kubeClient kubernetes.Interface, namespace string, desired *corev1.Secret) (*corev1.Secret, error) {
	staged := desired.DeepCopy()

	existing, err := kubeClient.CoreV1().Secrets(namespace).Get(ctx, registryCredentialsName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return staged, nil
	}

	if err != nil {
		return nil, fmt.Errorf("get registry credentials for staged update: %w", err)
	}

	if existing.Labels["app.kubernetes.io/managed-by"] != fieldManager {
		return nil, fmt.Errorf("secret %s/%s is not owned by gantryctl", namespace, registryCredentialsName)
	}

	if staged.Data == nil {
		staged.Data = map[string][]byte{}
	}

	for key, value := range existing.Data {
		if _, replaced := staged.Data[key]; !replaced {
			staged.Data[key] = bytes.Clone(value)
		}
	}

	return staged, nil
}

func applyRouteState(ctx context.Context, kubeClient kubernetes.Interface, namespace string, store *registryStore) error {
	if err := store.encode(); err != nil {
		return err
	}

	if err := setConfigMapData(ctx, kubeClient, namespace, nodeRoutesConfigMapName, store.routesData); err != nil {
		return err
	}

	return rollDaemonSet(ctx, kubeClient, namespace, nodeConfigDaemonSetName, "gantryctl.io/routes-hash", stringHash(store.routesData["registries.json"]))
}

func restoreAgentSnapshot(ctx context.Context, kubeClient kubernetes.Interface, namespace string, snapshot registrySnapshot) error {
	if err := setAgentPayload(ctx, kubeClient, namespace, snapshot.configData, snapshot.secret); err != nil {
		return err
	}

	return rollDaemonSet(ctx, kubeClient, namespace, agentDaemonSetName, "gantryctl.io/config-hash", agentStateHash(snapshot.configData, snapshot.secret))
}

func restoreRouteSnapshot(ctx context.Context, kubeClient kubernetes.Interface, namespace string, snapshot registrySnapshot) error {
	if err := setConfigMapData(ctx, kubeClient, namespace, nodeRoutesConfigMapName, snapshot.routesData); err != nil {
		return err
	}

	return rollDaemonSet(ctx, kubeClient, namespace, nodeConfigDaemonSetName, "gantryctl.io/routes-hash", stringHash(snapshot.routesData["registries.json"]))
}

func rollbackAgentSnapshot(ctx context.Context, kubeClient kubernetes.Interface, namespace string, timeout time.Duration, snapshot registrySnapshot) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	if err := restoreAgentSnapshot(rollbackCtx, kubeClient, namespace, snapshot); err != nil {
		return err
	}

	return waitForDaemonSet(rollbackCtx, kubeClient, namespace, agentDaemonSetName, timeout)
}

func rollbackRouteSnapshot(ctx context.Context, kubeClient kubernetes.Interface, namespace string, timeout time.Duration, snapshot registrySnapshot) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	if err := restoreRouteSnapshot(rollbackCtx, kubeClient, namespace, snapshot); err != nil {
		return err
	}

	return waitForDaemonSet(rollbackCtx, kubeClient, namespace, nodeConfigDaemonSetName, timeout)
}

func rollbackRegistrySnapshot(ctx context.Context, kubeClient kubernetes.Interface, namespace string, timeout time.Duration, snapshot registrySnapshot, routeTouched, routeWasActive bool) error {
	if !routeTouched {
		return rollbackAgentSnapshot(ctx, kubeClient, namespace, timeout, snapshot)
	}

	if routeWasActive {
		if err := rollbackAgentSnapshot(ctx, kubeClient, namespace, timeout, snapshot); err != nil {
			return err
		}

		return rollbackRouteSnapshot(ctx, kubeClient, namespace, timeout, snapshot)
	}

	if err := rollbackRouteSnapshot(ctx, kubeClient, namespace, timeout, snapshot); err != nil {
		return err
	}

	return rollbackAgentSnapshot(ctx, kubeClient, namespace, timeout, snapshot)
}

func setConfigMapData(ctx context.Context, kubeClient kubernetes.Interface, namespace, name string, data map[string]string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		configMap, err := kubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if configMap.Labels["app.kubernetes.io/managed-by"] != fieldManager {
			return fmt.Errorf("ConfigMap %s/%s is not owned by gantryctl", namespace, name)
		}

		configMap.Data = copyStringMap(data)
		_, err = kubeClient.CoreV1().ConfigMaps(namespace).Update(ctx, configMap, metav1.UpdateOptions{})

		return err
	})
}

func setSecret(ctx context.Context, kubeClient kubernetes.Interface, namespace string, desired *corev1.Secret) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := kubeClient.CoreV1().Secrets(namespace).Get(ctx, registryCredentialsName, metav1.GetOptions{})
		if desired == nil {
			if apierrors.IsNotFound(err) {
				return nil
			}

			if err != nil {
				return err
			}

			if existing.Labels["app.kubernetes.io/managed-by"] != fieldManager {
				return fmt.Errorf("secret %s/%s is not owned by gantryctl", namespace, registryCredentialsName)
			}

			return kubeClient.CoreV1().Secrets(namespace).Delete(ctx, existing.Name, metav1.DeleteOptions{})
		}

		if apierrors.IsNotFound(err) {
			copy := desired.DeepCopy()
			copy.ResourceVersion = ""
			_, err = kubeClient.CoreV1().Secrets(namespace).Create(ctx, copy, metav1.CreateOptions{})

			return err
		}

		if err != nil {
			return err
		}

		if existing.Labels["app.kubernetes.io/managed-by"] != fieldManager {
			return fmt.Errorf("secret %s/%s is not owned by gantryctl", namespace, registryCredentialsName)
		}

		existing.Data = copyByteMap(desired.Data)

		existing.Type = corev1.SecretTypeOpaque
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}

		existing.Labels["app.kubernetes.io/managed-by"] = fieldManager
		_, err = kubeClient.CoreV1().Secrets(namespace).Update(ctx, existing, metav1.UpdateOptions{})

		return err
	})
}

func rollDaemonSet(ctx context.Context, kubeClient kubernetes.Interface, namespace, name, annotation, value string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		daemonSet, err := kubeClient.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if daemonSet.Labels["app.kubernetes.io/managed-by"] != fieldManager {
			return fmt.Errorf("DaemonSet %s/%s is not owned by gantryctl", namespace, name)
		}

		if daemonSet.Spec.Template.Annotations == nil {
			daemonSet.Spec.Template.Annotations = map[string]string{}
		}

		if daemonSet.Spec.Template.Annotations[annotation] == value {
			return nil
		}

		daemonSet.Spec.Template.Annotations[annotation] = value
		_, err = kubeClient.AppsV1().DaemonSets(namespace).Update(ctx, daemonSet, metav1.UpdateOptions{})

		return err
	})
}

func installedAgentImage(ctx context.Context, kubeClient kubernetes.Interface, namespace string) (string, error) {
	daemonSet, err := kubeClient.AppsV1().DaemonSets(namespace).Get(ctx, agentDaemonSetName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get standalone Gantry DaemonSet: %w", err)
	}

	if daemonSet.Labels["app.kubernetes.io/managed-by"] != fieldManager {
		return "", fmt.Errorf("DaemonSet %s/%s is not owned by gantryctl", namespace, agentDaemonSetName)
	}

	for _, container := range daemonSet.Spec.Template.Spec.Containers {
		if container.Name == "gantry" {
			return container.Image, nil
		}
	}

	return "", errors.New("standalone Gantry DaemonSet has no gantry container")
}

func newOwnedSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      registryCredentialsName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": fieldManager,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{},
	}
}

func credentialKey(host string) string {
	digest := sha256.Sum256([]byte(host))
	return "registry-" + hex.EncodeToString(digest[:8])
}

func agentStateHash(configData map[string]string, secret *corev1.Secret) string {
	var buffer bytes.Buffer

	keys := make([]string, 0, len(configData))
	for key := range configData {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		buffer.WriteString(key)
		buffer.WriteByte(0)
		buffer.WriteString(configData[key])
		buffer.WriteByte(0)
	}

	if secret != nil {
		secretKeys := make([]string, 0, len(secret.Data))
		for key := range secret.Data {
			secretKeys = append(secretKeys, key)
		}

		sort.Strings(secretKeys)

		for _, key := range secretKeys {
			buffer.WriteString(key)
			buffer.WriteByte(0)
			buffer.Write(secret.Data[key])
			buffer.WriteByte(0)
		}
	}

	return stringHash(buffer.String())
}

func stringHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func copyStringMap(source map[string]string) map[string]string {
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}

	return copy
}

func copyByteMap(source map[string][]byte) map[string][]byte {
	copy := make(map[string][]byte, len(source))
	for key, value := range source {
		copy[key] = bytes.Clone(value)
	}

	return copy
}

func daemonSetReady(daemonSet *appsv1.DaemonSet) bool {
	return daemonSet.Status.DesiredNumberScheduled > 0 &&
		daemonSet.Status.ObservedGeneration >= daemonSet.Generation &&
		daemonSet.Status.UpdatedNumberScheduled == daemonSet.Status.DesiredNumberScheduled &&
		daemonSet.Status.NumberReady == daemonSet.Status.DesiredNumberScheduled
}
