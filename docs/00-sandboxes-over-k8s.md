# Sandboxes over k8s

POC of using k8s + Persistent Volumes to manage agentic sandboxes.
Isolation runtime becomes concern of k8s. It could be runc or kata containers or anything else supported by k8s.
Sandboxes are stored right in k8s cluster as CRD. No separate database needed.
Persistent volumes are not supported yet (future work).