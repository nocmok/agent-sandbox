// Package k8sclient builds the Kubernetes REST config sandboxd talks to
// the cluster with.
package k8sclient

import (
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// BuildConfig prefers in-cluster credentials (when running as a Pod) and
// falls back to the local kubeconfig — respecting $KUBECONFIG and the
// standard loading rules — for local development.
func BuildConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return cfg, nil
}
