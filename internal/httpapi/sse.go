package httpapi

import (
	"fmt"
	"net/http"
	"strings"
)

// sseWriter frames each Write call as one Server-Sent Events message,
// flushing immediately so output streams to the client as it's produced.
// Docker delivers exec output in arbitrary chunk boundaries (not aligned to
// lines), so a chunk spanning multiple lines is split into one "data:" line
// per line per the SSE multi-line convention; a chunk that splits a single
// line across two Write calls will render as two separate messages. Accepted
// as a POC limitation.
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (s *sseWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	text := strings.TrimRight(string(p), "\n")
	if text == "" {
		return len(p), nil
	}
	for _, line := range strings.Split(text, "\n") {
		if _, err := fmt.Fprintf(s.w, "data: %s\n", line); err != nil {
			return 0, err
		}
	}
	if _, err := fmt.Fprint(s.w, "\n"); err != nil {
		return 0, err
	}
	s.f.Flush()
	return len(p), nil
}
