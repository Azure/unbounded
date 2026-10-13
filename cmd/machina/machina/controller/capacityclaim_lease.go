// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrDomainLocked       = errors.New("capacity domain is locked")
	domainLockInvalidChar = regexp.MustCompile(`[^a-z0-9-]+`)
)

// DomainLocker serializes reservation and release mutations within a capacity
// domain.
type DomainLocker interface {
	WithLock(ctx context.Context, domain string, fn func(context.Context) error) error
}

// KubernetesDomainLocker coordinates through durable Lease objects.
type KubernetesDomainLocker struct {
	Client         client.Client
	Namespace      string
	HolderIdentity string
	LeaseDuration  time.Duration
	Now            func() time.Time
	local          MemoryDomainLocker
}

func (l *KubernetesDomainLocker) WithLock(
	ctx context.Context,
	domain string,
	fn func(context.Context) error,
) error {
	return l.local.WithLock(ctx, domain, func(localCtx context.Context) error {
		return l.withLease(localCtx, domain, fn)
	})
}

func (l *KubernetesDomainLocker) withLease(
	ctx context.Context,
	domain string,
	fn func(context.Context) error,
) error {
	if err := l.acquire(ctx, domain); err != nil {
		return err
	}

	lockCtx, cancel := context.WithCancel(ctx)
	renewDone := make(chan error, 1)

	go func() {
		ticker := time.NewTicker(l.leaseDuration() / 3)
		defer ticker.Stop()

		for {
			select {
			case <-lockCtx.Done():
				renewDone <- nil

				return
			case <-ticker.C:
				if err := l.acquire(lockCtx, domain); err != nil {
					renewDone <- fmt.Errorf("renew domain Lease: %w", err)

					cancel()

					return
				}
			}
		}
	}()

	err := fn(lockCtx)

	cancel()

	renewErr := <-renewDone
	releaseErr := l.release(context.WithoutCancel(ctx), domain)

	if err != nil {
		return err
	}

	if renewErr != nil {
		return renewErr
	}

	return releaseErr
}

func (l *KubernetesDomainLocker) acquire(ctx context.Context, domain string) error {
	duration := l.leaseDuration()

	now := l.now()
	key := types.NamespacedName{Namespace: l.Namespace, Name: capacityDomainLeaseName(domain)}

	return retry.OnError(wait.Backoff{Steps: 4, Duration: 25 * time.Millisecond, Factor: 2}, apierrors.IsConflict, func() error {
		var lease coordinationv1.Lease
		if err := l.Client.Get(ctx, key, &lease); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("get domain Lease: %w", err)
			}

			seconds := int32(duration / time.Second)
			renewTime := metav1.NewMicroTime(now)
			lease = coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       &l.HolderIdentity,
					LeaseDurationSeconds: &seconds,
					AcquireTime:          &renewTime,
					RenewTime:            &renewTime,
				},
			}

			if err := l.Client.Create(ctx, &lease); err != nil {
				if apierrors.IsAlreadyExists(err) {
					return apierrors.NewConflict(
						coordinationv1.Resource("leases"),
						key.Name,
						err,
					)
				}

				return fmt.Errorf("create domain Lease: %w", err)
			}

			return nil
		}

		if !leaseAvailable(&lease, l.HolderIdentity, now) {
			return ErrDomainLocked
		}

		seconds := int32(duration / time.Second)
		renewTime := metav1.NewMicroTime(now)
		lease.Spec.HolderIdentity = &l.HolderIdentity
		lease.Spec.LeaseDurationSeconds = &seconds

		lease.Spec.RenewTime = &renewTime
		if lease.Spec.AcquireTime == nil {
			lease.Spec.AcquireTime = &renewTime
		}

		if err := l.Client.Update(ctx, &lease); err != nil {
			return fmt.Errorf("update domain Lease: %w", err)
		}

		return nil
	})
}

func (l *KubernetesDomainLocker) release(ctx context.Context, domain string) error {
	key := types.NamespacedName{Namespace: l.Namespace, Name: capacityDomainLeaseName(domain)}

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var lease coordinationv1.Lease
		if err := l.Client.Get(ctx, key, &lease); err != nil {
			return client.IgnoreNotFound(err)
		}

		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != l.HolderIdentity {
			return nil
		}

		lease.Spec.HolderIdentity = nil
		lease.Spec.RenewTime = nil

		return l.Client.Update(ctx, &lease)
	})
}

func (l *KubernetesDomainLocker) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}

	return time.Now()
}

func (l *KubernetesDomainLocker) leaseDuration() time.Duration {
	if l.LeaseDuration > 0 {
		return l.LeaseDuration
	}

	return 30 * time.Second
}

func leaseAvailable(lease *coordinationv1.Lease, holder string, now time.Time) bool {
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" || *lease.Spec.HolderIdentity == holder {
		return true
	}

	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}

	expires := lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)

	return !expires.After(now)
}

func capacityDomainLeaseName(domain string) string {
	sanitized := strings.ToLower(domain)
	sanitized = domainLockInvalidChar.ReplaceAllString(sanitized, "-")

	sanitized = strings.Trim(sanitized, "-")
	if sanitized == "" {
		sanitized = "default"
	}

	const prefix = "capacityclaim-"
	if len(prefix)+len(sanitized) <= 63 {
		return prefix + sanitized
	}

	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(domain)))[:12]
	maxDomainLength := 63 - len(prefix) - len(hash) - 1

	return prefix + strings.Trim(sanitized[:maxDomainLength], "-") + "-" + hash
}

// MemoryDomainLocker is a deterministic fake used by unit tests.
type MemoryDomainLocker struct {
	mu     sync.Mutex
	locked map[string]bool
}

func (l *MemoryDomainLocker) WithLock(
	ctx context.Context,
	domain string,
	fn func(context.Context) error,
) error {
	l.mu.Lock()
	if l.locked == nil {
		l.locked = map[string]bool{}
	}

	if l.locked[domain] {
		l.mu.Unlock()

		return ErrDomainLocked
	}

	l.locked[domain] = true
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		delete(l.locked, domain)
		l.mu.Unlock()
	}()

	return fn(ctx)
}
