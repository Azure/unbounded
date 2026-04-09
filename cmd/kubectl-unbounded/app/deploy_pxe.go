package app

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	acappsv1 "k8s.io/client-go/applyconfigurations/apps/v1"
	accorev1 "k8s.io/client-go/applyconfigurations/core/v1"
	acmetav1 "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/project-unbounded/unbounded-kube/internal/kube"
)

// MetalmanImage is the default container image for the metalman controller
// deployment. It is set at build time via -ldflags:
//
//	-X github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/app.MetalmanImage=<image>
//
// When not set (e.g. during development), it falls back to "metalman:latest".
var MetalmanImage = "metalman:latest"

const (
	deployPXENamespace = "unbounded-kube"
)

// deployPXEParams holds the values needed to build the PXE Deployment.
type deployPXEParams struct {
	Site  string
	Image string
}

// buildPXEDeployment constructs the Deployment apply configuration for the
// unbounded-pxe server scoped to the given site.
func buildPXEDeployment(p deployPXEParams) *acappsv1.DeploymentApplyConfiguration {
	name := "metalman-controller-" + p.Site
	labels := map[string]string{
		"app":                    "unbounded-pxe",
		"unbounded-kube.io/site": p.Site,
	}

	return acappsv1.Deployment(name, deployPXENamespace).
		WithLabels(labels).
		WithSpec(acappsv1.DeploymentSpec().
			WithReplicas(1).
			WithSelector(acmetav1.LabelSelector().
				WithMatchLabels(labels),
			).
			WithTemplate(accorev1.PodTemplateSpec().
				WithLabels(labels).
				WithSpec(accorev1.PodSpec().
					WithServiceAccountName("metalman-controller").
					WithHostNetwork(true).
					WithDNSPolicy(corev1.DNSClusterFirstWithHostNet).
					WithNodeSelector(map[string]string{
						"unbounded-kube.io/site": p.Site,
					}).
					WithTolerations(accorev1.Toleration().
						WithKey("CriticalAddonsOnly").
						WithOperator(corev1.TolerationOpEqual).
						WithValue("true").
						WithEffect(corev1.TaintEffectNoSchedule),
					).
					WithVolumes(
						accorev1.Volume().
							WithName("tmp").
							WithEmptyDir(accorev1.EmptyDirVolumeSource()),
						accorev1.Volume().
							WithName("cache").
							WithEmptyDir(accorev1.EmptyDirVolumeSource()),
					).
					WithContainers(accorev1.Container().
						WithName("metalman").
						WithImage(p.Image).
						WithImagePullPolicy(corev1.PullAlways).
						WithArgs("serve-pxe", "--site="+p.Site).
						WithPorts(
							accorev1.ContainerPort().
								WithContainerPort(8880).
								WithName("http").
								WithProtocol(corev1.ProtocolTCP),
							accorev1.ContainerPort().
								WithContainerPort(8081).
								WithName("health").
								WithProtocol(corev1.ProtocolTCP),
							accorev1.ContainerPort().
								WithContainerPort(67).
								WithName("dhcp").
								WithProtocol(corev1.ProtocolUDP),
							accorev1.ContainerPort().
								WithContainerPort(69).
								WithName("tftp").
								WithProtocol(corev1.ProtocolUDP),
						).
						WithResources(accorev1.ResourceRequirements().
							WithRequests(corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							}).
							WithLimits(corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							}),
						).
						WithLivenessProbe(accorev1.Probe().
							WithHTTPGet(accorev1.HTTPGetAction().
								WithPath("/healthz").
								WithPort(intstr.FromString("health")),
							).
							WithInitialDelaySeconds(5).
							WithPeriodSeconds(10),
						).
						WithReadinessProbe(accorev1.Probe().
							WithHTTPGet(accorev1.HTTPGetAction().
								WithPath("/readyz").
								WithPort(intstr.FromString("health")),
							).
							WithInitialDelaySeconds(3).
							WithPeriodSeconds(5),
						).
						WithVolumeMounts(
							accorev1.VolumeMount().
								WithName("tmp").
								WithMountPath("/tmp"),
							accorev1.VolumeMount().
								WithName("cache").
								WithMountPath("/var/cache/metalman"),
						),
					),
				),
			),
		)
}

// deployPXEHandler holds flags and state for the deploy-pxe command.
type deployPXEHandler struct {
	kubeconfigPath string
	site           string
	image          string
}

func (h *deployPXEHandler) execute(ctx context.Context) error {
	if h.site == "" {
		return fmt.Errorf("--site is required")
	}

	kubeconfigPath := getKubeconfigPath(h.kubeconfigPath)

	clientset, _, err := kube.ClientAndConfigFromFile(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("creating kubernetes client: %w", err)
	}

	deploy := buildPXEDeployment(deployPXEParams{
		Site:  h.site,
		Image: h.image,
	})

	result, err := clientset.AppsV1().Deployments(deployPXENamespace).Apply(
		ctx, deploy, metav1.ApplyOptions{FieldManager: fieldManagerID},
	)
	if err != nil {
		return fmt.Errorf("applying Deployment %q: %w", ptr.Deref(deploy.Name, ""), err)
	}

	fmt.Printf("Deployment/%s applied\n", result.Name)

	return nil
}

func deployPXECommand() *cobra.Command {
	handler := &deployPXEHandler{}

	cmd := &cobra.Command{
		Use:   "deploy-pxe",
		Short: "Deploy the PXE server for a site",
		Long: `Deploy (or update) a Kubernetes Deployment running metalman serve-pxe
for a given site. The Deployment is server-side applied into the
unbounded-kube namespace.`,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return handler.execute(cmd.Context())
		},
	}

	cmd.Flags().StringVar(&handler.kubeconfigPath, "kubeconfig", "", "Path to kubeconfig file")
	cmd.Flags().StringVar(&handler.site, "site", "", "Site name (required; scopes the PXE instance to machines labeled unbounded-kube.io/site=<site>)")
	cmd.Flags().StringVar(&handler.image, "image", MetalmanImage, "Container image for the PXE deployment")

	if err := cmd.MarkFlagRequired("site"); err != nil {
		panic(err)
	}

	return cmd
}
