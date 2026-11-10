// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	restfake "k8s.io/client-go/rest/fake"
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

func exactKubeProxyTagForTest(_ context.Context, _, serverTag string) (string, error) {
	return serverTag, nil
}

func TestManagedKubeProxyImage(t *testing.T) {
	tests := []struct {
		name       string
		configured string
		provider   *appsv1.DaemonSet
		want       string
	}{
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
		{
			name:     "an unversioned provider supplies the fallback repository",
			provider: kubeSystemKubeProxy("kube-proxy", "provider.example/kube-proxy"),
			want:     "provider.example/kube-proxy:" + testServerVersion,
		},
		{
			name:       "an override still wins over an unversioned provider repository",
			configured: "myreg.example/kube-proxy",
			provider:   kubeSystemKubeProxy("kube-proxy", "provider.example/kube-proxy"),
			want:       "myreg.example/kube-proxy:" + testServerVersion,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []runtime.Object
			if tt.provider != nil {
				objects = append(objects, tt.provider)
			}

			got, err := managedKubeProxyImage(t.Context(), clientsetWithServerVersion(testServerVersion, objects...), tt.configured, exactKubeProxyTagForTest)
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
	for _, configured := range []string{
		"myreg.example/kube-proxy:v1.34.0",
		"myreg.example/kube-proxy:latest",
		"myreg.example/kube-proxy@" + testImageDigest,
		"myreg.example/kube-proxy:v1.34.0@" + testImageDigest,
	} {
		t.Run(configured, func(t *testing.T) {
			clientset := clientsetWithServerVersion(testServerVersion, kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage))

			got, err := ManagedKubeProxyImage(t.Context(), clientset, configured)
			if err != nil {
				t.Fatalf("ManagedKubeProxyImage() error = %v", err)
			}

			if got != configured {
				t.Fatalf("ManagedKubeProxyImage() = %q, want unchanged %q", got, configured)
			}

			if actions := clientset.Actions(); len(actions) != 0 {
				t.Fatalf("a pinned image read the cluster: %v", actions)
			}
		})
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
		wantElapsed  time.Duration
	}{
		{name: "success on the initial read", wantAttempts: 1},
		{name: "NotFound on the initial read", finalErr: notFound, wantAttempts: 1, wantFallback: true},
		{name: "timeout is retried", failures: 1, readErr: timeout, wantAttempts: 2, wantElapsed: 10 * time.Second},
		{name: "internal error is retried", failures: 1, readErr: internalError, wantAttempts: 2, wantElapsed: 10 * time.Second},
		{name: "other errors are retried", failures: 1, readErr: errors.New("connection reset"), wantAttempts: 2, wantElapsed: 10 * time.Second},
		{name: "retries continue at the maximum delay", failures: 8, readErr: internalError, wantAttempts: 9, wantElapsed: 20*time.Minute + 10*time.Second},
		{name: "the final retry succeeds", failures: 147, readErr: internalError, wantAttempts: 148, wantElapsed: 11*time.Hour + 55*time.Minute + 10*time.Second},
		{name: "NotFound stops retries", failures: 1, readErr: timeout, finalErr: notFound, wantAttempts: 2, wantFallback: true, wantElapsed: 10 * time.Second},
		{name: "NotFound near the deadline stops retries", failures: 147, readErr: timeout, finalErr: notFound, wantAttempts: 148, wantFallback: true, wantElapsed: 11*time.Hour + 55*time.Minute + 10*time.Second},
		{name: "exhausted retries fall back", failures: 148, readErr: internalError, wantAttempts: 148, wantFallback: true, wantElapsed: 12 * time.Hour},
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

						got, err := managedKubeProxyImage(t.Context(), clientset, configured.image, exactKubeProxyTagForTest)
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

						if elapsed := time.Since(started); elapsed != tt.wantElapsed {
							t.Fatalf("discovery took %v, want %v", elapsed, tt.wantElapsed)
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
		{name: "canceled during a capped retry wait", after: 20 * time.Minute, cancelContext: true, wantAttempts: 8, wantErr: context.Canceled},
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

	wantAt := started
	delay := 10 * time.Second

	for i, attemptedAt := range attempts {
		if !attemptedAt.Equal(wantAt) {
			t.Fatalf("attempt %d at %v, want %v", i+1, attemptedAt.Sub(started), wantAt.Sub(started))
		}

		wantAt = wantAt.Add(delay)
		delay = min(delay*2, 5*time.Minute)
	}
}

func TestProviderKubeProxyImageRetryDeadlineLimitsInFlightRequests(t *testing.T) {
	for _, tt := range []struct {
		name          string
		parentTimeout time.Duration
		wantElapsed   time.Duration
		wantErr       error
	}{
		{name: "twelve-hour retry budget", wantElapsed: 12 * time.Hour},
		{name: "earlier parent deadline", parentTimeout: 2 * time.Hour, wantElapsed: 2 * time.Hour, wantErr: context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				requests := 0

				clientset, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "https://kubernetes.example"}, restfake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					requests++

					if req.URL.Path != "/apis/apps/v1/namespaces/kube-system/daemonsets/kube-proxy" {
						t.Errorf("unexpected provider discovery request: %s", req.URL)
					}

					if deadline, ok := req.Context().Deadline(); !ok || !deadline.Equal(started.Add(tt.wantElapsed)) {
						t.Errorf("request deadline = %v, present = %t, want %v", deadline, ok, started.Add(tt.wantElapsed))
					}

					<-req.Context().Done()

					return nil, req.Context().Err()
				}))
				if err != nil {
					t.Fatal(err)
				}

				ctx := t.Context()

				if tt.parentTimeout != 0 {
					var cancel context.CancelFunc

					ctx, cancel = context.WithTimeout(ctx, tt.parentTimeout)
					defer cancel()
				}

				got, err := providerKubeProxyImage(ctx, clientset)
				if got != "" || !errors.Is(err, tt.wantErr) {
					t.Fatalf("providerKubeProxyImage() = %q, %v, want no image and %v", got, err, tt.wantErr)
				}

				if requests != 1 || time.Since(started) != tt.wantElapsed {
					t.Fatalf("requests = %d, elapsed = %v, want 1 request bounded by %v", requests, time.Since(started), tt.wantElapsed)
				}
			})
		})
	}
}

// TestManagedKubeProxyImageUnavailableServerVersionReturnsError verifies that
// exhausted provider reads reach version discovery, but an unavailable version
// endpoint returns an error instead of the former latest fallback.
func TestManagedKubeProxyImageUnavailableServerVersionReturnsError(t *testing.T) {
	for _, tt := range []struct {
		name       string
		configured string
	}{
		{name: "default repository"},
		{name: "configured repository", configured: "myreg.example/kube-proxy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clientset := k8sfake.NewClientset(kubeSystemKubeProxy("kube-proxy", testProviderKubeProxyImage))
				unavailableErr := errors.New("apiserver unavailable")
				unavailable := func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, unavailableErr
				}

				var attempts []time.Time

				clientset.PrependReactor("get", "daemonsets", func(action clienttesting.Action) (bool, runtime.Object, error) {
					attempts = append(attempts, time.Now())
					return unavailable(action)
				})
				clientset.PrependReactor("get", "version", unavailable)

				started := time.Now()

				got, err := ManagedKubeProxyImage(t.Context(), clientset, tt.configured)
				if !errors.Is(err, unavailableErr) {
					t.Fatalf("ManagedKubeProxyImage() error = %v, want %v", err, unavailableErr)
				}

				if got != "" {
					t.Fatalf("ManagedKubeProxyImage() = %q, want no image", got)
				}

				assertKubeProxyImageDiscoveryAttempts(t, started, attempts, 148)

				if elapsed := time.Since(started); elapsed != 12*time.Hour {
					t.Fatalf("discovery took %v, want %v", elapsed, 12*time.Hour)
				}
			})
		})
	}
}

func TestManagedKubeProxyImageProviderVersionSkipsRegistry(t *testing.T) {
	for _, tt := range []struct {
		name       string
		configured string
		provider   string
		want       string
	}{
		{name: "provider tag", provider: testProviderKubeProxyImage, want: testProviderKubeProxyImage},
		{name: "provider tag and digest", provider: testProviderKubeProxyImage + "@" + testImageDigest, want: testProviderKubeProxyImage + "@" + testImageDigest},
		{name: "provider digest", provider: "provider.example/kube-proxy@" + testImageDigest, want: "provider.example/kube-proxy@" + testImageDigest},
		{name: "provider latest", provider: "provider.example/kube-proxy:latest", want: "provider.example/kube-proxy:latest"},
		{name: "mirror keeps the provider tag", configured: "myreg.example/kube-proxy", provider: testProviderKubeProxyImage + "@" + testImageDigest, want: "myreg.example/kube-proxy:v1.34.2-1"},
		{name: "mirror keeps a supplied latest tag", configured: "myreg.example/kube-proxy", provider: "provider.example/kube-proxy:latest", want: "myreg.example/kube-proxy:latest"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clientset := clientsetWithServerVersion("", kubeSystemKubeProxy("kube-proxy", tt.provider))

			got, err := managedKubeProxyImage(t.Context(), clientset, tt.configured, func(context.Context, string, string) (string, error) {
				t.Fatal("a provider version must not trigger a registry lookup")
				return "", nil
			})
			if err != nil {
				t.Fatalf("ManagedKubeProxyImage() error = %v", err)
			}

			if got != tt.want {
				t.Fatalf("ManagedKubeProxyImage() = %q, want %q", got, tt.want)
			}

			if actions := clientset.Actions(); len(actions) != 1 || actions[0].GetResource().Resource != "daemonsets" {
				t.Fatalf("provider image resolution read unexpected resources: %v", actions)
			}
		})
	}
}

func TestManagedKubeProxyImageRegistryFallback(t *testing.T) {
	for _, tt := range []struct {
		name       string
		configured string
		provider   string
		repository string
	}{
		{name: "default upstream repository", repository: DefaultKubeProxyRepository},
		{name: "configured mirror repository", configured: "myreg.example/team/kube-proxy", repository: "myreg.example/team/kube-proxy"},
		{name: "unversioned provider repository", provider: "provider.example/kube-proxy", repository: "provider.example/kube-proxy"},
		{name: "mirror replaces an unversioned provider repository", configured: "myreg.example/kube-proxy", provider: "provider.example/kube-proxy", repository: "myreg.example/kube-proxy"},
		{name: "mirror cannot inherit a provider digest as a tag", configured: "myreg.example/kube-proxy", provider: "provider.example/kube-proxy@" + testImageDigest, repository: "myreg.example/kube-proxy"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var objects []runtime.Object
			if tt.provider != "" {
				objects = append(objects, kubeSystemKubeProxy("kube-proxy", tt.provider))
			}

			clientset := clientsetWithServerVersion("v1.36.2-aks.1+build.7", objects...)
			lookups := 0

			got, err := managedKubeProxyImage(t.Context(), clientset, tt.configured, func(_ context.Context, repository, serverTag string) (string, error) {
				lookups++

				if repository != tt.repository || serverTag != "v1.36.2" {
					t.Fatalf("registry lookup = %q, %q, want %q, v1.36.2", repository, serverTag, tt.repository)
				}

				return "v1.36.2-2", nil
			})
			if err != nil {
				t.Fatalf("ManagedKubeProxyImage() error = %v", err)
			}

			if want := tt.repository + ":v1.36.2-2"; got != want {
				t.Fatalf("ManagedKubeProxyImage() = %q, want %q", got, want)
			}

			if lookups != 1 {
				t.Fatalf("registry lookups = %d, want 1", lookups)
			}
		})
	}
}

func TestManagedKubeProxyImageRegistryFailureReturnsNoImage(t *testing.T) {
	lookupErr := errors.New("registry unavailable")
	clientset := clientsetWithServerVersion("v1.36.2")

	got, err := managedKubeProxyImage(t.Context(), clientset, "myreg.example/kube-proxy", func(context.Context, string, string) (string, error) {
		return "", lookupErr
	})
	if !errors.Is(err, lookupErr) || got != "" {
		t.Fatalf("ManagedKubeProxyImage() = %q, %v, want no image and %v", got, err, lookupErr)
	}
}

func TestServerVersionTag(t *testing.T) {
	for _, gitVersion := range []string{"v1.36.2", "1.36.2", "v1.36.2-aks.1", "v1.36.2+build.7", "v1.36.2-aks.1+build.7"} {
		t.Run(gitVersion, func(t *testing.T) {
			got, err := serverVersionTag(t.Context(), clientsetWithServerVersion(gitVersion))
			if err != nil || got != "v1.36.2" {
				t.Fatalf("serverVersionTag() = %q, %v, want v1.36.2", got, err)
			}
		})
	}
}

func TestServerVersionTagRejectsMissingOrInvalidVersions(t *testing.T) {
	for _, gitVersion := range []string{"", "latest", "v1.36", "v1.036.2", "not-a-version"} {
		t.Run(gitVersion, func(t *testing.T) {
			clientset := clientsetWithServerVersion(gitVersion)

			got, err := managedKubeProxyImage(t.Context(), clientset, "", func(context.Context, string, string) (string, error) {
				t.Fatal("an invalid API server version must not trigger a registry lookup")
				return "", nil
			})
			if err == nil || got != "" {
				t.Fatalf("ManagedKubeProxyImage() = %q, %v, want no image and an error", got, err)
			}

			if !strings.Contains(err.Error(), "server version") {
				t.Fatalf("error = %v, want a Kubernetes server version error", err)
			}
		})
	}
}
