package sandbox

import (
	"context"
	"io"
)

// ContainerSpec carries only what varies per-sandbox. The fixed security
// profile (cap-drop, tmpfs, resource limits, user, etc.) is applied inside
// internal/dockerengine as constants, not passed through here.
type ContainerSpec struct {
	Image      string
	Name       string // == sandbox id, per the doc's naming strategy
	VolumeName string // == sandbox id, per the doc's naming strategy
	MountPath  string // workspace directory inside the container
}

// ContainerStatus reports whether a sandbox's container currently exists in
// Docker and, if so, whether it's running - the basis for the doc's
// "is this sandbox executing" check.
type ContainerStatus int

const (
	ContainerStatusNotExist ContainerStatus = iota
	ContainerStatusRunning
	ContainerStatusExited
)

// DockerClient is implemented by internal/dockerengine against the Docker
// Engine API. Declared here (the consumer package) so unit tests can supply
// hand-written fakes.
type DockerClient interface {
	PullImage(ctx context.Context, image string) error

	// CreateVolume creates a Docker volume named name, NFS-backed at
	// nfsDevicePath (e.g. ":/exports/<id>").
	CreateVolume(ctx context.Context, name string, nfsDevicePath string) error
	RemoveVolume(ctx context.Context, name string) error

	// ContainerStatus looks up the container named spec.Name (the sandbox
	// id). Returns ContainerStatusNotExist if no such container exists.
	ContainerStatus(ctx context.Context, name string) (ContainerStatus, error)

	// RemoveContainer force-removes a leftover container, e.g. one found
	// exited during the doc's "dead container should be deleted" cleanup.
	RemoveContainer(ctx context.Context, name string) error

	// RunContainer creates and starts a fresh container for spec, runs
	// command via /bin/sh -c, streams demuxed stdout/stderr to out as it
	// arrives, waits for it to exit, and disposes of the container
	// (auto-remove) before returning - the full per-exec lifecycle described
	// in the doc.
	RunContainer(ctx context.Context, spec ContainerSpec, command string, out io.Writer) (exitCode int, err error)
}
