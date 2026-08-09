//go:build integration

// Package integration exercises the full sandboxd lifecycle against the real
// docker-compose stack (Postgres, Docker Engine, and a real NFS server). Run
// with:
//
//	go test -tags=integration ./test/integration/...
//
// Set KEEP_STACK=1 to leave the compose stack running after the tests for
// inspection instead of tearing it down.
package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"
)

var baseURL = "http://localhost:8080"

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	repoRoot, err := repoRootDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "locating repo root:", err)
		return 1
	}

	up := exec.Command("docker", "compose", "up", "-d", "--build")
	up.Dir = repoRoot
	up.Stdout = os.Stdout
	up.Stderr = os.Stderr
	if err := up.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "docker compose up failed:", err)
		return 1
	}

	if os.Getenv("KEEP_STACK") != "1" {
		defer func() {
			down := exec.Command("docker", "compose", "down", "-v")
			down.Dir = repoRoot
			down.Stdout = os.Stdout
			down.Stderr = os.Stderr
			_ = down.Run()
		}()
	}

	if err := waitForHealthy(90 * time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "sandboxd did not become healthy:", err)
		return 1
	}

	return m.Run()
}

func waitForHealthy(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func repoRootDir() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	dir := string(out)
	for len(dir) > 0 && (dir[len(dir)-1] == '\n' || dir[len(dir)-1] == '\r') {
		dir = dir[:len(dir)-1]
	}
	return dir, nil
}
