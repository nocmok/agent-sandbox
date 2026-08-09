package httpapi

import (
	"net/http"

	"github.com/google/uuid"
)

func parseID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(r.PathValue("id"))
}
