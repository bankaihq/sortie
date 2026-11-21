package clickup

import (
	"errors"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// validateConfig checks ClickUp-specific configuration constraints and
// returns diagnostics for the sortie validate pipeline. It does not
// construct an adapter or make network calls, and it never places the
// api_key value in a diagnostic message.
func validateConfig(fields registry.TrackerConfigFields) []registry.ValidationDiag {
	var diags []registry.ValidationDiag

	diags = append(diags, validateEndpoint(fields.Endpoint)...)
	diags = append(diags, validateProject(fields.Project)...)
	diags = append(diags, validateAPIKeyHint(fields.APIKey)...)
	diags = append(diags, validateQueryFilter(fields.QueryFilter)...)
	diags = append(diags, registry.DiagStateLabelElements("tracker.active_states", fields.ActiveStates, registry.SeverityWarning)...)
	diags = append(diags, registry.DiagStateLabelElements("tracker.terminal_states", fields.TerminalStates, registry.SeverityWarning)...)
	diags = append(diags, registry.DiagStateOverlap(fields)...)

	return diags
}

// validateEndpoint checks tracker.endpoint against the rule
// [NewClickUpAdapter] enforces. An empty endpoint selects the ClickUp API.
func validateEndpoint(endpoint string) []registry.ValidationDiag {
	if endpoint == "" || validEndpoint(strings.TrimRight(endpoint, "/")) {
		return nil
	}
	return []registry.ValidationDiag{{
		Severity: "error",
		Check:    "tracker.endpoint.invalid",
		Message:  "tracker.endpoint must be a URL with a scheme and host, such as " + defaultEndpoint,
	}}
}

// validateProject warns when tracker.project does not look like a list ID.
// ClickUp list IDs are numeric; a list name or URL is a common mistake.
func validateProject(project string) []registry.ValidationDiag {
	if project == "" {
		return nil
	}
	for _, r := range project {
		if r < '0' || r > '9' {
			return []registry.ValidationDiag{{
				Severity: "warning",
				Check:    "tracker.project.format",
				Message:  "tracker.project should be the numeric ClickUp list ID, as it appears after /li/ in the list's URL",
			}}
		}
	}
	return nil
}

// validateAPIKeyHint warns when tracker.api_key does not look like a
// ClickUp personal token. OAuth access tokens have another shape and are
// sent the same way, so this is a warning only.
func validateAPIKeyHint(apiKey string) []registry.ValidationDiag {
	if apiKey == "" || strings.HasPrefix(apiKey, "pk_") {
		return nil
	}
	return []registry.ValidationDiag{{
		Severity: "warning",
		Check:    "tracker.api_key.clickup_format",
		Message:  `tracker.api_key does not start with "pk_"; a ClickUp personal token does`,
	}}
}

// validateQueryFilter checks tracker.query_filter with [parseQueryFilter],
// the grammar [NewClickUpAdapter] enforces, so the offline verdict cannot
// drift from the construction verdict.
func validateQueryFilter(raw string) []registry.ValidationDiag {
	_, err := parseQueryFilter(raw)
	if err == nil {
		return nil
	}

	message := err.Error()
	if trackerErr, ok := errors.AsType[*domain.TrackerError](err); ok {
		message = trackerErr.Message
	}

	return []registry.ValidationDiag{{
		Severity: "error",
		Check:    "tracker.query_filter.invalid",
		Message:  message,
	}}
}
