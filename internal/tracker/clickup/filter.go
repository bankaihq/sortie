package clickup

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// reservedFilterKeys are list-tasks parameters the adapter sets itself.
// Overriding one would change which tasks a state read returns rather than
// narrow the candidates, so naming one fails construction.
var reservedFilterKeys = map[string]struct{}{
	"statuses[]": {}, "include_closed": {}, "page": {}, "order_by": {},
	"reverse": {}, "subtasks": {}, "include_markdown_description": {},
	"archived": {},
}

// allowedFilterKeys are the list-tasks filter parameters the adapter
// passes through. ClickUp ignores a parameter it does not know, so any
// other key is refused rather than silently dropped.
var allowedFilterKeys = map[string]struct{}{
	"assignees[]":     {},
	"watchers[]":      {},
	"tags[]":          {},
	"custom_items[]":  {},
	"custom_fields":   {},
	"due_date_gt":     {},
	"due_date_lt":     {},
	"date_created_gt": {},
	"date_created_lt": {},
	"date_updated_gt": {},
	"date_updated_lt": {},
	"date_done_gt":    {},
	"date_done_lt":    {},
}

// parseQueryFilter parses tracker.query_filter as a URL query fragment for
// ClickUp's list-tasks route. An empty or blank value yields nil. A value
// that does not parse, or names a reserved or unknown key, returns a
// [*domain.TrackerError] of kind [domain.ErrTrackerPayload]. Keys are
// checked in sorted order so the reported key is stable.
func parseQueryFilter(raw string) (url.Values, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	values, err := url.ParseQuery(raw)
	if err != nil {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "clickup: tracker.query_filter is not a valid url query fragment",
			Err:     err,
		}
	}

	for _, key := range slices.Sorted(maps.Keys(values)) {
		if _, reserved := reservedFilterKeys[key]; reserved {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf("clickup: tracker.query_filter key %q is owned by the adapter and cannot be overridden", key),
			}
		}
		if _, allowed := allowedFilterKeys[key]; !allowed {
			return nil, &domain.TrackerError{
				Kind: domain.ErrTrackerPayload,
				Message: fmt.Sprintf(
					"clickup: tracker.query_filter key %q is not a filter clickup's list-tasks route honors; an unrecognized key is silently ignored rather than applied", key),
			}
		}
	}
	return values, nil
}
