package bedrockapi

import (
	"encoding/json"
	"net/http"
)

func exceptionName(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "ValidationException"
	case http.StatusUnauthorized:
		return "UnauthorizedException"
	case http.StatusForbidden:
		return "AccessDeniedException"
	case http.StatusNotFound:
		return "ResourceNotFoundException"
	case http.StatusRequestTimeout:
		return "ModelTimeoutException"
	case http.StatusTooManyRequests:
		return "ThrottlingException"
	default:
		return "InternalServerException"
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	body, _ := json.Marshal(struct {
		Message string `json:"message"`
	}{Message: msg})
	w.Header().Set("X-Amzn-ErrorType", exceptionName(status))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
