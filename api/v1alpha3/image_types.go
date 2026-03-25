package v1alpha3

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func init() {
	SchemeBuilder.Register(&Image{}, &ImageList{})
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=img
// +kubebuilder:printcolumn:name="Boot Image",type=string,JSONPath=`.spec.dhcpBootImageName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Image struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ImageSpec   `json:"spec"`
	Status            ImageStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ImageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Image `json:"items"`
}

type ImageSpec struct {
	DHCPBootImageName string `json:"dhcpBootImageName,omitempty"`
	Files             []File `json:"files,omitempty"`
}

type File struct {
	Path     string          `json:"path"`
	HTTP     *HTTPSource     `json:"http,omitempty"`
	Template *TemplateSource `json:"template,omitempty"`
	Static   *StaticSource   `json:"static,omitempty"`
}

type HTTPSource struct {
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Convert string `json:"convert,omitempty"`
}

type TemplateSource struct {
	Content string `json:"content"`
}

type StaticSource struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding,omitempty"`
}

type ImageStatus struct{}
