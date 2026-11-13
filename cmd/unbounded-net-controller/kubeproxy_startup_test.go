// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/informers"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
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

		// Start unrelated informers while discovery is blocked. None of the
		// managed kube-proxy controller's informers have been requested yet.
		fixture.startInformers(t)

		if got := fixture.daemonSets(t); len(got) != 0 {
			t.Fatalf("created DaemonSets before image discovery completed: %v", got)
		}

		close(releaseDiscovery)

		var got []appsv1.DaemonSet

		err := wait.PollUntilContextTimeout(fixture.ctx, 10*time.Millisecond, time.Second, true, func(context.Context) (bool, error) {
			got = fixture.daemonSets(t)
			return len(got) == 1, nil
		})
		if err != nil || len(got) != 1 || got[0].Name != "unbounded-net-kube-proxy-test" {
			t.Fatalf("DaemonSets = %v, want the managed DaemonSet after delayed discovery", got)
		}

		if image := got[0].Spec.Template.Spec.Containers[0].Image; image != managedKubeProxyStartupImage {
			t.Fatalf("kube-proxy image = %q, want %q", image, managedKubeProxyStartupImage)
		}

		if image := got[0].Spec.Template.Spec.InitContainers[0].Image; image != managedKubeProxyStartupImage {
			t.Fatalf("bootstrap image = %q, want %q", image, managedKubeProxyStartupImage)
		}

		synctest.Wait()

		for resource, actions := range map[string][]clienttesting.Action{
			"pods":     fixture.clientset.Actions(),
			"machines": fixture.dynamicClient.Actions(),
		} {
			watches := 0

			for _, action := range actions {
				if action.GetVerb() == "watch" && action.GetResource().Resource == resource {
					watches++
				}
			}

			if watches != 1 {
				t.Fatalf("%s watches = %d, want the existing informer to stay running without restarting", resource, watches)
			}
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
	dynamicClient  *dynamicfake.FakeDynamicClient
	factory        informers.SharedInformerFactory
	dynamicFactory dynamicinformer.DynamicSharedInformerFactory
	pods           cache.SharedIndexInformer
	machines       cache.SharedIndexInformer
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
		ctx: ctx, cancel: cancel, clientset: clientset, dynamicClient: dynamicClient, factory: factory, dynamicFactory: dynamicFactory,
		// Start with unrelated informers, not the Node, Site, or DaemonSet
		// informers that the managed kube-proxy constructor must request itself.
		pods:     factory.Core().V1().Pods().Informer(),
		machines: dynamicFactory.ForResource(unboundedv1alpha3.GroupVersion.WithResource("machines")).Informer(),
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

	synctest.Wait()

	for name, informer := range map[string]cache.SharedIndexInformer{"pods": f.pods, "machines": f.machines} {
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
