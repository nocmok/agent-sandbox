package httpapi

type createSandboxRequest struct {
	ID    string `json:"id"`
	Image string `json:"image"`
}

type sandboxResponse struct {
	ID    string `json:"id"`
	Image string `json:"image"`
}

type execRequest struct {
	Command string `json:"command"`
}
