// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

const managedKubeProxyStartupImage = "registry.example/kube-proxy:v1.36.2-2"

func TestStartManagedKubeProxyControllerDoesNotBlockStartup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newManagedKubeProxyStartupFixture(t)
		defer fixture.stop()

		discoveryStarted := make(chan struct{})
		releaseDiscovery := make(chan struct{})

		fixture.start(t, func(ctx context.Context) (string, error) {
			close(discoveryStarted)

			select {
			case <-releaseDiscovery:
				return managedKubeProxyStartupImage, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})

		select {
		case <-discoveryStarted:
		default:
			t.Fatal("image discovery did not start in the background")
		}

		// Start only once, while discovery is blocked. This also catches a
		// DaemonSet informer that was not requested before factory startup.
		fixture.startInformers(t)

		if got := fixture.daemonSets(t); len(got) != 0 {
			t.Fatalf("created DaemonSets before image discovery completed: %v", got)
		}

		close(releaseDiscovery)
		synctest.Wait()

		got := fixture.daemonSets(t)
		if len(got) != 1 || got[0].Name != "unbounded-net-kube-proxy-test" {
			t.Fatalf("DaemonSets = %v, want the managed DaemonSet after delayed discovery", got)
		}

		if image := got[0].Spec.Template.Spec.Containers[0].Image; image != managedKubeProxyStartupImage {
			t.Fatalf("kube-proxy image = %q, want %q", image, managedKubeProxyStartupImage)
		}

		if image := got[0].Spec.Template.Spec.InitContainers[0].Image; image != managedKubeProxyStartupImage {
			t.Fatalf("bootstrap image = %q, want %q", image, managedKubeProxyStartupImage)
		}
	})
}

func TestStartManagedKubeProxyControllerAbortsAfterDiscovery(t *testing.T) {
	for _, tt := range []struct {
		name         string
		discoveryErr error
		cancelLeader bool
	}{
		{name: "discovery failure", discoveryErr: errors.New("registry unavailable")},
		{name: "leadership canceled", cancelLeader: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newManagedKubeProxyStartupFixture(t)
				defer fixture.stop()

				releaseDiscovery := make(chan struct{})

				fixture.start(t, func(ctx context.Context) (string, error) {
					select {
					case <-releaseDiscovery:
					case <-ctx.Done():
					}
					// Return an image even after cancellation or alongside an
					// error, so startup must honor the context and error first.
					return managedKubeProxyStartupImage, tt.discoveryErr
				})
				fixture.startInformers(t)

				if tt.cancelLeader {
					fixture.cancel()
				} else {
					close(releaseDiscovery)
				}

				synctest.Wait()

				if got := fixture.daemonSets(t); len(got) != 0 {
					t.Fatalf("created DaemonSets after failed or canceled discovery: %v", got)
				}
			})
		})
	}
}

func TestStartManagedKubeProxyControllerSkipsCanceledDiscovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newManagedKubeProxyStartupFixture(t)
		defer fixture.stop()

		fixture.cancel()
		fixture.start(t, func(context.Context) (string, error) {
			t.Error("image discovery ran after leadership was canceled")
			return managedKubeProxyStartupImage, nil
		})

		if got := fixture.daemonSets(t); len(got) != 0 {
			t.Fatalf("created DaemonSets with a canceled leadership context: %v", got)
		}
	})
}

type managedKubeProxyStartupFixture struct {
	ctx            context.Context
	cancel         context.CancelFunc
	clientset      *k8sfake.Clientset
	factory        informers.SharedInformerFactory
	dynamicFactory dynamicinformer.DynamicSharedInformerFactory
	nodes          cache.SharedIndexInformer
	sites          cache.SharedIndexInformer
}

func newManagedKubeProxyStartupFixture(t *testing.T) *managedKubeProxyStartupFixture {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := unboundedv1alpha3.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	clientset := k8sfake.NewClientset()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: unboundedv1alpha3.SiteSpec{PodCidrAssignments: []unboundednetv1alpha1.PodCidrAssignment{{
			AssignmentEnabled: new(true),
			CidrBlocks:        []string{"10.244.0.0/16"},
		}}},
	})
	factory := informers.NewSharedInformerFactory(clientset, 0)
	dynamicFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
	ctx, cancel := context.WithCancel(t.Context())

	return &managedKubeProxyStartupFixture{
		ctx: ctx, cancel: cancel, clientset: clientset, factory: factory, dynamicFactory: dynamicFactory,
		// Match the informers already requested by the Site controller, but do
		// not request DaemonSets here: the startup helper must do that itself.
		nodes: factory.Core().V1().Nodes().Informer(),
		sites: dynamicFactory.ForResource(unboundedv1alpha3.GroupVersion.WithResource("sites")).Informer(),
	}
}

func (f *managedKubeProxyStartupFixture) start(t *testing.T, resolveImage func(context.Context) (string, error)) {
	t.Helper()

	returned := make(chan struct{})

	go func() {
		startManagedKubeProxyController(f.ctx, f.clientset, f.dynamicFactory, f.factory, "unbounded-net", resolveImage)
		close(returned)
	}()

	synctest.Wait()

	select {
	case <-returned:
	default:
		t.Fatal("managed kube-proxy startup blocked on image discovery")
	}
}

func (f *managedKubeProxyStartupFixture) startInformers(t *testing.T) {
	t.Helper()
	f.factory.Start(f.ctx.Done())
	f.dynamicFactory.Start(f.ctx.Done())
	daemonSets := f.factory.Apps().V1().DaemonSets().Informer()

	synctest.Wait()

	for name, informer := range map[string]cache.SharedIndexInformer{"nodes": f.nodes, "sites": f.sites, "daemonsets": daemonSets} {
		if !informer.HasSynced() {
			t.Fatalf("%s informer did not sync while image discovery was blocked", name)
		}
	}
}

func (f *managedKubeProxyStartupFixture) daemonSets(t *testing.T) []appsv1.DaemonSet {
	t.Helper()

	list, err := f.clientset.AppsV1().DaemonSets("unbounded-net").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	return list.Items
}

func (f *managedKubeProxyStartupFixture) stop() {
	f.cancel()
	f.factory.Shutdown()
	f.dynamicFactory.Shutdown()
}
