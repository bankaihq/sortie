package clickup

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/adaptertest"
)

// skipUnlessIntegration skips the current test when SORTIE_CLICKUP_TEST is
// not "1", so disabled integration tests are reported as skipped.
func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_CLICKUP_TEST") != "1" {
		t.Skip("skipping ClickUp integration test: set SORTIE_CLICKUP_TEST=1 to enable")
	}
}

// requireEnv reads an environment variable and fails the test when empty.
func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

// integrationAdapter builds an adapter from SORTIE_CLICKUP_API_KEY,
// SORTIE_CLICKUP_LIST and SORTIE_CLICKUP_ACTIVE_STATES (comma separated,
// default "to do"), with optional SORTIE_CLICKUP_TEAM_ID and
// SORTIE_CLICKUP_QUERY_FILTER.
func integrationAdapter(t *testing.T) *ClickUpAdapter {
	t.Helper()
	active := []any{}
	states := os.Getenv("SORTIE_CLICKUP_ACTIVE_STATES")
	if states == "" {
		states = "to do"
	}
	for s := range strings.SplitSeq(states, ",") {
		active = append(active, strings.TrimSpace(s))
	}
	cfg := map[string]any{
		"api_key":       requireEnv(t, "SORTIE_CLICKUP_API_KEY"),
		"project":       requireEnv(t, "SORTIE_CLICKUP_LIST"),
		"active_states": active,
		"team_id":       os.Getenv("SORTIE_CLICKUP_TEAM_ID"),
		"query_filter":  os.Getenv("SORTIE_CLICKUP_QUERY_FILTER"),
	}
	adapter, err := NewClickUpAdapter(cfg)
	if err != nil {
		t.Fatalf("NewClickUpAdapter: %v", err)
	}
	return adapter.(*ClickUpAdapter)
}

// TestIntegrationReadPath reads the configured list without writing: the
// candidates, then the first candidate in full and by ID and identifier.
func TestIntegrationReadPath(t *testing.T) {
	skipUnlessIntegration(t)
	adapter := integrationAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	issues, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(issues) == 0 {
		t.Skip("the configured list has no candidate tasks to read")
	}
	for _, iss := range issues {
		adaptertest.AssertIssueNormalized(t, iss)
	}

	first := issues[0]
	full, err := adapter.FetchIssueByID(ctx, first.ID)
	if err != nil {
		t.Fatalf("FetchIssueByID(%s): %v", first.ID, err)
	}
	if full.Comments == nil {
		t.Error("FetchIssueByID returned nil Comments, want a non-nil slice")
	}
	adaptertest.AssertCommentsAscending(t, full.Comments)
	adaptertest.AssertBlockerRefsNormalized(t, full.BlockedBy)

	states, err := adapter.FetchIssueStatesByIDs(ctx, []string{first.ID, "000000000"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	adaptertest.AssertStateMapOmitsMissing(t, []string{first.ID, "000000000"}, states)

	byKey, err := adapter.FetchIssueStatesByIdentifiers(ctx, []string{first.Identifier})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}
	if !strings.EqualFold(byKey[first.Identifier], first.State) {
		t.Errorf("state by identifier = %q, want %q", byKey[first.Identifier], first.State)
	}
}

// TestIntegrationCommentWrite posts one comment on the task named by
// SORTIE_CLICKUP_WRITE_TASK and checks that the read path lists it. It is
// separate from the read path so a read-only run leaves the list untouched.
func TestIntegrationCommentWrite(t *testing.T) {
	skipUnlessIntegration(t)
	taskID := os.Getenv("SORTIE_CLICKUP_WRITE_TASK")
	if taskID == "" {
		t.Skip("set SORTIE_CLICKUP_WRITE_TASK to a task ID to run the write test")
	}
	adapter := integrationAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	id, err := adapter.CommentIssue(ctx, taskID, "sortie clickup adapter integration test "+time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("CommentIssue: %v", err)
	}
	comments, err := adapter.FetchIssueComments(ctx, taskID)
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	adaptertest.AssertPostedCommentListed(t, id, comments)
}
