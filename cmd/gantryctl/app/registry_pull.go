// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/Azure/unbounded/internal/gantry/noderoute"
)

type registryTestOptions struct {
	root              *rootOptions
	workloadNamespace string
	serviceAccount    string
	imagePullSecrets  []string
	keepPod           bool
}

func newRegistryTestCommand(root *rootOptions) *cobra.Command {
	options := &registryTestOptions{root: root, workloadNamespace: "default"}
	command := &cobra.Command{
		Use:   "test IMAGE",
		Short: "Verify a kubelet image pull traverses Gantry",
		Example: `  gantryctl registry test myacr.azurecr.io/team/image:v1
  gantryctl registry test registry.example.com/team/image:v1 --workload-namespace team-a --image-pull-secret any-secret-name
  gantryctl registry test registry.example.com/team/image:v1 --workload-namespace team-a --service-account build-runner`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return options.run(command.Context(), command.OutOrStdout(), args[0])
		},
	}
	command.Flags().StringVar(&options.workloadNamespace, "workload-namespace", options.workloadNamespace, "Namespace for the temporary pull Pod")
	command.Flags().StringVar(&options.serviceAccount, "service-account", "", "ServiceAccount used by the temporary pull Pod")
	command.Flags().StringSliceVar(&options.imagePullSecrets, "image-pull-secret", nil, "Image pull Secret name; may be repeated")
	command.Flags().BoolVar(&options.keepPod, "keep-pod", false, "Keep the temporary pull Pod after the test")

	return command
}

func (o *registryTestOptions) run(ctx context.Context, output io.Writer, image string) error {
	host, err := registryFromImage(image)
	if err != nil {
		return err
	}

	clients, err := o.root.clusterClients()
	if err != nil {
		return err
	}

	store, err := loadRegistryStore(ctx, clients.kube, o.root.namespace)
	if err != nil {
		return err
	}

	auth, configured := store.auth[host]
	if !configured {
		return fmt.Errorf("registry %s is not configured; run gantryctl registry add %s first", host, host)
	}

	agent, err := selectReadyAgent(ctx, clients, o.root.namespace)
	if err != nil {
		return err
	}

	before, err := mirrorBytesServed(ctx, clients, o.root.namespace, agent.Name)
	if err != nil {
		return fmt.Errorf("read Gantry metrics before test: %w", err)
	}

	pullSecrets := make([]corev1.LocalObjectReference, 0, len(o.imagePullSecrets))
	for _, name := range o.imagePullSecrets {
		if strings.TrimSpace(name) == "" {
			return errors.New("--image-pull-secret must not be empty")
		}

		pullSecrets = append(pullSecrets, corev1.LocalObjectReference{Name: name})
	}

	pod, err := clients.kube.CoreV1().Pods(o.workloadNamespace).Create(ctx, pullTestPod(
		o.workloadNamespace,
		agent.Spec.NodeName,
		image,
		o.serviceAccount,
		pullSecrets,
	), metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create pull-test Pod: %w", err)
	}

	if !o.keepPod {
		defer func() {
			grace := int64(0)
			_ = clients.kube.CoreV1().Pods(o.workloadNamespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{GracePeriodSeconds: &grace}) //nolint:errcheck // test pod cleanup is best effort
		}()
	}

	if err := waitForImagePull(ctx, clients, o.workloadNamespace, pod.Name, o.root.timeout); err != nil {
		return err
	}

	after, err := waitForMirrorBytes(ctx, clients, o.root.namespace, agent.Name, before, 30*time.Second)
	if err != nil {
		return err
	}

	return writeOutputf(output,
		"Registry configured      PASS (%s)\nAuthentication           PASS (%s)\nKubelet/containerd pull  PASS (%s)\nRequest traversed Gantry PASS (served bytes %.0f -> %.0f on node %s)\n",
		host, auth.Mode, image, before, after, agent.Spec.NodeName,
	)
}

func pullTestPod(namespace, nodeName, image, serviceAccount string, pullSecrets []corev1.LocalObjectReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "gantryctl-pull-",
			Namespace:    namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "gantryctl-pull-test",
				"app.kubernetes.io/managed-by": fieldManager,
			},
		},
		Spec: corev1.PodSpec{
			NodeName:           nodeName,
			RestartPolicy:      corev1.RestartPolicyNever,
			ServiceAccountName: serviceAccount,
			ImagePullSecrets:   pullSecrets,
			Tolerations: []corev1.Toleration{{
				Operator: corev1.TolerationOpExists,
			}},
			Containers: []corev1.Container{{
				Name:            "pull",
				Image:           image,
				ImagePullPolicy: corev1.PullAlways,
			}},
		},
	}
}

func registryFromImage(image string) (string, error) {
	image = strings.TrimSpace(image)

	slash := strings.IndexByte(image, '/')
	if slash <= 0 {
		return "", fmt.Errorf("image %q must include an explicit registry host", image)
	}

	prefix := image[:slash]
	if prefix != "localhost" && !strings.ContainsAny(prefix, ".:") {
		return "", fmt.Errorf("image %q must include an explicit registry host", image)
	}

	return noderoute.NormalizeRegistryHost(prefix)
}

func selectReadyAgent(ctx context.Context, clients *clusterClients, namespace string) (*corev1.Pod, error) {
	pods, err := clients.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=gantry,app.kubernetes.io/instance=standalone",
	})
	if err != nil {
		return nil, fmt.Errorf("list standalone Gantry agents: %w", err)
	}

	ready := make([]*corev1.Pod, 0, len(pods.Items))
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Spec.NodeName != "" && podReady(pod) {
			ready = append(ready, pod)
		}
	}

	if len(ready) == 0 {
		return nil, errors.New("no Ready standalone Gantry agent found")
	}

	sort.Slice(ready, func(i, j int) bool {
		return ready[i].Spec.NodeName < ready[j].Spec.NodeName
	})

	return ready[0], nil
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func waitForImagePull(ctx context.Context, clients *clusterClients, namespace, name string, timeout time.Duration) error {
	var lastReason string

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		pod, err := clients.kube.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != "pull" {
				continue
			}

			if status.ImageID != "" {
				return true, nil
			}

			if status.State.Waiting != nil {
				lastReason = status.State.Waiting.Reason + ": " + status.State.Waiting.Message
				if status.State.Waiting.Reason == "ImagePullBackOff" {
					return false, fmt.Errorf("image pull failed: %s", lastReason)
				}
			}
		}

		return false, nil
	})
	if err != nil {
		if lastReason != "" {
			return fmt.Errorf("wait for image pull (%s): %w", lastReason, err)
		}

		return fmt.Errorf("wait for image pull: %w", err)
	}

	return nil
}

func waitForMirrorBytes(ctx context.Context, clients *clusterClients, namespace, pod string, before float64, timeout time.Duration) (float64, error) {
	var current float64

	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		value, err := mirrorBytesServed(ctx, clients, namespace, pod)
		if err != nil {
			return false, err
		}

		current = value

		return current > before, nil
	})
	if err != nil {
		return current, fmt.Errorf("image pulled but Gantry did not complete a response; retry with an uncached image: %w", err)
	}

	return current, nil
}

func mirrorBytesServed(ctx context.Context, clients *clusterClients, namespace, pod string) (float64, error) {
	restClient := clients.kube.CoreV1().RESTClient()
	if restClient == nil {
		return 0, errors.New("kubernetes REST client does not support pod proxy requests")
	}

	body, err := restClient.Get().
		Namespace(namespace).
		Resource("pods").
		Name(pod + ":9095").
		SubResource("proxy").
		Suffix("metrics").
		DoRaw(ctx)
	if err != nil {
		return 0, err
	}

	return metricSum(body, "gantry_mirror_bytes_served_total"), nil
}

func metricSum(metrics []byte, name string) float64 {
	var total float64

	for _, rawLine := range strings.Split(string(metrics), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if !strings.HasPrefix(line, name+" ") && !strings.HasPrefix(line, name+"{") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err == nil {
			total += value
		}
	}

	return total
}
