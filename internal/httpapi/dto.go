package httpapi

import (
	"time"

	"agent-sandbox/internal/sandbox"
)

type createSandboxRequest struct {
	Image     string `json:"image"`
	Workspace string `json:"workspace"`
}

type execRequest struct {
	Command string `json:"command"`
}

type sandboxDTO struct {
	ID        string    `json:"id"`
	Image     string    `json:"image"`
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"created_at"`
}

func toDTO(sb sandbox.Sandbox) sandboxDTO {
	return sandboxDTO{
		ID:        sb.ID.String(),
		Image:     sb.Image,
		Workspace: sb.Workspace,
		CreatedAt: sb.CreatedAt,
	}
}
