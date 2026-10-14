package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/LocalImages/LocalImage/internal/session"
)

// Every error response has the shape {"error": "<message>", "code": "<stable code>"}
// (plus "details" where useful). Clients should branch on the code, never the message.
//
//	session_gone        404  the session does not exist (already deleted, expired, never created)
//	limit_exceeded      429  too many sessions, or a ttl over the limit (details: max, running)
//	invalid_request     400  the request is malformed or names something that does not exist
//	conflict            409  the request clashes with current state (e.g. profile in use)
//	image_not_ready     409  noBuild was set and the browser image is not built (see prepare)
//	unauthorized        401  missing or wrong API key
//	forbidden           403  the Host or Origin of the request is not allowed (see LOCALIMAGE_ALLOWED_HOSTS)
//	docker_unavailable  503  the daemon cannot reach Docker
//	not_found           404  some other resource does not exist
//	internal            500  anything else
type errorBody struct {
	Error   string `json:"error"`
	Code    string `json:"code"`
	Details any    `json:"details,omitempty"`
}

// classify maps an error to an HTTP status and stable code. Typed session
// errors win; otherwise the handler's fallback status is used.
func classify(err error, fallback int) (status int, code string, details any) {
	var limit *session.LimitError
	switch {
	case errors.As(err, &limit):
		return http.StatusTooManyRequests, "limit_exceeded", map[string]int{"max": limit.Max, "running": limit.Active}
	case errors.Is(err, session.ErrLimit):
		return http.StatusTooManyRequests, "limit_exceeded", nil
	case errors.Is(err, session.ErrNotFound):
		return http.StatusNotFound, "session_gone", nil
	case errors.Is(err, session.ErrInvalid):
		return http.StatusBadRequest, "invalid_request", nil
	case errors.Is(err, session.ErrNotReady):
		return http.StatusConflict, "image_not_ready", nil
	case errors.Is(err, session.ErrConflict):
		return http.StatusConflict, "conflict", nil
	case strings.Contains(err.Error(), "Cannot connect to the Docker daemon"):
		return http.StatusServiceUnavailable, "docker_unavailable", nil
	}
	switch fallback {
	case http.StatusBadRequest:
		return fallback, "invalid_request", nil
	case http.StatusUnauthorized:
		return fallback, "unauthorized", nil
	case http.StatusNotFound:
		return fallback, "not_found", nil
	case http.StatusConflict:
		return fallback, "conflict", nil
	case http.StatusForbidden:
		return fallback, "forbidden", nil
	case http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType:
		return fallback, "invalid_request", nil
	case http.StatusTooManyRequests:
		return fallback, "limit_exceeded", nil
	}
	return fallback, "internal", nil
}

// fail writes an error response. status is the fallback for untyped errors.
func fail(w http.ResponseWriter, status int, err error) {
	st, code, details := classify(err, status)
	if st == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "2")
	}
	writeJSON(w, st, errorBody{Error: err.Error(), Code: code, Details: details})
}
