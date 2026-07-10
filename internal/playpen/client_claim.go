// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package playpen

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

const (
	playpenClaimAnnotation     = "playpen.unbounded-cloud.io/claimed-by"
	playpenClaimedAtAnnotation = "playpen.unbounded-cloud.io/claimed-at"
	playpenClaimedPodEnv       = "PLAYPEN_CLAIMED_POD"
	playpenRemoteIPEnv         = "PLAYPEN_REMOTE_IP"
	playpenVMMACEnv            = "PLAYPEN_VM_MAC"
)

var errPlaypenPodUnavailable = errors.New("playpen pod is unavailable")

type playpenPodClaim struct {
	pods      typedcorev1.PodInterface
	namespace string
	name      string
	uid       types.UID
	claimID   string
	remoteIP  string
	mac       string
}

func claimPlaypenPodForClient(ctx context.Context, cfg ClientConfig) (*playpenPodClaim, error) {
	if strings.TrimSpace(cfg.RemoteIP) != "" {
		return nil, nil
	}

	client := cfg.KubeClient
	if client == nil {
		var err error

		client, err = playpenKubeClient(cfg.Kubeconfig, cfg.KubeContext)
		if err != nil {
			return nil, err
		}
	}

	endpointIP, _, err := net.ParseCIDR(cfg.EndpointCIDR)
	if err != nil {
		return nil, fmt.Errorf("endpoint-cidr must be an IP prefix before claiming a playpen pod: %q", cfg.EndpointCIDR)
	}

	namespace := strings.TrimSpace(cfg.PodNamespace)
	if namespace == "" {
		namespace = defaultPlaypenNamespace
	}

	selector := strings.TrimSpace(cfg.PodSelector)
	if selector == "" {
		selector = defaultPlaypenSelector
	}

	claim, err := claimPlaypenPod(ctx, client.CoreV1().Pods(namespace), selector, string(uuid.NewUUID()), endpointIP.To4() != nil)
	if err != nil {
		return nil, fmt.Errorf("claim playpen pod in namespace %s: %w", namespace, err)
	}

	return claim, nil
}

func playpenKubeClient(kubeconfig, kubeContext string) (kubernetes.Interface, error) {
	restConfig, err := playpenRESTConfig(kubeconfig, kubeContext)
	if err != nil {
		return nil, err
	}

	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}

	return client, nil
}

func playpenRESTConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = strings.TrimSpace(kubeconfig)

	overrides := &clientcmd.ConfigOverrides{CurrentContext: strings.TrimSpace(kubeContext)}

	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes client configuration: %w", err)
	}

	return restConfig, nil
}

func claimPlaypenPod(ctx context.Context, pods typedcorev1.PodInterface, selector, claimID string, ipv4 bool) (*playpenPodClaim, error) {
	list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })

	for i := range list.Items {
		candidate := &list.Items[i]
		if candidate.Annotations[playpenClaimAnnotation] != "" {
			continue
		}

		claim, err := claimPlaypenPodCandidate(ctx, pods, candidate, claimID, ipv4)
		if errors.Is(err, errPlaypenPodUnavailable) || apierrors.IsNotFound(err) {
			continue
		}

		if err != nil {
			return nil, err
		}

		return claim, nil
	}

	return nil, errors.New("no unclaimed ready playpen pods are available")
}

func claimPlaypenPodCandidate(ctx context.Context, pods typedcorev1.PodInterface, candidate *corev1.Pod, claimID string, ipv4 bool) (*playpenPodClaim, error) {
	var claim *playpenPodClaim

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		pod, err := pods.Get(ctx, candidate.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if candidate.UID != "" && pod.UID != candidate.UID {
			return errPlaypenPodUnavailable
		}

		if owner := pod.Annotations[playpenClaimAnnotation]; owner != "" && owner != claimID {
			return errPlaypenPodUnavailable
		}

		remoteIP, ok := availablePlaypenPodIP(pod, ipv4)
		if !ok {
			return errPlaypenPodUnavailable
		}

		if pod.Annotations[playpenClaimAnnotation] == claimID {
			claim = newPlaypenPodClaim(pods, pod, claimID, remoteIP)

			return nil
		}

		updated := pod.DeepCopy()
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}

		updated.Annotations[playpenClaimAnnotation] = claimID
		updated.Annotations[playpenClaimedAtAnnotation] = time.Now().UTC().Format(time.RFC3339)

		updated, err = pods.Update(ctx, updated, metav1.UpdateOptions{})
		if err != nil {
			return err
		}

		claim = newPlaypenPodClaim(pods, updated, claimID, remoteIP)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return claim, nil
}

func newPlaypenPodClaim(pods typedcorev1.PodInterface, pod *corev1.Pod, claimID, remoteIP string) *playpenPodClaim {
	identity := pod.Namespace + "/" + pod.Name

	return &playpenPodClaim{
		pods:      pods,
		namespace: pod.Namespace,
		name:      pod.Name,
		uid:       pod.UID,
		claimID:   claimID,
		remoteIP:  remoteIP,
		mac:       MACFromIdentity(identity).String(),
	}
}

func (c *playpenPodClaim) environment(environ []string) []string {
	result := make([]string, 0, len(environ)+3)
	for _, entry := range environ {
		if strings.HasPrefix(entry, playpenClaimedPodEnv+"=") ||
			strings.HasPrefix(entry, playpenRemoteIPEnv+"=") ||
			strings.HasPrefix(entry, playpenVMMACEnv+"=") {
			continue
		}

		result = append(result, entry)
	}

	return append(result,
		playpenClaimedPodEnv+"="+c.namespace+"/"+c.name,
		playpenRemoteIPEnv+"="+c.remoteIP,
		playpenVMMACEnv+"="+c.mac,
	)
}

func writePlaypenClaim(w io.Writer, claim *playpenPodClaim) error {
	_, err := fmt.Fprintf(w, "%s=%s/%s\n%s=%s\n%s=%s\n",
		playpenClaimedPodEnv, claim.namespace, claim.name,
		playpenRemoteIPEnv, claim.remoteIP,
		playpenVMMACEnv, claim.mac,
	)

	return err
}

func availablePlaypenPodIP(pod *corev1.Pod, ipv4 bool) (string, bool) {
	if !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodRunning || !podReady(pod) {
		return "", false
	}

	for _, podIP := range append(pod.Status.PodIPs, corev1.PodIP{IP: pod.Status.PodIP}) {
		ip := net.ParseIP(strings.TrimSpace(podIP.IP))
		if ip != nil && (ip.To4() != nil) == ipv4 {
			return ip.String(), true
		}
	}

	return "", false
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func (c *playpenPodClaim) release(ctx context.Context) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		pod, err := c.pods.Get(ctx, c.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}

		if err != nil {
			return err
		}

		if c.uid != "" && pod.UID != c.uid {
			return nil
		}

		if pod.Annotations[playpenClaimAnnotation] != c.claimID {
			return nil
		}

		updated := pod.DeepCopy()
		delete(updated.Annotations, playpenClaimAnnotation)
		delete(updated.Annotations, playpenClaimedAtAnnotation)
		_, err = c.pods.Update(ctx, updated, metav1.UpdateOptions{})

		return err
	})
}
