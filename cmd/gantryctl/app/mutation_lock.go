// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const registryMutationLeaseName = "gantryctl-registry-mutation"

const (
	registryMutationLeaseDuration = 30 * time.Second
	registryMutationRenewInterval = 10 * time.Second
)

func withRegistryMutationLock(ctx context.Context, kubeClient kubernetes.Interface, namespace string, timeout time.Duration, run func(context.Context) error) error {
	holder, err := randomHolderIdentity()
	if err != nil {
		return err
	}

	acquireCtx, acquireCancel := context.WithTimeout(ctx, timeout)
	defer acquireCancel()

	if err := acquireRegistryMutationLease(acquireCtx, kubeClient, namespace, holder); err != nil {
		return fmt.Errorf("acquire Gantry registry mutation lock: %w", err)
	}

	operationCtx, operationCancel := context.WithCancel(ctx)

	var (
		renewErr error
		renewMu  sync.Mutex
	)

	renewDone := make(chan struct{})

	go func() {
		defer close(renewDone)

		if err := renewRegistryMutationLease(operationCtx, kubeClient, namespace, holder); err != nil && !errors.Is(err, context.Canceled) {
			renewMu.Lock()
			renewErr = err
			renewMu.Unlock()
			operationCancel()
		}
	}()

	runErr := run(operationCtx)

	operationCancel()
	<-renewDone

	releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	releaseErr := releaseRegistryMutationLease(releaseCtx, kubeClient, namespace, holder)

	releaseCancel()

	renewMu.Lock()
	defer renewMu.Unlock()

	return errors.Join(runErr, renewErr, releaseErr)
}

func randomHolderIdentity() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate mutation lock identity: %w", err)
	}

	return "gantryctl-" + hex.EncodeToString(value), nil
}

func acquireRegistryMutationLease(ctx context.Context, kubeClient kubernetes.Interface, namespace, holder string) error {
	leases := kubeClient.CoordinationV1().Leases(namespace)

	return wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		now := metav1.NewMicroTime(time.Now())

		lease, err := leases.Get(ctx, registryMutationLeaseName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			durationSeconds := int32(registryMutationLeaseDuration / time.Second)

			_, err = leases.Create(ctx, &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      registryMutationLeaseName,
					Namespace: namespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": fieldManager,
					},
				},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       &holder,
					LeaseDurationSeconds: &durationSeconds,
					AcquireTime:          &now,
					RenewTime:            &now,
				},
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				return false, nil
			}

			return err == nil, err
		}

		if err != nil {
			return false, err
		}

		if lease.Labels["app.kubernetes.io/managed-by"] != fieldManager {
			return false, fmt.Errorf("lease %s/%s is not owned by gantryctl", namespace, registryMutationLeaseName)
		}

		if leaseHeld(lease, now.Time) {
			return false, nil
		}

		durationSeconds := int32(registryMutationLeaseDuration / time.Second)
		lease.Spec.HolderIdentity = &holder
		lease.Spec.LeaseDurationSeconds = &durationSeconds
		lease.Spec.AcquireTime = &now
		lease.Spec.RenewTime = &now

		_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			return false, nil
		}

		return err == nil, err
	})
}

func leaseHeld(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return false
	}

	duration := registryMutationLeaseDuration
	if lease.Spec.LeaseDurationSeconds != nil {
		duration = time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	}

	renewed := lease.CreationTimestamp.Time
	if lease.Spec.RenewTime != nil {
		renewed = lease.Spec.RenewTime.Time
	} else if lease.Spec.AcquireTime != nil {
		renewed = lease.Spec.AcquireTime.Time
	}

	return now.Before(renewed.Add(duration))
}

func renewRegistryMutationLease(ctx context.Context, kubeClient kubernetes.Interface, namespace, holder string) error {
	ticker := time.NewTicker(registryMutationRenewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := updateRegistryMutationLease(ctx, kubeClient, namespace, holder, false); err != nil {
				return fmt.Errorf("renew Gantry registry mutation lock: %w", err)
			}
		}
	}
}

func releaseRegistryMutationLease(ctx context.Context, kubeClient kubernetes.Interface, namespace, holder string) error {
	if err := updateRegistryMutationLease(ctx, kubeClient, namespace, holder, true); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("release Gantry registry mutation lock: %w", err)
	}

	return nil
}

func updateRegistryMutationLease(ctx context.Context, kubeClient kubernetes.Interface, namespace, holder string, release bool) error {
	leases := kubeClient.CoordinationV1().Leases(namespace)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		lease, err := leases.Get(ctx, registryMutationLeaseName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
			return errors.New("gantry registry mutation lock ownership was lost")
		}

		now := metav1.NewMicroTime(time.Now())

		if release {
			empty := ""
			lease.Spec.HolderIdentity = &empty
		} else {
			lease.Spec.RenewTime = &now
		}

		_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})

		return err
	})
}
