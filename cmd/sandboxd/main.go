// Command sandboxd is the Agent Sandbox API server: it manages sandbox
// metadata as Kubernetes custom resources and runs exec commands in
// ephemeral Jobs. See docs/openapi.yml and docs/00-sandboxes-over-k8s.md.
package main

import (
	"log"
	"net/http"
	"os"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"agent-sandbox/internal/httpapi"
	"agent-sandbox/internal/k8sclient"
	"agent-sandbox/internal/sandbox"
)

func main() {
	namespace := getEnv("NAMESPACE", "default")
	addr := ":" + getEnv("PORT", "8080")

	cfg, err := k8sclient.BuildConfig()
	if err != nil {
		log.Fatalf("build kubernetes config: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("build dynamic client: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("build kubernetes clientset: %v", err)
	}

	store := sandbox.NewStore(dynClient, namespace)
	executor := sandbox.NewExecutor(clientset, namespace)
	svc := sandbox.NewService(store, executor)

	router := httpapi.NewRouter(svc)

	log.Printf("sandboxd listening on %s (namespace=%s)", addr, namespace)
	if err := http.ListenAndServe(addr, router); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
