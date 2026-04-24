// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// newScheme returns a runtime.Scheme that includes the core Kubernetes types
// and the unbounded-kube v1alpha3 types (Machine, MachineOperation, etc.).
func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(v1alpha3.AddToScheme(s))

	return s
}
