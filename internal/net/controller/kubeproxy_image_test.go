// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

const (
	testProviderKubeProxyImage = "provider.example/kube-proxy:v1.34.2-1"
	testServerVersion          = "v1.34.2"
	testImageDigest            = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// kubeSystemKubeProxy is a provider kube-proxy DaemonSet whose only container
// is named and imaged as given.
func kubeSystemKubeProxy(container, image string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: metav1.NamespaceSystem},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: container, Image: image}},
		}}},
	}
}

func clientsetWithServerVersion(gitVersion string, objects ...runtime.Object) *k8sfake.Clientset {
	clientset := k8sfake.NewClientset(objects...)
	clientset.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: gitVersion}

	return clientset
}

func TestManagedKubeProxyImage(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		provider   *appsv1.DaemonSet
		want       string
	}{
		{
			name:     "unset reuses the provider image",
			provider: kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage),
			want:     testProviderKubeProxyImage,
		},
		{
			name: "unset without a provider falls back to registry.k8s.io",
			want: "registry.k8s.io/kube-proxy:" + testServerVersion,
		},
		{
			name:       "a repository takes the provider tag",
			configured: "myreg.example/kube-proxy",
			provider:   kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage),
			want:       "myreg.example/kube-proxy:v1.34.2-1",
		},
		{
			name:       "a repository without a provider takes the server version",
			configured: "myreg.example/kube-proxy",
			want:       "myreg.example/kube-proxy:" + testServerVersion,
		},
		{
			name:       "a registry port is not mistaken for a tag",
			configured: "localhost:5000/kube-proxy",
			provider:   kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage),
			want:       "localhost:5000/kube-proxy:v1.34.2-1",
		},
		{
			name:       "a pinned tag is used as-is",
			configured: "myreg.example/kube-proxy:v1.34.0",
			provider:   kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage),
			want:       "myreg.example/kube-proxy:v1.34.0",
		},
		{
			name:       "a pinned digest is used as-is",
			configured: "myreg.example/kube-proxy@" + testImageDigest,
			provider:   kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage),
			want:       "myreg.example/kube-proxy@" + testImageDigest,
		},
		{
			name:       "only the provider tag carries over, not its digest",
			configured: "myreg.example/kube-proxy",
			provider:   kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage+"@"+testImageDigest),
			want:       "myreg.example/kube-proxy:v1.34.2-1",
		},
		{
			name:       "a provider image pinned only by digest gives the server version",
			configured: "myreg.example/kube-proxy",
			provider:   kubeSystemKubeProxy("kube-proxy", "provider.example/kube-proxy@"+testImageDigest),
			want:       "myreg.example/kube-proxy:" + testServerVersion,
		},
		{
			name:       "an unparsable provider image gives the server version",
			configured: "myreg.example/kube-proxy",
			provider:   kubeSystemKubeProxy("kube-proxy", "Provider.example/Kube-Proxy:v1"),
			want:       "myreg.example/kube-proxy:" + testServerVersion,
		},
		{
			name:     "a provider without a kube-proxy container counts as absent",
			provider: kubeSystemKubeProxy("proxy", testProviderKubeProxyImage),
			want:     "registry.k8s.io/kube-proxy:" + testServerVersion,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []runtime.Object
			if tt.provider != nil {
				objects = append(objects, tt.provider)
			}

			got, err := ManagedKubeProxyImage(t.Context(), clientsetWithServerVersion(testServerVersion, objects...), tt.configured)
			if err != nil {
				t.Fatalf("ManagedKubeProxyImage() error = %v", err)
			}

			if got != tt.want {
				t.Fatalf("ManagedKubeProxyImage() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestManagedKubeProxyImagePinnedReadsNothing pins that a configured version is
// final: nothing in the cluster can change it, so nothing is read.
func TestManagedKubeProxyImagePinnedReadsNothing(t *testing.T) {
	clientset := clientsetWithServerVersion(testServerVersion, kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage))

	if _, err := ManagedKubeProxyImage(t.Context(), clientset, "myreg.example/kube-proxy:v1.34.0"); err != nil {
		t.Fatalf("ManagedKubeProxyImage() error = %v", err)
	}

	if actions := clientset.Actions(); len(actions) != 0 {
		t.Fatalf("a pinned image read the cluster: %v", actions)
	}
}

func TestManagedKubeProxyImageRejectsMalformedValues(t *testing.T) {
	for _, configured := range []string{"myreg.example/Kube-Proxy", "myreg.example/kube-proxy:", " myreg.example/kube-proxy"} {
		t.Run(configured, func(t *testing.T) {
			clientset := clientsetWithServerVersion(testServerVersion)

			if got, err := ManagedKubeProxyImage(t.Context(), clientset, configured); err == nil {
				t.Fatalf("ManagedKubeProxyImage(%q) = %q, want an error", configured, got)
			}

			if actions := clientset.Actions(); len(actions) != 0 {
				t.Fatalf("a malformed value read the cluster: %v", actions)
			}
		})
	}
}

func TestManagedKubeProxyImageRetriesProviderDiscovery(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "daemonsets"}, "kube-proxy")
	timeout := apierrors.NewTimeoutError("apiserver timeout", 0)
	internalError := apierrors.NewInternalError(errors.New("apiserver unavailable"))
	tests := []struct {
		name         string
		failures     int
		readErr      error
		finalErr     error
		wantAttempts int
		wantFallback bool
	}{
		{name: "success on the initial read", wantAttempts: 1},
		{name: "NotFound on the initial read", finalErr: notFound, wantAttempts: 1, wantFallback: true},
		{name: "timeout is retried", failures: 1, readErr: timeout, wantAttempts: 2},
		{name: "internal error is retried", failures: 1, readErr: internalError, wantAttempts: 2},
		{name: "other errors are retried", failures: 1, readErr: errors.New("connection reset"), wantAttempts: 2},
		{name: "the final retry succeeds", failures: 10, readErr: internalError, wantAttempts: 11},
		{name: "NotFound stops retries", failures: 1, readErr: timeout, finalErr: notFound, wantAttempts: 2, wantFallback: true},
		{name: "exhausted retries fall back", failures: 11, readErr: internalError, wantAttempts: 11, wantFallback: true},
	}

	for _, configured := range []struct {
		name      string
		image     string
		recovered string
		fallback  string
	}{
		{
			name:      "unset",
			recovered: testProviderKubeProxyImage,
			fallback:  "registry.k8s.io/kube-proxy:" + testServerVersion,
		},
		{
			name:      "repository",
			image:     "myreg.example/kube-proxy",
			recovered: "myreg.example/kube-proxy:v1.34.2-1",
			fallback:  "myreg.example/kube-proxy:" + testServerVersion,
		},
	} {
		t.Run(configured.name, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						clientset := clientsetWithServerVersion(testServerVersion, kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage))

						var attempts []time.Time

						clientset.PrependReactor("get", "daemonsets", func(clienttesting.Action) (bool, runtime.Object, error) {
							attempts = append(attempts, time.Now())
							if len(attempts) <= tt.failures {
								return true, nil, tt.readErr
							}

							if tt.finalErr != nil {
								return true, nil, tt.finalErr
							}

							return false, nil, nil
						})

						started := time.Now()

						got, err := ManagedKubeProxyImage(t.Context(), clientset, configured.image)
						if err != nil {
							t.Fatalf("ManagedKubeProxyImage() error = %v", err)
						}

						want := configured.recovered
						if tt.wantFallback {
							want = configured.fallback
						}

						if got != want {
							t.Fatalf("ManagedKubeProxyImage() = %q, want %q", got, want)
						}

						assertKubeProxyImageDiscoveryAttempts(t, started, attempts, tt.wantAttempts)

						wantElapsed := time.Duration(tt.wantAttempts-1) * 10 * time.Second
						if elapsed := time.Since(started); elapsed != wantElapsed {
							t.Fatalf("discovery took %v, want %v", elapsed, wantElapsed)
						}
					})
				})
			}
		})
	}
}

func TestManagedKubeProxyImageDiscoveryStopsOnCancellation(t *testing.T) {
	for _, tt := range []struct {
		name          string
		after         time.Duration
		cancelContext bool
		wantAttempts  int
		wantErr       error
	}{
		{name: "already canceled", cancelContext: true, wantErr: context.Canceled},
		{name: "canceled during a retry wait", after: 5 * time.Second, cancelContext: true, wantAttempts: 1, wantErr: context.Canceled},
		{name: "deadline during a retry wait", after: 5 * time.Second, wantAttempts: 1, wantErr: context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clientset := clientsetWithServerVersion(testServerVersion, kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage))

				var attempts []time.Time

				clientset.PrependReactor("get", "daemonsets", func(clienttesting.Action) (bool, runtime.Object, error) {
					attempts = append(attempts, time.Now())
					return true, nil, errors.New("apiserver unavailable")
				})

				started := time.Now()

				var (
					ctx    context.Context
					cancel context.CancelFunc
				)
				if tt.cancelContext {
					ctx, cancel = context.WithCancel(t.Context())
					if tt.after == 0 {
						cancel()
					} else {
						timer := time.AfterFunc(tt.after, cancel)
						defer timer.Stop()
					}
				} else {
					ctx, cancel = context.WithTimeout(t.Context(), tt.after)
				}
				defer cancel()

				got, err := ManagedKubeProxyImage(ctx, clientset, "")
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ManagedKubeProxyImage() error = %v, want %v", err, tt.wantErr)
				}

				if got != "" {
					t.Fatalf("ManagedKubeProxyImage() = %q, want no image after cancellation", got)
				}

				assertKubeProxyImageDiscoveryAttempts(t, started, attempts, tt.wantAttempts)

				if elapsed := time.Since(started); elapsed != tt.after {
					t.Fatalf("discovery took %v, want %v", elapsed, tt.after)
				}

				if actions := clientset.Actions(); len(actions) != tt.wantAttempts {
					t.Fatalf("unexpected API calls after cancellation: %v", actions)
				}
			})
		})
	}
}

func assertKubeProxyImageDiscoveryAttempts(t *testing.T, started time.Time, attempts []time.Time, want int) {
	t.Helper()

	if len(attempts) != want {
		t.Fatalf("discovery made %d attempts, want %d", len(attempts), want)
	}

	for i, attemptedAt := range attempts {
		wantAt := started.Add(time.Duration(i) * 10 * time.Second)
		if !attemptedAt.Equal(wantAt) {
			t.Fatalf("attempt %d at %v, want %v", i+1, attemptedAt.Sub(started), wantAt.Sub(started))
		}
	}
}

// TestManagedKubeProxyImageFallsBackWhenTheClusterCannotBeRead keeps the
// behavior that predates repository values: exhausted retries degrade the
// choice of image rather than stopping managed kube-proxy.
func TestManagedKubeProxyImageFallsBackWhenTheClusterCannotBeRead(t *testing.T) {
	for _, tt := range []struct {
		configured string
		want       string
	}{
		{want: "registry.k8s.io/kube-proxy:latest"},
		{configured: "myreg.example/kube-proxy", want: "myreg.example/kube-proxy:latest"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clientset := k8sfake.NewClientset(kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage))
				unavailable := func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("apiserver unavailable")
				}

				var attempts []time.Time

				clientset.PrependReactor("get", "daemonsets", func(action clienttesting.Action) (bool, runtime.Object, error) {
					attempts = append(attempts, time.Now())
					return unavailable(action)
				})
				clientset.PrependReactor("get", "version", unavailable)

				started := time.Now()

				got, err := ManagedKubeProxyImage(t.Context(), clientset, tt.configured)
				if err != nil {
					t.Fatalf("ManagedKubeProxyImage() error = %v", err)
				}

				if got != tt.want {
					t.Fatalf("ManagedKubeProxyImage() = %q, want %q", got, tt.want)
				}

				assertKubeProxyImageDiscoveryAttempts(t, started, attempts, 11)

				if elapsed := time.Since(started); elapsed != 100*time.Second {
					t.Fatalf("discovery took %v, want %v", elapsed, 100*time.Second)
				}
			})
		})
	}
}
