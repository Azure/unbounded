// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package certs

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	aggregator "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	aggregatorinformers "k8s.io/kube-aggregator/pkg/client/informers/externalversions/apiregistration/v1"
	"k8s.io/utils/clock"
)

const (
	SecretName    = "rebound-serving"
	nextAddedAt   = "rebound.unbounded-cloud.io/next-ca-added-at"
	prevDropAt    = "rebound.unbounded-cloud.io/prev-ca-drop-at"
	leafExpiresAt = "rebound.unbounded-cloud.io/ca-leaf-expires-at"
	retryInterval = 10 * time.Second
)

// Rotator is a leader-elected runnable. Set its fields before starting it.
// Zero durations use defaults: CA 365 days, leaf 30 days, grace 10 minutes.
type Rotator struct {
	Client         kubernetes.Interface
	Aggregator     aggregator.Interface
	Namespace      string
	ServiceName    string
	APIServiceName string
	CALifetime     time.Duration
	LeafLifetime   time.Duration
	CAGrace        time.Duration
	Clock          clock.Clock
}

func (r *Rotator) defaults() error {
	if r.Client == nil || r.Aggregator == nil {
		return fmt.Errorf("certificate rotator requires Kubernetes and aggregator clients")
	}

	if r.Namespace == "" {
		r.Namespace = "unbounded-system"
	}

	if r.ServiceName == "" {
		r.ServiceName = "rebound"
	}

	if r.APIServiceName == "" {
		r.APIServiceName = "v1alpha1.rebound.unbounded-cloud.io"
	}

	if r.CALifetime == 0 {
		r.CALifetime = 365 * 24 * time.Hour
	}

	if r.LeafLifetime == 0 {
		r.LeafLifetime = 30 * 24 * time.Hour
	}

	if r.CAGrace == 0 {
		r.CAGrace = 10 * time.Minute
	}

	if r.Clock == nil {
		r.Clock = clock.RealClock{}
	}

	if r.LeafLifetime < time.Second || r.CAGrace < time.Second || r.CALifetime < time.Second || r.CAGrace >= r.CALifetime/3 {
		return fmt.Errorf("lifetimes and grace must be positive, and grace must be less than one third of CA lifetime")
	}

	return nil
}

func (r *Rotator) dnsNames() []string {
	base := r.ServiceName + "." + r.Namespace
	return []string{base + ".svc", base + ".svc.cluster.local", r.ServiceName, base}
}

// Reconcile persists certificates before publishing trust. A promotion is allowed
// only after the expanded APIService bundle has been published for a full grace.
func (r *Rotator) Reconcile(ctx context.Context) (time.Duration, error) {
	if err := r.defaults(); err != nil {
		return retryInterval, err
	}

	now := r.Clock.Now()
	secrets := r.Client.CoreV1().Secrets(r.Namespace)
	secret, err := secrets.Get(ctx, SecretName, metav1.GetOptions{})

	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return retryInterval, err
	}

	if missing {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: r.Namespace}, Type: corev1.SecretTypeTLS}
	}

	original := secret.DeepCopy()
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}

	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}

	// Verify publication on every pass. If trust was lost or changed, restart
	// the grace rather than promoting based on an old timestamp.
	published := false
	if len(secret.Data["next-ca.crt"]) > 0 {
		published, err = r.bundlePublished(ctx, secret.Data["ca-bundle.crt"])
		if err != nil {
			return retryInterval, err
		}

		expected := bytes.Join([][]byte{secret.Data["ca.crt"], secret.Data["next-ca.crt"], secret.Data["prev-ca.crt"]}, nil)
		published = published && bytes.Equal(expected, secret.Data["ca-bundle.crt"])
	}

	due, err := r.reconcileSecret(secret, now, published)
	if err != nil {
		return retryInterval, err
	}

	if missing {
		_, err = secrets.Create(ctx, secret, metav1.CreateOptions{})
	} else if !reflect.DeepEqual(original, secret) {
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
	}

	if err != nil {
		return retryInterval, err
	}

	ok, err := r.syncBundle(ctx, secret.Data["ca-bundle.crt"])
	if err != nil {
		return retryInterval, err
	}

	if !ok {
		return retryInterval, nil
	}

	return max(time.Second, min(time.Hour, due.Sub(now))), nil
}

func (r *Rotator) reconcileSecret(s *corev1.Secret, now time.Time, published bool) (time.Time, error) {
	ca, signer, err := loadCA(s.Data["ca.crt"], s.Data["ca.key"], now)
	if err != nil {
		// A broken signing key cannot be rotated gracefully. Recover service;
		// healthy rotations below always preserve overlapping trust.
		cert, key, issueErr := issue(now, r.CALifetime, nil, nil, nil)
		if issueErr != nil {
			return now, issueErr
		}

		s.Data["ca.crt"], s.Data["ca.key"] = cert, key
		delete(s.Data, "next-ca.crt")
		delete(s.Data, "next-ca.key")
		delete(s.Annotations, nextAddedAt)
		delete(s.Annotations, leafExpiresAt)

		ca, signer, err = loadCA(cert, key, now)
		if err != nil {
			return now, err
		}
	}

	// The high-water mark covers every leaf issued by this CA, including
	// leaves cached on another replica and leaves with older lifetime settings.
	leaf, _, leafErr := loadPair(s.Data["tls.crt"], s.Data["tls.key"])
	if leafErr == nil && leaf.CheckSignatureFrom(ca) == nil {
		recordExpiry(s, leaf.NotAfter)
	}

	r.dropPrevious(s, now)

	if len(s.Data["next-ca.crt"]) > 0 {
		next, _, nextErr := loadCA(s.Data["next-ca.crt"], s.Data["next-ca.key"], now)
		if nextErr != nil || !now.Before(renewAt(next)) {
			delete(s.Data, "next-ca.crt")
			delete(s.Data, "next-ca.key")
			delete(s.Annotations, nextAddedAt)
		} else {
			added, parseErr := time.Parse(time.RFC3339Nano, s.Annotations[nextAddedAt])
			if !published {
				// Do not write a fresh timestamp on every failed publication:
				// our Secret watch would turn that into a busy retry loop.
				delete(s.Annotations, nextAddedAt)
			} else if parseErr != nil || added.After(now) {
				s.Annotations[nextAddedAt] = now.Format(time.RFC3339Nano)
			} else if !now.Before(added.Add(r.CAGrace)) {
				drop, parseErr := time.Parse(time.RFC3339Nano, s.Annotations[leafExpiresAt])
				if parseErr != nil {
					drop = ca.NotAfter
				}

				s.Data["prev-ca.crt"] = s.Data["ca.crt"]
				s.Annotations[prevDropAt] = earlier(drop, ca.NotAfter).Format(time.RFC3339Nano)
				s.Data["ca.crt"], s.Data["ca.key"] = s.Data["next-ca.crt"], s.Data["next-ca.key"]
				delete(s.Data, "next-ca.crt")
				delete(s.Data, "next-ca.key")
				delete(s.Annotations, nextAddedAt)
				delete(s.Annotations, leafExpiresAt)

				ca, signer, err = loadCA(s.Data["ca.crt"], s.Data["ca.key"], now)
				if err != nil {
					return now, err
				}
			}
		}
	}

	// Do not overwrite a previous CA that is still draining.
	if !now.Before(renewAt(ca)) && len(s.Data["next-ca.crt"]) == 0 && len(s.Data["prev-ca.crt"]) == 0 {
		cert, key, issueErr := issue(now, r.CALifetime, nil, nil, nil)
		if issueErr != nil {
			return now, issueErr
		}

		s.Data["next-ca.crt"], s.Data["next-ca.key"] = cert, key
		s.Annotations[nextAddedAt] = now.Format(time.RFC3339Nano)
	}

	if leafErr != nil || !validLeaf(leaf, ca, r.dnsNames(), now) {
		cert, key, issueErr := issue(now, r.LeafLifetime, r.dnsNames(), ca, signer)
		if issueErr != nil {
			return now, issueErr
		}

		s.Data["tls.crt"], s.Data["tls.key"] = cert, key

		leaf, _, err = loadPair(cert, key)
		if err != nil {
			return now, err
		}

		recordExpiry(s, leaf.NotAfter)
	}

	s.Data["ca-bundle.crt"] = bytes.Join([][]byte{s.Data["ca.crt"], s.Data["next-ca.crt"], s.Data["prev-ca.crt"]}, nil)
	due := renewAt(leaf)

	if len(s.Data["next-ca.crt"]) > 0 {
		added, parseErr := time.Parse(time.RFC3339Nano, s.Annotations[nextAddedAt])
		if parseErr != nil {
			due = earlier(due, now.Add(retryInterval))
		} else {
			due = earlier(due, added.Add(r.CAGrace))
		}
	} else if len(s.Data["prev-ca.crt"]) == 0 {
		due = earlier(due, renewAt(ca))
	}

	if len(s.Data["prev-ca.crt"]) > 0 {
		drop, parseErr := time.Parse(time.RFC3339Nano, s.Annotations[prevDropAt])
		if parseErr != nil {
			return now, parseErr
		}

		due = earlier(due, drop)
	}

	return due, nil
}

func recordExpiry(s *corev1.Secret, expires time.Time) {
	old, err := time.Parse(time.RFC3339Nano, s.Annotations[leafExpiresAt])
	if err != nil || expires.After(old) {
		s.Annotations[leafExpiresAt] = expires.Format(time.RFC3339Nano)
	}
}

func (r *Rotator) dropPrevious(s *corev1.Secret, now time.Time) {
	if len(s.Data["prev-ca.crt"]) == 0 {
		return
	}

	previous, err := parseCertificate(s.Data["prev-ca.crt"])
	if err != nil {
		delete(s.Data, "prev-ca.crt")
		delete(s.Annotations, prevDropAt)

		return
	}

	drop, err := time.Parse(time.RFC3339Nano, s.Annotations[prevDropAt])
	if err != nil {
		drop = previous.NotAfter
	}

	drop = earlier(drop, previous.NotAfter)

	s.Annotations[prevDropAt] = drop.Format(time.RFC3339Nano)
	if !now.Before(drop) {
		delete(s.Data, "prev-ca.crt")
		delete(s.Annotations, prevDropAt)
	}
}

func (r *Rotator) bundlePublished(ctx context.Context, bundle []byte) (bool, error) {
	service, err := r.Aggregator.ApiregistrationV1().APIServices().Get(ctx, r.APIServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	return !service.Spec.InsecureSkipTLSVerify && bytes.Equal(service.Spec.CABundle, bundle), nil
}

func (r *Rotator) syncBundle(ctx context.Context, bundle []byte) (bool, error) {
	services := r.Aggregator.ApiregistrationV1().APIServices()

	service, err := services.Get(ctx, r.APIServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, err
	}

	if !service.Spec.InsecureSkipTLSVerify && bytes.Equal(service.Spec.CABundle, bundle) {
		return true, nil
	}

	service.Spec.CABundle = bytes.Clone(bundle)
	service.Spec.InsecureSkipTLSVerify = false
	_, err = services.Update(ctx, service, metav1.UpdateOptions{})

	return err == nil, err
}

func (r *Rotator) NeedLeaderElection() bool { return true }

// Start retries transient failures and reacts to both trust and Secret changes.
func (r *Rotator) Start(ctx context.Context) error {
	if err := r.defaults(); err != nil {
		return err
	}

	changed := make(chan struct{}, 1)
	notify := func(any) {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	secretInformer := coreinformers.NewFilteredSecretInformer(r.Client, r.Namespace, 0, cache.Indexers{}, selectName(SecretName))

	apiInformer := aggregatorinformers.NewFilteredAPIServiceInformer(r.Aggregator, 0, cache.Indexers{}, selectName(r.APIServiceName))
	for _, informer := range []cache.SharedIndexInformer{secretInformer, apiInformer} {
		_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: notify, UpdateFunc: func(_, obj any) { notify(obj) }, DeleteFunc: notify})
		if err != nil {
			return err
		}
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{}, 2)

	for _, informer := range []cache.SharedIndexInformer{secretInformer, apiInformer} {
		go func() { informer.Run(watchCtx.Done()); done <- struct{}{} }()
	}

	defer func() { cancel(); <-done; <-done }()

	for ctx.Err() == nil {
		delay, err := r.Reconcile(ctx)
		if err != nil {
			klog.ErrorS(err, "Rebound certificate reconciliation failed")

			delay = retryInterval
		}

		timer := r.Clock.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-changed:
		case <-timer.C():
		}

		timer.Stop()
	}

	return nil
}

func selectName(name string) func(*metav1.ListOptions) {
	return func(options *metav1.ListOptions) {
		options.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
	}
}
