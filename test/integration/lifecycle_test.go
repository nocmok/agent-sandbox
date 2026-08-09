//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type sandboxDTO struct {
	ID string `json:"id"`
}

func createSandbox(t *testing.T, idemKey string) sandboxDTO {
	t.Helper()
	body := `{"image":"alpine","workspace":"/workspace"}`
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/sandboxes", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", idemKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: expected 200, got %d: %s", resp.StatusCode, b)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return sandboxDTO{ID: out.ID}
}

func getSandbox(t *testing.T, id string) (sandboxDTO, int) {
	t.Helper()
	resp, err := http.Get(baseURL + "/sandboxes/" + id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var sb sandboxDTO
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&sb); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return sb, resp.StatusCode
}

func execSSE(t *testing.T, id, command string) (string, int) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"command": command})
	resp, err := http.Post(baseURL+"/sandboxes/"+id+"/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out), resp.StatusCode
}

func uploadFile(t *testing.T, id, path, content string) int {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "upload")
	if err != nil {
		t.Fatalf("multipart: %v", err)
	}
	part.Write([]byte(content))
	mw.Close()

	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/sandboxes/%s/files?path=%s", baseURL, id, path), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func downloadFile(t *testing.T, id, path string) (string, int) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/sandboxes/%s/files?path=%s", baseURL, id, path))
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return string(b), resp.StatusCode
	}
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parsing content-type: %v", err)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	part, err := mr.NextPart()
	if err != nil {
		t.Fatalf("reading multipart part: %v", err)
	}
	content, err := io.ReadAll(part)
	if err != nil {
		t.Fatalf("reading part content: %v", err)
	}
	return string(content), resp.StatusCode
}

func deleteSandbox(t *testing.T, id string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, baseURL+"/sandboxes/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestFullLifecycle(t *testing.T) {
	idemKey := uuid.New().String()

	sb := createSandbox(t, idemKey)
	if sb.ID == "" {
		t.Fatal("expected non-empty id")
	}

	t.Run("idempotent re-create returns same id", func(t *testing.T) {
		sb2 := createSandbox(t, idemKey)
		if sb2.ID != sb.ID {
			t.Fatalf("expected same id, got %s and %s", sb.ID, sb2.ID)
		}
	})

	t.Run("new sandbox is gettable", func(t *testing.T) {
		got, code := getSandbox(t, sb.ID)
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if got.ID != sb.ID {
			t.Fatalf("expected id %s, got %s", sb.ID, got.ID)
		}
	})

	t.Run("upload and download file", func(t *testing.T) {
		if code := uploadFile(t, sb.ID, "test.txt", "hello persisted data"); code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		content, code := downloadFile(t, sb.ID, "test.txt")
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if content != "hello persisted data" {
			t.Fatalf("expected roundtripped content, got %q", content)
		}
	})

	t.Run("exec runs a command, streams output, and sees uploaded file", func(t *testing.T) {
		out, code := execSSE(t, sb.ID, "echo hello-from-exec && id -u && cat /workspace/test.txt")
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", code, out)
		}
		if !strings.Contains(out, "hello-from-exec") {
			t.Fatalf("expected output to contain hello-from-exec, got %q", out)
		}
		if !strings.Contains(out, "1000") {
			t.Fatalf("expected output to contain uid 1000, got %q", out)
		}
		if !strings.Contains(out, "hello persisted data") {
			t.Fatalf("expected output to contain uploaded file contents, got %q", out)
		}
		if !strings.Contains(out, `event: exit`) || !strings.Contains(out, `"code":0`) {
			t.Fatalf("expected a successful exit event, got %q", out)
		}
	})

	t.Run("file written during exec persists after it finishes", func(t *testing.T) {
		out, code := execSSE(t, sb.ID, "echo written-during-exec > /workspace/during.txt")
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", code, out)
		}
		content, code := downloadFile(t, sb.ID, "during.txt")
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d", code)
		}
		if strings.TrimSpace(content) != "written-during-exec" {
			t.Fatalf("expected persisted content, got %q", content)
		}
	})

	t.Run("exec, delete, and files all reject concurrent access while an exec is running", func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(1)
		execDone := make(chan struct{})
		go func() {
			defer wg.Done()
			defer close(execDone)
			execSSE(t, sb.ID, "sleep 3")
		}()

		// Give the exec a moment to acquire the lock before probing it.
		time.Sleep(500 * time.Millisecond)

		if code := uploadFile(t, sb.ID, "blocked.txt", "should not land"); code != http.StatusConflict {
			t.Fatalf("expected 409 uploading while executing, got %d", code)
		}
		if code := deleteSandbox(t, sb.ID); code != http.StatusConflict {
			t.Fatalf("expected 409 deleting while executing, got %d", code)
		}
		_, code := execSSE(t, sb.ID, "echo should-not-run")
		if code != http.StatusConflict {
			t.Fatalf("expected 409 for a second concurrent exec, got %d", code)
		}

		<-execDone
		wg.Wait()
	})

	t.Run("delete succeeds once idle", func(t *testing.T) {
		if code := deleteSandbox(t, sb.ID); code != http.StatusNoContent {
			t.Fatalf("expected 204, got %d", code)
		}
		if _, code := getSandbox(t, sb.ID); code != http.StatusNotFound {
			t.Fatalf("expected 404 after delete, got %d", code)
		}
	})
}

func TestConcurrentExecsOnSameSandboxOneWins(t *testing.T) {
	sb := createSandbox(t, uuid.New().String())
	defer deleteSandbox(t, sb.ID)

	type result struct {
		out  string
		code int
	}
	done := make(chan result, 2)
	go func() {
		out, code := execSSE(t, sb.ID, "echo one")
		done <- result{out, code}
	}()
	go func() {
		out, code := execSSE(t, sb.ID, "echo two")
		done <- result{out, code}
	}()

	deadline := time.After(30 * time.Second)
	var oks int
	for i := 0; i < 2; i++ {
		select {
		case r := <-done:
			switch r.code {
			case http.StatusOK:
				oks++
			case http.StatusConflict:
				// expected outcome for whichever call loses the lock race
			default:
				t.Fatalf("unexpected status %d: %s", r.code, r.out)
			}
		case <-deadline:
			t.Fatal("timed out waiting for concurrent execs — possible deadlock")
		}
	}
	if oks < 1 {
		t.Fatalf("expected at least one exec to succeed, got 0 of 2")
	}
}
