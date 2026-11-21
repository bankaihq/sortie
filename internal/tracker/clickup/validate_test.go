package clickup

import (
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

func TestValidateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fields registry.TrackerConfigFields
		want   []string // checks reported, in order
	}{
		{
			name:   "valid",
			fields: registry.TrackerConfigFields{Project: "901", APIKey: "pk_1", QueryFilter: "tags[]=agent"},
		},
		{
			name:   "bad endpoint",
			fields: registry.TrackerConfigFields{Endpoint: "clickup", Project: "901", APIKey: "pk_1"},
			want:   []string{"tracker.endpoint.invalid"},
		},
		{
			name:   "list url as project",
			fields: registry.TrackerConfigFields{Project: "https://app.clickup.com/1/v/li/901", APIKey: "pk_1"},
			want:   []string{"tracker.project.format"},
		},
		{
			name:   "token shape",
			fields: registry.TrackerConfigFields{Project: "901", APIKey: "secret"},
			want:   []string{"tracker.api_key.clickup_format"},
		},
		{
			name:   "reserved filter key",
			fields: registry.TrackerConfigFields{Project: "901", APIKey: "pk_1", QueryFilter: "page=2"},
			want:   []string{"tracker.query_filter.invalid"},
		},
		{
			name: "overlapping states",
			fields: registry.TrackerConfigFields{
				Project: "901", APIKey: "pk_1",
				ActiveStates: []string{"to do"}, TerminalStates: []string{"To Do"},
			},
			want: []string{"tracker.states.overlap"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			diags := validateConfig(tt.fields)
			got := make([]string, len(diags))
			for i, d := range diags {
				got[i] = d.Check
				if strings.Contains(d.Message, tt.fields.APIKey) && tt.fields.APIKey != "" {
					t.Errorf("diagnostic %q leaks the api_key", d.Message)
				}
			}
			if !slices.Equal(got, tt.want) { // nil and empty compare equal
				t.Errorf("checks = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestValidateQueryFilterMatchesConstructor pins that the offline verdict
// and the construction verdict agree.
func TestValidateQueryFilterMatchesConstructor(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "tags[]=a", "tags[]=a&assignees[]=1", "statuses[]=x", "nope=1", "%zz"} {
		_, err := parseQueryFilter(raw)
		diags := validateQueryFilter(raw)
		if (err == nil) != (len(diags) == 0) {
			t.Errorf("query_filter %q: constructor error %v, validate diags %v", raw, err, diags)
		}
	}
}
