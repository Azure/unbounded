// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	ManagedKubeProxyNodeLabelKey   = "net.unbounded-cloud.io/kube-proxy"
	ManagedKubeProxyNodeLabelValue = "managed"

	managedKubeProxyNamePrefix = "unbounded-net-kube-proxy-"
	managedKubeProxyAppName    = "unbounded-net-kube-proxy"
)

var kubeProxySiteGVR = schema.GroupVersionResource{
	Group:    unboundedv1alpha3.GroupVersion.Group,
	Version:  unboundedv1alpha3.GroupVersion.Version,
	Resource: "sites",
}

// ManagedKubeProxyOptions configures the kube-proxy DaemonSets created for
// unbounded-managed nodes.
type ManagedKubeProxyOptions struct {
	Namespace string
	Image     string
}

// ManagedKubeProxyController reconciles kube-proxy DaemonSets for unbounded
// nodes that are not already covered by the cluster provider's kube-proxy.
type ManagedKubeProxyController struct {
	clientset kubernetes.Interface
	options   ManagedKubeProxyOptions

	nodeLister      corev1listers.NodeLister
	nodeSynced      cache.InformerSynced
	dsLister        appsv1listers.DaemonSetLister
	dsSynced        cache.InformerSynced
	siteInformer    cache.SharedIndexInformer
	siteSynced      cache.InformerSynced
	workqueue       workqueue.TypedRateLimitingInterface[string]
	providerDSCache []*appsv1.DaemonSet
}

// NewManagedKubeProxyController creates a controller for managed kube-proxy.
func NewManagedKubeProxyController(
	clientset kubernetes.Interface,
	dynamicInformerFactory dynamicinformer.DynamicSharedInformerFactory,
	informerFactory informers.SharedInformerFactory,
	options ManagedKubeProxyOptions,
) (*ManagedKubeProxyController, error) {
	if options.Namespace == "" {
		return nil, fmt.Errorf("managed kube-proxy namespace is required")
	}

	if options.Image == "" {
		return nil, fmt.Errorf("managed kube-proxy image is required")
	}

	nodeInformer := informerFactory.Core().V1().Nodes()
	dsInformer := informerFactory.Apps().V1().DaemonSets()
	siteInformer := dynamicInformerFactory.ForResource(kubeProxySiteGVR).Informer()

	c := &ManagedKubeProxyController{
		clientset:    clientset,
		options:      options,
		nodeLister:   nodeInformer.Lister(),
		nodeSynced:   nodeInformer.Informer().HasSynced,
		dsLister:     dsInformer.Lister(),
		dsSynced:     dsInformer.Informer().HasSynced,
		siteInformer: siteInformer,
		siteSynced:   siteInformer.HasSynced,
		workqueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "ManagedKubeProxy"},
		),
	}

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.enqueueAll() },
		UpdateFunc: func(any, any) { c.enqueueAll() },
		DeleteFunc: func(any) { c.enqueueAll() },
	}

	if _, err := nodeInformer.Informer().AddEventHandler(handler); err != nil {
		return nil, fmt.Errorf("add node event handler: %w", err)
	}

	if _, err := dsInformer.Informer().AddEventHandler(handler); err != nil {
		return nil, fmt.Errorf("add daemonset event handler: %w", err)
	}

	if _, err := siteInformer.AddEventHandler(handler); err != nil {
		return nil, fmt.Errorf("add site event handler: %w", err)
	}

	return c, nil
}

// Run starts the managed kube-proxy controller.
func (c *ManagedKubeProxyController) Run(ctx context.Context, workers int) error {
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(ctx.Done(), c.nodeSynced, c.dsSynced, c.siteSynced); !ok {
		return fmt.Errorf("failed to wait for managed kube-proxy caches to sync")
	}

	c.enqueueAll()

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runWorker, time.Second)
	}

	<-ctx.Done()

	return nil
}

func fromUnstructured(u *unstructured.Unstructured, out any) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out)
}

func resourceMustParse(value string) resource.Quantity {
	return resource.MustParse(value)
}

func (c *ManagedKubeProxyController) enqueueAll() {
	c.workqueue.Add("all")
}

func (c *ManagedKubeProxyController) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *ManagedKubeProxyController) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := c.workqueue.Get()
	if shutdown {
		return false
	}
	defer c.workqueue.Done(key)

	if err := c.sync(ctx); err != nil {
		c.workqueue.AddRateLimited(key)
		klog.Errorf("managed kube-proxy sync failed: %v", err)
	} else {
		c.workqueue.Forget(key)
	}

	return true
}

func (c *ManagedKubeProxyController) sync(ctx context.Context) error {
	sites, err := c.listSites()
	if err != nil {
		return err
	}

	nodes, err := c.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}

	dsList, err := c.dsLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("list daemonsets: %w", err)
	}

	c.providerDSCache = providerKubeProxyDaemonSets(dsList)

	siteNames := map[string]struct{}{}

	for i := range sites {
		site := sites[i]

		siteNames[site.Name] = struct{}{}
		if err := c.ensureDaemonSet(ctx, site); err != nil {
			return err
		}
	}

	if err := c.deleteStaleDaemonSets(ctx, dsList, siteNames); err != nil {
		return err
	}

	for _, node := range nodes {
		if err := c.reconcileNodeLabel(ctx, node); err != nil {
			return err
		}
	}

	return nil
}

func (c *ManagedKubeProxyController) listSites() ([]unboundedv1alpha3.Site, error) {
	objs := c.siteInformer.GetStore().List()

	sites := make([]unboundedv1alpha3.Site, 0, len(objs))
	for _, obj := range objs {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		var site unboundedv1alpha3.Site
		if err := fromUnstructured(u, &site); err != nil {
			return nil, fmt.Errorf("decode Site %s: %w", u.GetName(), err)
		}

		sites = append(sites, site)
	}

	return sites, nil
}

func (c *ManagedKubeProxyController) reconcileNodeLabel(ctx context.Context, node *corev1.Node) error {
	current := ""
	if node.Labels != nil {
		current = node.Labels[ManagedKubeProxyNodeLabelKey]
	}

	want := shouldManageKubeProxyForNode(node, c.providerDSCache)

	if want && current == ManagedKubeProxyNodeLabelValue {
		return nil
	}

	if !want && current == "" {
		return nil
	}

	if want {
		patch := fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, ManagedKubeProxyNodeLabelKey, ManagedKubeProxyNodeLabelValue)
		_, err := c.clientset.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})

		return err
	}

	patch := fmt.Sprintf(`[{"op":"remove","path":"/metadata/labels/%s"}]`, escapeJSONPointer(ManagedKubeProxyNodeLabelKey))

	_, err := c.clientset.CoreV1().Nodes().Patch(ctx, node.Name, types.JSONPatchType, []byte(patch), metav1.PatchOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsInvalid(err) {
		return nil
	}

	return err
}

func shouldManageKubeProxyForNode(node *corev1.Node, providerDS []*appsv1.DaemonSet) bool {
	if NodeSiteLabel(node) == "" {
		return false
	}

	if _, exists := node.Labels["kubernetes.azure.com/managedby"]; exists {
		return false
	}

	if node.Labels["kubernetes.azure.com/cluster"] != "" {
		return false
	}

	for _, ds := range providerDS {
		if daemonSetCouldScheduleOnNode(ds, node) {
			return false
		}
	}

	return true
}

func providerKubeProxyDaemonSets(dsList []*appsv1.DaemonSet) []*appsv1.DaemonSet {
	var out []*appsv1.DaemonSet

	for _, ds := range dsList {
		if ds.Labels["app.kubernetes.io/name"] == managedKubeProxyAppName || strings.HasPrefix(ds.Name, managedKubeProxyNamePrefix) {
			continue
		}

		for _, c := range ds.Spec.Template.Spec.Containers {
			if c.Name == "kube-proxy" || strings.Contains(c.Image, "kube-proxy") {
				out = append(out, ds)
				break
			}
		}
	}

	return out
}

func daemonSetCouldScheduleOnNode(ds *appsv1.DaemonSet, node *corev1.Node) bool {
	for k, v := range ds.Spec.Template.Spec.NodeSelector {
		if node.Labels[k] != v {
			return false
		}
	}

	if affinity := ds.Spec.Template.Spec.Affinity; affinity != nil && affinity.NodeAffinity != nil && affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		if len(terms) > 0 {
			matched := false

			for _, term := range terms {
				if nodeSelectorTermMatchesNode(term.MatchExpressions, node.Labels) {
					matched = true
					break
				}
			}

			if !matched {
				return false
			}
		}
	}

	return true
}

func nodeSelectorTermMatchesNode(exprs []corev1.NodeSelectorRequirement, nodeLabels map[string]string) bool {
	for _, expr := range exprs {
		value, exists := nodeLabels[expr.Key]
		switch expr.Operator {
		case corev1.NodeSelectorOpIn:
			if !exists || !stringInSlice(value, expr.Values) {
				return false
			}
		case corev1.NodeSelectorOpNotIn:
			if exists && stringInSlice(value, expr.Values) {
				return false
			}
		case corev1.NodeSelectorOpExists:
			if !exists {
				return false
			}
		case corev1.NodeSelectorOpDoesNotExist:
			if exists {
				return false
			}
		}
	}

	return true
}

func stringInSlice(value string, values []string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}

	return false
}

func (c *ManagedKubeProxyController) ensureDaemonSet(ctx context.Context, site unboundedv1alpha3.Site) error {
	clusterCIDR, ok := siteKubeProxyClusterCIDR(site)
	if !ok {
		return nil
	}

	want := c.daemonSetForSite(site, clusterCIDR)

	existing, err := c.clientset.AppsV1().DaemonSets(c.options.Namespace).Get(ctx, want.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = c.clientset.AppsV1().DaemonSets(c.options.Namespace).Create(ctx, want, metav1.CreateOptions{})
		return err
	}

	if err != nil {
		return err
	}

	// spec.selector is immutable. When it differs (e.g. the site label moved
	// from the deprecated key to the canonical unbounded-cloud.io/site), the
	// DaemonSet cannot be updated in place; delete and recreate it. This is a
	// one-time, per-site kube-proxy blip during the label migration.
	if !equalLabelSelector(existing.Spec.Selector, want.Spec.Selector) {
		if err := c.clientset.AppsV1().DaemonSets(c.options.Namespace).Delete(ctx, existing.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}

		_, err = c.clientset.AppsV1().DaemonSets(c.options.Namespace).Create(ctx, want, metav1.CreateOptions{})

		return err
	}

	existing.Spec.Template = want.Spec.Template
	existing.Spec.UpdateStrategy = want.Spec.UpdateStrategy
	_, err = c.clientset.AppsV1().DaemonSets(c.options.Namespace).Update(ctx, existing, metav1.UpdateOptions{})

	return err
}

// equalLabelSelector reports whether two label selectors have the same
// matchLabels (matchExpressions are not used for the managed kube-proxy DS).
func equalLabelSelector(a, b *metav1.LabelSelector) bool {
	if a == nil || b == nil {
		return a == b
	}

	if len(a.MatchLabels) != len(b.MatchLabels) {
		return false
	}

	for k, v := range a.MatchLabels {
		if b.MatchLabels[k] != v {
			return false
		}
	}

	return true
}

func siteKubeProxyClusterCIDR(site unboundedv1alpha3.Site) (string, bool) {
	var ipv4, ipv6 string

	for _, assignment := range site.Spec.PodCidrAssignments {
		if !assignmentEnabled(assignment.AssignmentEnabled) {
			continue
		}

		for _, cidr := range assignment.CidrBlocks {
			if strings.Contains(cidr, ":") {
				if ipv6 == "" {
					ipv6 = cidr
				}

				continue
			}

			if ipv4 == "" {
				ipv4 = cidr
			}
		}
	}

	if ipv4 != "" && ipv6 != "" {
		return ipv4 + "," + ipv6, true
	}

	if ipv4 != "" {
		return ipv4, true
	}

	if ipv6 != "" {
		return ipv6, true
	}

	return "", false
}

func (c *ManagedKubeProxyController) daemonSetForSite(site unboundedv1alpha3.Site, clusterCIDR string) *appsv1.DaemonSet {
	name := managedKubeProxyDaemonSetName(site.Name)
	labels := map[string]string{
		"app.kubernetes.io/name":      managedKubeProxyAppName,
		"app.kubernetes.io/component": "kube-proxy",
		canonicalSiteLabelKey:         site.Name,
	}
	maxUnavailable := intstr.FromInt32(1)

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.options.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": managedKubeProxyAppName, canonicalSiteLabelKey: site.Name}},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type:          appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: &maxUnavailable},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"kubernetes.azure.com/set-kube-service-host-fqdn": "true"},
					Labels:      labels,
				},
				Spec: corev1.PodSpec{
					HostNetwork:        true,
					ServiceAccountName: "unbounded-net-kube-proxy",
					PriorityClassName:  "system-node-critical",
					NodeSelector: map[string]string{
						ManagedKubeProxyNodeLabelKey: ManagedKubeProxyNodeLabelValue,
						canonicalSiteLabelKey:        site.Name,
					},
					Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					InitContainers: []corev1.Container{{
						Name:            "kube-proxy-bootstrap",
						Image:           c.options.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"/bin/sh", "-c", kubeProxyBootstrapScript},
						SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
						Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resourceMustParse("100m")}},
						VolumeMounts:    kubeProxyBootstrapVolumeMounts(),
					}},
					Containers: []corev1.Container{{
						Name:            "kube-proxy",
						Image:           c.options.Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command: []string{
							"kube-proxy",
							"--conntrack-max-per-core=0",
							"--metrics-bind-address=0.0.0.0:10249",
							"--cluster-cidr=" + clusterCIDR,
							"--detect-local-mode=ClusterCIDR",
							"--pod-interface-name-prefix=",
							"--v=3",
						},
						SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
						Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resourceMustParse("100m")}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "iptableslock", MountPath: "/run/xtables.lock"},
							{Name: "modules", MountPath: "/lib/modules"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "iptableslock", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/xtables.lock", Type: hostPathTypePtr(corev1.HostPathFileOrCreate)}}},
						{Name: "sysctls", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc/sysctl.d", Type: hostPathTypePtr(corev1.HostPathDirectoryOrCreate)}}},
						{Name: "modules", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/lib/modules", Type: hostPathTypePtr(corev1.HostPathDirectory)}}},
					},
				},
			},
		},
	}
}

func (c *ManagedKubeProxyController) deleteStaleDaemonSets(ctx context.Context, dsList []*appsv1.DaemonSet, siteNames map[string]struct{}) error {
	for _, ds := range dsList {
		if ds.Namespace != c.options.Namespace || ds.Labels["app.kubernetes.io/name"] != managedKubeProxyAppName {
			continue
		}

		siteName := strings.TrimPrefix(ds.Name, managedKubeProxyNamePrefix)
		if _, ok := siteNames[siteName]; ok {
			continue
		}

		if err := c.clientset.AppsV1().DaemonSets(c.options.Namespace).Delete(ctx, ds.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

func managedKubeProxyDaemonSetName(siteName string) string {
	return managedKubeProxyNamePrefix + siteName
}

func boolPtr(v bool) *bool { return &v }

func hostPathTypePtr(v corev1.HostPathType) *corev1.HostPathType { return &v }

func kubeProxyBootstrapVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: "sysctls", MountPath: "/etc/sysctl.d"},
		{Name: "modules", MountPath: "/lib/modules"},
	}
}

const kubeProxyBootstrapScript = `get_num_cpu() {
  sys_cpu_online=$(cat /sys/devices/system/cpu/online)
  result=0
  OLD_IFS="$IFS"; IFS=","
  for rng in $sys_cpu_online; do
    if echo "$rng" | grep -q -- "-"; then
      min=${rng%-*}; max=${rng#*-}
      if [ "$min" -le "$max" ]; then
        result=$((result + (max - min + 1)))
      fi
    else
      result=$((result + 1))
    fi
  done
  IFS="$OLD_IFS"
  echo $result
}
SYSCTL=/proc/sys/net/netfilter/nf_conntrack_max
NUM_CPU=$(get_num_cpu)
DESIRED=$((32768*NUM_CPU))
if [ "$DESIRED" -lt 131072 ]; then DESIRED=131072; fi
echo "$DESIRED" > "$SYSCTL"
`
