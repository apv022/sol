package sol

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Error is an API error: the {"error": {"code", "message"}} body of API.md. Compare with
// errors.Is against the Err* sentinels; only the code is compared.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error implements error.
func (e *Error) Error() string {
	if e.Message == "" {
		return "sol: " + e.Code
	}
	return "sol: " + e.Code + ": " + e.Message
}

// Is makes errors.Is(err, sol.ErrNotFound) match on the code alone.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// HTTPStatus is the HTTP status that goes with the error code.
func (e *Error) HTTPStatus() int {
	switch e.Code {
	case "bad_request":
		return 400
	case "unauthorized":
		return 401
	case "forbidden":
		return 403
	case "not_found":
		return 404
	case "conflict":
		return 409
	case "unavailable":
		return 503
	case "not_implemented":
		return 501
	}
	return 500
}

// Sentinels for the error codes of API.md.
var (
	ErrBadRequest     = &Error{Code: "bad_request"}
	ErrUnauthorized   = &Error{Code: "unauthorized"}
	ErrForbidden      = &Error{Code: "forbidden"}
	ErrNotFound       = &Error{Code: "not_found"}
	ErrConflict       = &Error{Code: "conflict"}
	ErrUnavailable    = &Error{Code: "unavailable"}
	ErrNotImplemented = &Error{Code: "not_implemented"}
)

// Errorf builds an Error with the given code and message.
func Errorf(code, msg string) *Error { return &Error{Code: code, Message: msg} }

func codeForStatus(status int) string {
	switch status {
	case 400:
		return "bad_request"
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 409:
		return "conflict"
	case 501:
		return "not_implemented"
	case 502, 503, 504:
		return "unavailable"
	}
	return "internal"
}

// WriteJSON writes v as a JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes err as an API error response. A non-*Error becomes code "internal" (500).
func WriteError(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Code: "internal", Message: err.Error()}
	}
	WriteJSON(w, e.HTTPStatus(), map[string]any{"error": e})
}
