package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// sseWriter frames each Write call as one or more Server-Sent Events
// messages, one JSON-encoded {type, value} event per line, flushing
// immediately so output streams to the client as it's produced. Kubernetes
// delivers pod log output in arbitrary chunk boundaries (not aligned to
// lines), so a chunk spanning multiple lines becomes one event per line; a
// chunk that splits a single line across two Write calls will render as two
// separate events. Accepted as a POC limitation.
//
// The Kubernetes pod logs API interleaves container stdout and stderr into
// a single stream with no per-line attribution, so Write (used for command
// output) always tags its events "stdout"; only writeEvent's caller can
// produce a "stderr" event, used for our own runtime errors.
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

type execEvent struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

func (s *sseWriter) writeEvent(eventType, line string) error {
	payload, err := json.Marshal(execEvent{Type: eventType, Value: line})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", payload); err != nil {
		return err
	}
	s.f.Flush()
	return nil
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
		if err := s.writeEvent("stdout", line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
