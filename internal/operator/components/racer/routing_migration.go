// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

// Bridge the old leader-only readiness contract before narrowing the Service.
// Probe the actual process's named checks, never infer leadership from PodReady
// or an image tag. All legacy replicas may carry the hint because only their
// elected leader can become Ready. New replicas clear it before starting and
// refuse readiness until the Service selector has migrated.
func planRoutingMigration(ctx context.Context, env *component.Env, plan *component.Plan) ([]component.ObjectRef, error) {
	var service corev1.Service

	err := env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: controlPlaneName}, &service)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	if service.Spec.Selector[racermeta.MetadataPrefix+"serving-leader"] == "true" {
		return nil, nil
	}

	if !reflect.DeepEqual(service.Spec.Selector, map[string]string{componentLabel: controlPlaneName}) {
		return nil, fmt.Errorf("cannot migrate unexpected Racer Service selector: %v", service.Spec.Selector)
	}

	var pods corev1.PodList
	if err := env.LiveReader().List(ctx, &pods, client.InNamespace(env.Namespace), client.MatchingLabels{componentLabel: controlPlaneName}); err != nil {
		return nil, err
	}

	var dependencies []component.ObjectRef

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		if err := migrationPodOwner(ctx, env.LiveReader(), pod); err != nil {
			return nil, err
		}

		legacy, err := legacyReadiness(ctx, pod)
		if err != nil {
			return nil, fmt.Errorf("classify Racer Pod %s before Service migration: %w", pod.Name, err)
		}

		if !legacy {
			continue
		}

		before := component.ToUnstructured(pod)
		pod.Labels[racermeta.MetadataPrefix+"serving-leader"] = "true"
		pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
		op := component.Operation{Kind: component.OpMergePatch, Base: before, Object: component.ToUnstructured(pod), Component: controlPlaneName}
		plan.Add(op)
		dependencies = append(dependencies, op.Ref())
	}

	return dependencies, nil
}

func migrationPodOwner(ctx context.Context, reader client.Reader, pod *corev1.Pod) error {
	owner := metav1.GetControllerOf(pod)
	if pod.UID == "" || pod.Spec.ServiceAccountName != controlPlaneName || owner == nil || owner.Kind != "ReplicaSet" || owner.APIVersion != "apps/v1" {
		return fmt.Errorf("pod %s is not a managed Racer replica", pod.Name)
	}

	var rs appsv1.ReplicaSet
	if err := reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}, &rs); err != nil {
		return err
	}

	if rs.UID != owner.UID {
		return fmt.Errorf("ReplicaSet UID changed for %s", pod.Name)
	}

	owner = metav1.GetControllerOf(&rs)
	if owner == nil || owner.Kind != "Deployment" || owner.APIVersion != "apps/v1" || owner.Name != controlPlaneName {
		return fmt.Errorf("ReplicaSet %s is not owned by Racer", rs.Name)
	}

	var deployment appsv1.Deployment
	if err := reader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}, &deployment); err != nil {
		return err
	}

	if deployment.UID != owner.UID {
		return fmt.Errorf("deployment UID changed for %s", pod.Name)
	}

	return nil
}

func legacyReadiness(ctx context.Context, pod *corev1.Pod) (bool, error) {
	if net.ParseIP(pod.Status.PodIP) == nil {
		return false, fmt.Errorf("missing Pod IP")
	}

	port := "8081"

	for _, container := range pod.Spec.Containers {
		if container.Name == "controller" {
			for _, p := range container.Ports {
				if p.Name == "health" {
					port = strconv.Itoa(int(p.ContainerPort))
				}
			}
		}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(pod.Status.PodIP, port)+"/readyz?verbose", nil)
	if err != nil {
		return false, err
	}

	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()

	response, err := (&http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
	if err != nil {
		return false, err
	}

	defer func() {
		if err := response.Body.Close(); err != nil {
			log.Printf("close legacy readiness response: %v", err)
		}
	}()

	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return false, err
	}

	return classifyReadiness(response.StatusCode, string(body))
}

func classifyReadiness(status int, body string) (bool, error) {
	if status != http.StatusOK && status != http.StatusInternalServerError {
		return false, fmt.Errorf("unexpected readiness status %d", status)
	}

	lines := "\n" + body
	check := func(name string) bool {
		return strings.Contains(lines, "\n[+]"+name+" ok\n") || strings.Contains(lines, "\n[-]"+name+" failed: reason withheld\n")
	}

	legacy, warm := check("leader-tls"), check("replica-tls")
	if legacy == warm {
		return false, fmt.Errorf("unknown readiness contract")
	}

	return legacy, nil
}
