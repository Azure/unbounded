// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	ktesting "k8s.io/client-go/testing"
)

const testPlaypenNamespace = "playpen-test"

func TestClaimPlaypenPodSelectsReadyUnclaimedPod(t *testing.T) {
	claimed := readyPlaypenPod("claimed", "10.0.0.1")
	claimed.Annotations = map[string]string{playpenClaimAnnotation: "another-client"}

	notReady := readyPlaypenPod("not-ready", "10.0.0.2")
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse

	client := fake.NewSimpleClientset(
		claimed,
		notReady,
		readyPlaypenPod("selected", "10.0.0.3"),
		readyPlaypenPodWithLabels("other", "10.0.0.4", map[string]string{"app": "other"}),
	)

	claim, err := claimPlaypenPod(
		context.Background(),
		client.CoreV1().Pods(testPlaypenNamespace),
		"app=playpen",
		"this-client",
		true,
	)
	if err != nil {
		t.Fatalf("claimPlaypenPod() error = %v", err)
	}

	if claim.name != "selected" || claim.remoteIP != "10.0.0.3" || claim.mac != MACFromIdentity(testPlaypenNamespace+"/selected").String() {
		t.Fatalf("claim = %#v, want selected pod", claim)
	}

	pod, err := client.CoreV1().Pods(testPlaypenNamespace).Get(context.Background(), "selected", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get claimed pod: %v", err)
	}

	if got := pod.Annotations[playpenClaimAnnotation]; got != "this-client" {
		t.Fatalf("claim annotation = %q, want %q", got, "this-client")
	}

	if pod.Annotations[playpenClaimedAtAnnotation] == "" {
		t.Fatal("claimed-at annotation was not set")
	}
}

func TestPlaypenPodClaimOutputAndEnvironment(t *testing.T) {
	pod := readyPlaypenPod("playpen-2", "10.0.0.3")
	claim := newPlaypenPodClaim(nil, pod, "owner", pod.Status.PodIP)

	var output bytes.Buffer
	if err := writePlaypenClaim(&output, claim); err != nil {
		t.Fatalf("writePlaypenClaim() error = %v", err)
	}

	wantOutput := "PLAYPEN_CLAIMED_POD=playpen-test/playpen-2\n" +
		"PLAYPEN_REMOTE_IP=10.0.0.3\n" +
		"PLAYPEN_VM_MAC=" + MACFromIdentity("playpen-test/playpen-2").String() + "\n"
	if output.String() != wantOutput {
		t.Fatalf("claim output = %q, want %q", output.String(), wantOutput)
	}

	environ := claim.environment([]string{"KEEP=value", "PLAYPEN_VM_MAC=stale"})

	wantEnvironment := []string{
		"KEEP=value",
		"PLAYPEN_CLAIMED_POD=playpen-test/playpen-2",
		"PLAYPEN_REMOTE_IP=10.0.0.3",
		"PLAYPEN_VM_MAC=" + MACFromIdentity("playpen-test/playpen-2").String(),
	}
	if fmt.Sprint(environ) != fmt.Sprint(wantEnvironment) {
		t.Fatalf("claim environment = %v, want %v", environ, wantEnvironment)
	}
}

func TestClaimPlaypenPodSelectsMatchingIPFamily(t *testing.T) {
	pod := readyPlaypenPod("dual-stack", "10.0.0.3")
	pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.0.3"}, {IP: "2001:db8::3"}}

	client := fake.NewSimpleClientset(pod)

	claim, err := claimPlaypenPod(
		context.Background(),
		client.CoreV1().Pods(testPlaypenNamespace),
		"app=playpen",
		"ipv6-client",
		false,
	)
	if err != nil {
		t.Fatalf("claimPlaypenPod() error = %v", err)
	}

	if claim.remoteIP != "2001:db8::3" {
		t.Fatalf("remote IP = %q, want IPv6 pod IP", claim.remoteIP)
	}
}

func TestClaimPlaypenPodReturnsErrorWithoutAvailablePods(t *testing.T) {
	claimed := readyPlaypenPod("claimed", "10.0.0.1")
	claimed.Annotations = map[string]string{playpenClaimAnnotation: "another-client"}

	client := fake.NewSimpleClientset(claimed)

	_, err := claimPlaypenPod(
		context.Background(),
		client.CoreV1().Pods(testPlaypenNamespace),
		"app=playpen",
		"this-client",
		true,
	)
	if err == nil || err.Error() != "no unclaimed ready playpen pods are available" {
		t.Fatalf("claimPlaypenPod() error = %v, want no available pods", err)
	}
}

func TestRunClientReleasesClaimAfterValidationError(t *testing.T) {
	pod := readyPlaypenPodWithLabels(
		"pod",
		"10.0.0.3",
		map[string]string{"app.kubernetes.io/name": "playpen"},
	)
	client := fake.NewSimpleClientset(pod)

	cfg := DefaultClientConfig()
	cfg.EndpointCIDR = "172.30.1.2/30"
	cfg.GatewayIP = "172.30.1.1"
	cfg.PodNamespace = testPlaypenNamespace
	cfg.KubeClient = client

	err := RunClient(context.Background(), cfg)
	if err == nil || err.Error() != "a command to run in the client namespace is required" {
		t.Fatalf("RunClient() error = %v, want missing command", err)
	}

	updated, err := client.CoreV1().Pods(testPlaypenNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}

	if _, ok := updated.Annotations[playpenClaimAnnotation]; ok {
		t.Fatal("claim annotation was not released")
	}
}

func TestClaimPlaypenPodConcurrentClientsAreExclusive(t *testing.T) {
	pod := readyPlaypenPod("only-pod", "10.0.0.3")
	client := fake.NewSimpleClientset(pod)
	installOptimisticPodUpdates(t, client)

	type result struct {
		claim *playpenPodClaim
		err   error
	}

	start := make(chan struct{})
	reads := sync.WaitGroup{}
	reads.Add(2)

	allRead := make(chan struct{})

	go func() {
		reads.Wait()
		close(allRead)
	}()

	results := make(chan result, 2)

	for _, claimID := range []string{"client-a", "client-b"} {
		go func() {
			<-start

			pods := &barrierPodClient{
				PodInterface: client.CoreV1().Pods(testPlaypenNamespace),
				reads:        &reads,
				allRead:      allRead,
			}

			claim, err := claimPlaypenPodCandidate(
				context.Background(),
				pods,
				pod,
				claimID,
				true,
			)
			results <- result{claim: claim, err: err}
		}()
	}

	close(start)

	successes := 0
	failures := 0

	for range 2 {
		result := <-results
		if result.err == nil {
			successes++

			continue
		}

		if !errors.Is(result.err, errPlaypenPodUnavailable) {
			t.Fatalf("unexpected claim error: %v", result.err)
		}

		failures++
	}

	if successes != 1 || failures != 1 {
		t.Fatalf("successful claims = %d, failed claims = %d; want 1 and 1", successes, failures)
	}
}

type barrierPodClient struct {
	typedcorev1.PodInterface
	reads   *sync.WaitGroup
	allRead <-chan struct{}
	once    sync.Once
}

func (c *barrierPodClient) Get(ctx context.Context, name string, options metav1.GetOptions) (*corev1.Pod, error) {
	pod, err := c.PodInterface.Get(ctx, name, options)
	if err != nil {
		return nil, err
	}

	c.once.Do(func() {
		c.reads.Done()
		<-c.allRead
	})

	return pod, nil
}

func TestPlaypenPodClaimReleasePreservesAnotherOwner(t *testing.T) {
	pod := readyPlaypenPod("pod", "10.0.0.3")
	pod.Annotations = map[string]string{
		playpenClaimAnnotation:     "new-owner",
		playpenClaimedAtAnnotation: "timestamp",
		"example.com/keep":         "value",
	}

	client := fake.NewSimpleClientset(pod)

	claim := newPlaypenPodClaim(client.CoreV1().Pods(testPlaypenNamespace), pod, "old-owner", pod.Status.PodIP)
	if err := claim.release(context.Background()); err != nil {
		t.Fatalf("release() error = %v", err)
	}

	updated, err := client.CoreV1().Pods(testPlaypenNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}

	if got := updated.Annotations[playpenClaimAnnotation]; got != "new-owner" {
		t.Fatalf("claim annotation = %q, want new owner preserved", got)
	}

	if got := updated.Annotations["example.com/keep"]; got != "value" {
		t.Fatalf("unrelated annotation = %q, want preserved", got)
	}
}

func TestPlaypenPodClaimReleaseRemovesOwnedAnnotations(t *testing.T) {
	pod := readyPlaypenPod("pod", "10.0.0.3")
	pod.Annotations = map[string]string{
		playpenClaimAnnotation:     "owner",
		playpenClaimedAtAnnotation: "timestamp",
		"example.com/keep":         "value",
	}

	client := fake.NewSimpleClientset(pod)

	claim := newPlaypenPodClaim(client.CoreV1().Pods(testPlaypenNamespace), pod, "owner", pod.Status.PodIP)
	if err := claim.release(context.Background()); err != nil {
		t.Fatalf("release() error = %v", err)
	}

	updated, err := client.CoreV1().Pods(testPlaypenNamespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}

	if _, ok := updated.Annotations[playpenClaimAnnotation]; ok {
		t.Fatal("claim annotation was not removed")
	}

	if _, ok := updated.Annotations[playpenClaimedAtAnnotation]; ok {
		t.Fatal("claimed-at annotation was not removed")
	}

	if got := updated.Annotations["example.com/keep"]; got != "value" {
		t.Fatalf("unrelated annotation = %q, want preserved", got)
	}
}

func readyPlaypenPod(name, ip string) *corev1.Pod {
	return readyPlaypenPodWithLabels(name, ip, map[string]string{"app": "playpen"})
}

func readyPlaypenPodWithLabels(name, ip string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       testPlaypenNamespace,
			UID:             types.UID(name + "-uid"),
			ResourceVersion: "1",
			Labels:          labels,
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func installOptimisticPodUpdates(t *testing.T, client *fake.Clientset) {
	t.Helper()

	var mu sync.Mutex

	client.PrependReactor("update", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()

		update := action.(ktesting.UpdateAction).GetObject().(*corev1.Pod)
		resource := corev1.SchemeGroupVersion.WithResource("pods")

		currentObject, err := client.Tracker().Get(resource, update.Namespace, update.Name)
		if err != nil {
			return true, nil, err
		}

		current := currentObject.(*corev1.Pod)
		if update.ResourceVersion != current.ResourceVersion {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Resource: "pods"},
				update.Name,
				fmt.Errorf("resource version %s is stale", update.ResourceVersion),
			)
		}

		updated := update.DeepCopy()

		updated.ResourceVersion = fmt.Sprintf("%d", getResourceVersion(t, current.ResourceVersion)+1)
		if err := client.Tracker().Update(resource, updated, update.Namespace); err != nil {
			return true, nil, err
		}

		return true, updated, nil
	})
}

func getResourceVersion(t *testing.T, value string) int {
	t.Helper()

	var version int
	if _, err := fmt.Sscanf(value, "%d", &version); err != nil {
		t.Fatalf("parse resource version %q: %v", value, err)
	}

	return version
}
