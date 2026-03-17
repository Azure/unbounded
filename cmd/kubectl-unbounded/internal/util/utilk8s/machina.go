package utilk8s

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1alpha2 "github.com/project-unbounded/unbounded-kube/cmd/machina/machina/api/v1alpha2"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(machinav1alpha2.AddToScheme(scheme))
}

// NewClientFromCLIOpts creates a new Kubernetes client with the machina API scheme registered.
func NewClientFromCLIOpts(opts *genericclioptions.ConfigFlags) (client.Client, error) {
	restConfig, err := opts.ToRESTConfig()
	if err != nil {
		return nil, err
	}

	return client.New(restConfig, client.Options{
		Scheme: scheme,
	})
}

// NewClientsetFromCLIOpts creates a standard Kubernetes clientset from CLI flags.
func NewClientsetFromCLIOpts(opts *genericclioptions.ConfigFlags) (kubernetes.Interface, error) {
	restConfig, err := opts.ToRESTConfig()
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(restConfig)
}
