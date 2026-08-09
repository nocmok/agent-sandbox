// Package dockerengine implements sandbox.DockerClient against the real
// Docker Engine API. The fixed container security profile from
// docs/00-agent-sandbox-architecture.md lives here as constants, in one
// auditable place rather than scattered through the service/handler layers.
package dockerengine

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/containerd/errdefs"
	dockerclient "github.com/docker/docker/client"

	"agent-sandbox/internal/sandbox"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	containerUser   = "1000:1000"
	containerMemory = 2 * 1024 * 1024 * 1024 // 2g
	containerCPUs   = 2_000_000_000          // 2 NanoCPUs
	containerPids   = 100
)

type Client struct {
	cli           *dockerclient.Client
	nfsExportHost string
}

// New constructs a Client. nfsExportHost is the NFS server's hostname on the
// shared Docker network (used in NFS-backed volume driver_opts); network is
// the Docker network name sandbox containers are attached to so they can
// resolve it.
func New(nfsExportHost string) (*Client, error) {
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Client{cli: cli, nfsExportHost: nfsExportHost}, nil
}

func (c *Client) ImageExists(ctx context.Context, img string) (bool, error) {
	_, err := c.cli.ImageInspect(ctx, img)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *Client) PullImage(ctx context.Context, img string) error {
	rc, err := c.cli.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

func (c *Client) CreateVolume(ctx context.Context, name string, nfsDevicePath string) error {
	_, err := c.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   name,
		Driver: "local",
		DriverOpts: map[string]string{
			"type":   "nfs",
			"o":      fmt.Sprintf("addr=%s,rw,nolock,nfsvers=4", c.nfsExportHost),
			"device": nfsDevicePath,
		},
	})
	return err
}

func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	return c.cli.VolumeRemove(ctx, name, true)
}

func (c *Client) ContainerStatus(ctx context.Context, name string) (sandbox.ContainerStatus, error) {
	inspect, err := c.cli.ContainerInspect(ctx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return sandbox.ContainerStatusNotExist, nil
		}
		return sandbox.ContainerStatusNotExist, err
	}
	if inspect.State != nil && inspect.State.Running {
		return sandbox.ContainerStatusRunning, nil
	}
	return sandbox.ContainerStatusExited, nil
}

func (c *Client) RemoveContainer(ctx context.Context, name string) error {
	err := c.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true, RemoveVolumes: false})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// RunContainer creates the container with the fixed security profile from
// docs/00-agent-sandbox-architecture.md, attaches to its combined
// stdout/stderr stream, starts it, streams output to out until the container
// exits, and lets Docker auto-remove it (--rm) once done.
func (c *Client) RunContainer(ctx context.Context, spec sandbox.ContainerSpec, command string, out io.Writer) (int, error) {
	pidsLimit := int64(containerPids)

	cfg := &container.Config{
		Image:        spec.Image,
		Entrypoint:   []string{"/bin/sh", "-c", command},
		User:         containerUser,
		AttachStdout: true,
		AttachStderr: true,
	}

	hostCfg := &container.HostConfig{
		AutoRemove:     true,
		CapDrop:        []string{"ALL"},
		SecurityOpt:    []string{"no-new-privileges"},
		ReadonlyRootfs: true,
		Tmpfs: map[string]string{
			"/tmp":        "rw,noexec,nosuid,size=100m",
			"/home/agent": "rw,noexec,nosuid,size=500m",
		},
		Resources: container.Resources{
			Memory:    containerMemory,
			NanoCPUs:  containerCPUs,
			PidsLimit: &pidsLimit,
		},
		Mounts: []mount.Mount{{
			Type:   mount.TypeVolume,
			Source: spec.VolumeName,
			Target: spec.MountPath,
		}},
	}

	t0 := time.Now()
	resp, err := c.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.Name)
	if err != nil {
		return 0, err
	}
	log.Printf("[profile] %s ContainerCreate took %s", spec.Name, time.Since(t0))
	containerID := resp.ID

	t1 := time.Now()
	attached, err := c.cli.ContainerAttach(ctx, containerID, container.AttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return 0, err
	}
	defer attached.Close()
	log.Printf("[profile] %s ContainerAttach took %s", spec.Name, time.Since(t1))

	t2 := time.Now()
	if err := c.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return 0, err
	}
	log.Printf("[profile] %s ContainerStart took %s (time to first byte from here)", spec.Name, time.Since(t2))

	copyDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(out, out, attached.Reader)
		copyDone <- err
	}()

	// WaitConditionRemoved, not WaitConditionNotRunning: the container has
	// AutoRemove set, and removal (which detaches its volume) happens
	// asynchronously after it exits. Waiting only for "not running" races
	// the subsequent RemoveVolume call against that in-progress teardown.
	t3 := time.Now()
	statusCh, errCh := c.cli.ContainerWait(ctx, containerID, container.WaitConditionRemoved)
	var exitCode int
	select {
	case err := <-errCh:
		return 0, err
	case status := <-statusCh:
		exitCode = int(status.StatusCode)
	}
	log.Printf("[profile] %s ContainerWait(Removed) took %s (includes command run time)", spec.Name, time.Since(t3))

	if err := <-copyDone; err != nil {
		return exitCode, err
	}
	return exitCode, nil
}
