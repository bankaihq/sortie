package clickup

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

// newClickUpClient constructs the shared ClickUp transport. ClickUp takes
// a personal token in the Authorization header as is, with no scheme.
func newClickUpClient(baseURL, token, userAgent string) *httpkit.Client {
	return httpkit.NewClient(httpkit.ClientOptions{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Timeout: 30 * time.Second,
		Authorize: func(req *http.Request) {
			req.Header.Set("Authorization", token)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", userAgent)
		},
		ClassifyError:     classifyHTTPError,
		ClassifyTransport: httpkit.ClassifyTransport,
	})
}

// maxErrorBody is the maximum number of bytes read from a non-success
// response body for diagnostic error messages.
const maxErrorBody = 512

// notFoundCodes are the ClickUp error codes that mean the task does not
// exist or was deleted, whatever HTTP status carries them.
var notFoundCodes = map[string]struct{}{
	"ITEM_013": {},
}

// clickupError is the error body ClickUp sends with a non-success status.
type clickupError struct {
	Err   string `json:"err"`
	ECode string `json:"ECODE"`
}

// classifyHTTPError maps a non-success HTTP response to a
// [domain.TrackerError]. The method and path are included in the message
// for diagnostics; the token never is.
func classifyHTTPError(resp *http.Response, method, path string) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_, _ = io.Copy(io.Discard, resp.Body)
	detail := string(snippet)

	var body clickupError
	_ = json.Unmarshal(snippet, &body)
	if body.Err != "" {
		detail = body.Err
		if body.ECode != "" {
			detail += " (" + body.ECode + ")"
		}
	}

	if _, ok := notFoundCodes[body.ECode]; ok || resp.StatusCode == http.StatusNotFound {
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("%s %s: not found: %s", method, path, detail),
			Status:  resp.StatusCode,
		}
	}

	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("%s %s: bad request: %s", method, path, detail),
			Status:  resp.StatusCode,
		}

	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAuth,
			Message: fmt.Sprintf("%s %s: %d: %s", method, path, resp.StatusCode, detail),
			Status:  resp.StatusCode,
		}

	case resp.StatusCode == http.StatusTooManyRequests:
		msg := fmt.Sprintf("%s %s: rate limited", method, path)
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			msg += fmt.Sprintf(" (limit resets at unix time %s)", reset)
		}
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: msg,
			Status:  resp.StatusCode,
		}

	case resp.StatusCode >= 500:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerTransport,
			Message: fmt.Sprintf("%s %s: server error %d: %s", method, path, resp.StatusCode, detail),
			Status:  resp.StatusCode,
		}

	default:
		return &domain.TrackerError{
			Kind:    domain.ErrTrackerAPI,
			Message: fmt.Sprintf("%s %s: unexpected status %d: %s", method, path, resp.StatusCode, detail),
			Status:  resp.StatusCode,
		}
	}
}
