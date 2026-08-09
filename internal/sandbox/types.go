// Package sandbox implements the domain logic for managing agent sandboxes
// on top of Kubernetes: sandbox metadata lives as instances of a Sandbox
// custom resource (no separate database), and each sandbox gets a
// long-lived Pod, created alongside it and torn down with it, that exec
// commands run against via the pods/exec subresource. See the Executor doc
// comment for why, and for the exec-locking caveat.
package sandbox

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var sandboxGVR = schema.GroupVersionResource{
	Group:    "agent-sandbox.dev",
	Version:  "v1alpha1",
	Resource: "sandboxes",
}

// SandboxResource is the Go representation of the Sandbox custom resource.
// It is converted to/from unstructured.Unstructured when talking to the
// dynamic client.
type SandboxResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SandboxSpec `json:"spec"`
}

type SandboxSpec struct {
	Image string `json:"image"`
}

type SandboxResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxResource `json:"items"`
}
