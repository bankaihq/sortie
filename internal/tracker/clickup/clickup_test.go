package clickup

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func TestNewClickUpAdapterConfig(t *testing.T) {
	t.Parallel()

	base := func() map[string]any {
		return map[string]any{"api_key": testToken, "project": "901"}
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   domain.TrackerErrorKind
	}{
		{name: "defaults are valid", mutate: func(map[string]any) {}},
		{name: "numeric project from yaml", mutate: func(c map[string]any) { c["project"] = 901 }},
		{name: "whole float project", mutate: func(c map[string]any) { c["project"] = float64(901) }},
		{name: "numeric team id", mutate: func(c map[string]any) { c["team_id"] = 9012 }},
		{name: "missing api key", mutate: func(c map[string]any) { delete(c, "api_key") }, want: domain.ErrMissingTrackerAPIKey},
		{name: "missing project", mutate: func(c map[string]any) { delete(c, "project") }, want: domain.ErrMissingTrackerProject},
		{name: "fractional project", mutate: func(c map[string]any) { c["project"] = 9.5 }, want: domain.ErrTrackerPayload},
		{name: "endpoint without host", mutate: func(c map[string]any) { c["endpoint"] = "api.clickup.com" }, want: domain.ErrTrackerPayload},
		{name: "reserved filter key", mutate: func(c map[string]any) { c["query_filter"] = "statuses[]=open" }, want: domain.ErrTrackerPayload},
		{name: "unknown filter key", mutate: func(c map[string]any) { c["query_filter"] = "tag=agent" }, want: domain.ErrTrackerPayload},
		{name: "allowed filter keys", mutate: func(c map[string]any) { c["query_filter"] = "tags[]=agent-ready&assignees[]=42" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := base()
			tt.mutate(cfg)
			_, err := NewClickUpAdapter(cfg)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("NewClickUpAdapter: %v", err)
				}
				return
			}
			adaptertest.AssertTrackerErrorKind(t, err, tt.want)
		})
	}
}

func TestRegisteredMeta(t *testing.T) {
	t.Parallel()

	meta, ok := registry.Trackers.Meta("clickup")
	if !ok {
		t.Fatal(`tracker kind "clickup" is not registered`)
	}
	if !meta.RequiresAPIKey || !meta.RequiresProject {
		t.Errorf("meta = %+v, want api_key and project required", meta)
	}
	if meta.BlockerSource != registry.BlockersPerIssue {
		t.Errorf("BlockerSource = %q, want %q", meta.BlockerSource, registry.BlockersPerIssue)
	}
}

func TestFetchCandidateIssues(t *testing.T) {
	t.Parallel()

	fake, srv := newFake(t,
		&fakeTask{ID: "a1", CustomID: "ENG-1", Name: "First", Status: "to do", Tags: []string{"Agent-Ready"}, Priority: "2", Parent: "p9"},
		&fakeTask{ID: "a2", Name: "Second", Status: "in progress", WaitsOn: []string{"a1"}},
		&fakeTask{ID: "a3", Name: "Done", Status: "complete"},
		&fakeTask{ID: "a4", Name: "No deps field", Status: "to do", OmitDeps: true},
	)
	adapter := newTestAdapter(t, srv, nil)

	issues, err := adapter.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	got := make([]string, len(issues))
	for i, iss := range issues {
		got[i] = iss.ID
		adaptertest.AssertIssueNormalized(t, iss)
		if iss.Comments != nil {
			t.Errorf("issue %s Comments = %v, want nil", iss.ID, iss.Comments)
		}
	}
	if want := []string{"a1", "a2", "a4"}; !slices.Equal(got, want) {
		t.Fatalf("candidate IDs = %v, want %v", got, want)
	}

	first := issues[0]
	if first.Identifier != "ENG-1" {
		t.Errorf("Identifier = %q, want custom id ENG-1", first.Identifier)
	}
	if first.Priority == nil || *first.Priority != 2 {
		t.Errorf("Priority = %v, want 2", first.Priority)
	}
	if !slices.Equal(first.Labels, []string{"agent-ready"}) {
		t.Errorf("Labels = %v, want [agent-ready]", first.Labels)
	}
	if first.Description != "**First**" {
		t.Errorf("Description = %q, want the markdown description", first.Description)
	}
	if first.Parent == nil || first.Parent.ID != "p9" {
		t.Errorf("Parent = %+v, want p9", first.Parent)
	}
	if first.Assignee != "ada" || first.URL == "" || first.CreatedAt != "2023-11-14T22:13:20.000Z" {
		t.Errorf("Assignee/URL/CreatedAt = %q/%q/%q", first.Assignee, first.URL, first.CreatedAt)
	}

	// a1 is waited on by a2 but waits on nothing: answered from the payload.
	adaptertest.AssertCandidateBlockerSource(t, registry.BlockersPerIssue, issues[0], 0)
	// a2 waits on a1: left for the per-issue read.
	adaptertest.AssertCandidateBlockerSource(t, registry.BlockersPerIssue, issues[1], 1)
	// a4's payload proves nothing.
	if !issues[2].BlockersUnresolved {
		t.Error("a candidate without a dependencies field must be marked unresolved")
	}

	req := fake.calls()[0]
	for _, part := range []string{"statuses%5B%5D=to+do", "statuses%5B%5D=in+progress", "include_closed=true", "subtasks=true", "page=0"} {
		if !strings.Contains(req, part) {
			t.Errorf("list request %q lacks %q", req, part)
		}
	}
}

func TestFetchCandidateIssuesQueryFilterAndPaging(t *testing.T) {
	t.Parallel()

	var tasks []*fakeTask
	for i := range taskPageSize + 5 {
		tasks = append(tasks, &fakeTask{ID: "t" + string(rune('a'+i%26)) + strings.Repeat("x", i/26), Name: "n", Status: "to do", Tags: []string{"agent-ready"}})
	}
	tasks = append(tasks, &fakeTask{ID: "untagged", Name: "n", Status: "to do"})
	fake, srv := newFake(t, tasks...)
	adapter := newTestAdapter(t, srv, map[string]any{"query_filter": "tags[]=agent-ready"})

	issues, err := adapter.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(issues) != taskPageSize+5 {
		t.Errorf("got %d issues, want %d", len(issues), taskPageSize+5)
	}
	calls := fake.calls()
	if len(calls) != 2 || !strings.Contains(calls[1], "page=1") {
		t.Errorf("requests = %v, want two pages", calls)
	}
	if !strings.Contains(calls[0], "tags%5B%5D=agent-ready") {
		t.Errorf("first request %q does not carry the query filter", calls[0])
	}
}

func TestFetchCandidateIssuesEmptyActiveSendsNothing(t *testing.T) {
	t.Parallel()

	fake, srv := newFake(t)
	adapter := newTestAdapter(t, srv, map[string]any{"active_states": []any{}})

	issues, err := adapter.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	adaptertest.AssertEmptyNonNil(t, issues, "FetchCandidateIssues")
	adaptertest.AssertNoRequestOnEmptyInput(t, len(fake.calls()), "FetchCandidateIssues")
}

func TestFetchIssuesByStates(t *testing.T) {
	t.Parallel()

	_, srv := newFake(t,
		&fakeTask{ID: "a1", Name: "Open", Status: "to do"},
		&fakeTask{ID: "a2", Name: "Done", Status: "complete"},
	)
	adapter := newTestAdapter(t, srv, nil)

	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"Complete"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}
	if len(issues) != 1 || issues[0].ID != "a2" {
		t.Errorf("issues = %+v, want only a2", issues)
	}
}

func TestFetchIssueByID(t *testing.T) {
	t.Parallel()

	comments := manyComments(commentPageSize + 3)
	comments[1].Replies = []fakeComment{{ID: "r1", Text: "a reply", User: "cy", UserID: 3, DateMS: comments[len(comments)-1].DateMS + 5000}}
	fake, srv := newFake(t,
		&fakeTask{ID: "b1", CustomID: "ENG-9", Name: "Blocker", Status: "complete"},
		&fakeTask{ID: "a1", Name: "Work", Status: "to do", WaitsOn: []string{"b1", "gone"}, Comments: comments},
	)
	adapter := newTestAdapter(t, srv, nil)

	issue, err := adapter.FetchIssueByID(context.Background(), "a1")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}

	if issue.BlockersUnresolved {
		t.Error("BlockersUnresolved = true, want resolved on a full read")
	}
	adaptertest.AssertBlockerRefsNormalized(t, issue.BlockedBy)
	adaptertest.AssertBlockerIdentifiersMatchIssue(t, issue, issue.BlockedBy)
	want := []domain.BlockerRef{{ID: "b1", Identifier: "ENG-9", State: "complete"}}
	if !slices.Equal(issue.BlockedBy, want) {
		t.Errorf("BlockedBy = %+v, want %+v (a missing blocker is left out)", issue.BlockedBy, want)
	}

	if len(issue.Comments) != len(comments)+1 {
		t.Fatalf("got %d comments, want %d (all pages plus the reply)", len(issue.Comments), len(comments)+1)
	}
	adaptertest.AssertCommentsAscending(t, issue.Comments)
	last := issue.Comments[len(issue.Comments)-1]
	if last.ID != "r1" || last.Author != "cy" || last.AuthorID != "3" {
		t.Errorf("newest comment = %+v, want the reply r1 by cy", last)
	}
	if !slices.ContainsFunc(fake.calls(), func(c string) bool { return strings.Contains(c, "start_id=") }) {
		t.Error("comment pages after the first were not requested with start_id")
	}
}

func TestFetchIssueByIDNotFound(t *testing.T) {
	t.Parallel()

	_, srv := newFake(t)
	adapter := newTestAdapter(t, srv, nil)

	_, err := adapter.FetchIssueByID(context.Background(), "missing")
	adaptertest.AssertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
}

func TestFetchIssueBlockers(t *testing.T) {
	t.Parallel()

	_, srv := newFake(t,
		&fakeTask{ID: "b1", Name: "Blocker", Status: "in progress"},
		&fakeTask{ID: "a1", Name: "Work", Status: "to do", WaitsOn: []string{"b1"}},
		&fakeTask{ID: "a2", Name: "Free", Status: "to do"},
	)
	adapter := newTestAdapter(t, srv, nil)

	blockers, err := adapter.FetchIssueBlockers(context.Background(), "a1")
	if err != nil {
		t.Fatalf("FetchIssueBlockers: %v", err)
	}
	if !slices.Equal(blockers, []domain.BlockerRef{{ID: "b1", Identifier: "b1", State: "in progress"}}) {
		t.Errorf("blockers = %+v", blockers)
	}

	none, err := adapter.FetchIssueBlockers(context.Background(), "a2")
	if err != nil {
		t.Fatalf("FetchIssueBlockers: %v", err)
	}
	adaptertest.AssertEmptyNonNil(t, none, "FetchIssueBlockers")
}

func TestFetchIssueStates(t *testing.T) {
	t.Parallel()

	fake, srv := newFake(t,
		&fakeTask{ID: "a1", CustomID: "ENG-1", Name: "One", Status: "to do", Tags: []string{"Stage-Plan"}},
		&fakeTask{ID: "a2", Name: "Two", Status: "complete"},
	)
	adapter := newTestAdapter(t, srv, map[string]any{"team_id": "9012"})
	ctx := context.Background()

	requested := []string{"a1", "a2", "missing"}
	states, err := adapter.FetchIssueStatesByIDs(ctx, requested)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	adaptertest.AssertStateMapOmitsMissing(t, requested, states)
	if states["a1"] != "to do" || states["a2"] != "complete" || len(states) != 2 {
		t.Errorf("states = %v", states)
	}

	withLabels, err := adapter.FetchIssueStatesAndLabelsByIDs(ctx, requested)
	if err != nil {
		t.Fatalf("FetchIssueStatesAndLabelsByIDs: %v", err)
	}
	if got := withLabels["a1"]; got.State != "to do" || !slices.Equal(got.Labels, []string{"stage-plan"}) {
		t.Errorf("a1 = %+v", got)
	}
	if _, ok := withLabels["missing"]; ok {
		t.Error("a missing task must be omitted")
	}

	byIdentifier, err := adapter.FetchIssueStatesByIdentifiers(ctx, []string{"ENG-1", "a2", "ENG-404"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}
	if byIdentifier["ENG-1"] != "to do" || byIdentifier["a2"] != "complete" || len(byIdentifier) != 2 {
		t.Errorf("by identifier = %v", byIdentifier)
	}
	if !slices.ContainsFunc(fake.calls(), func(c string) bool {
		return strings.Contains(c, "/task/ENG-1?") && strings.Contains(c, "custom_task_ids=true") && strings.Contains(c, "team_id=9012")
	}) {
		t.Error("a custom task ID was not looked up with custom_task_ids and team_id")
	}
}

func TestFetchIssueStatesEmptyInputSendsNothing(t *testing.T) {
	t.Parallel()

	fake, srv := newFake(t)
	adapter := newTestAdapter(t, srv, nil)
	ctx := context.Background()

	if got, err := adapter.FetchIssueStatesByIDs(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("FetchIssueStatesByIDs(nil) = %v, %v", got, err)
	}
	if got, err := adapter.FetchIssueStatesAndLabelsByIDs(ctx, nil); err != nil || got == nil || len(got) != 0 {
		t.Errorf("FetchIssueStatesAndLabelsByIDs(nil) = %v, %v", got, err)
	}
	if got, err := adapter.FetchIssueStatesByIdentifiers(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("FetchIssueStatesByIdentifiers(nil) = %v, %v", got, err)
	}
	adaptertest.AssertNoRequestOnEmptyInput(t, len(fake.calls()), "state reads")
}

func TestFetchIssueComments(t *testing.T) {
	t.Parallel()

	_, srv := newFake(t, &fakeTask{ID: "a1", Name: "Quiet", Status: "to do"})
	adapter := newTestAdapter(t, srv, nil)

	comments, err := adapter.FetchIssueComments(context.Background(), "a1")
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	adaptertest.AssertEmptyNonNil(t, comments, "FetchIssueComments")

	_, err = adapter.FetchIssueComments(context.Background(), "missing")
	adaptertest.AssertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
}

func TestCommentIssue(t *testing.T) {
	t.Parallel()

	for _, numeric := range []bool{false, true} {
		t.Run(map[bool]string{false: "string id", true: "numeric id"}[numeric], func(t *testing.T) {
			t.Parallel()

			fake, srv := newFake(t, &fakeTask{ID: "a1", Name: "Work", Status: "to do", Comments: manyComments(2)})
			fake.numericCommentIDs = numeric
			adapter := newTestAdapter(t, srv, nil)
			ctx := context.Background()

			id, err := adapter.CommentIssue(ctx, "a1", "Sortie picked this up")
			if err != nil {
				t.Fatalf("CommentIssue: %v", err)
			}
			comments, err := adapter.FetchIssueComments(ctx, "a1")
			if err != nil {
				t.Fatalf("FetchIssueComments: %v", err)
			}
			adaptertest.AssertPostedCommentListed(t, id, comments)
		})
	}
}

func TestTransitionIssue(t *testing.T) {
	t.Parallel()

	fake, srv := newFake(t, &fakeTask{ID: "a1", Name: "Work", Status: "to do"})
	adapter := newTestAdapter(t, srv, nil)
	ctx := context.Background()

	if err := adapter.TransitionIssue(ctx, "a1", "In Review"); err != nil {
		t.Fatalf("TransitionIssue: %v", err)
	}
	if got := fake.task("a1").Status; got != "in review" {
		t.Errorf("status = %q, want in review", got)
	}

	adaptertest.AssertTrackerErrorKind(t, adapter.TransitionIssue(ctx, "a1", "no such status"), domain.ErrTrackerPayload)
	adaptertest.AssertTrackerErrorKind(t, adapter.TransitionIssue(ctx, "missing", "complete"), domain.ErrTrackerNotFound)

	fake.ignoreStatus = true
	adaptertest.AssertTrackerErrorKind(t, adapter.TransitionIssue(ctx, "a1", "complete"), domain.ErrTrackerPayload)
}

func TestLabels(t *testing.T) {
	t.Parallel()

	fake, srv := newFake(t, &fakeTask{ID: "a1", Name: "Work", Status: "to do", Tags: []string{"bug"}})
	adapter := newTestAdapter(t, srv, nil)
	ctx := context.Background()

	before, err := adapter.FetchIssueByID(ctx, "a1")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}
	if err := adapter.AddLabel(ctx, "a1", "Stage-Plan"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	after, err := adapter.FetchIssueByID(ctx, "a1")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}
	adaptertest.AssertLabelAddIsAdditive(t, before.Labels, after.Labels, "stage-plan")
	candidates, err := adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	adaptertest.AssertLabelVisible(t, "stage-plan", true, after, candidates)

	if err := adapter.RemoveLabel(ctx, "a1", "STAGE-PLAN"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	removed, err := adapter.FetchIssueByID(ctx, "a1")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}
	candidates, err = adapter.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	adaptertest.AssertLabelVisible(t, "stage-plan", false, removed, candidates)
	if !slices.Contains(removed.Labels, "bug") {
		t.Error("RemoveLabel removed another label")
	}

	writes := 0
	for _, c := range fake.calls() {
		if strings.HasPrefix(c, "DELETE") {
			writes++
		}
	}
	if err := adapter.RemoveLabel(ctx, "a1", "absent"); err != nil {
		t.Fatalf("RemoveLabel(absent): %v", err)
	}
	for _, c := range fake.calls() {
		if strings.HasPrefix(c, "DELETE") {
			writes--
		}
	}
	if writes != 0 {
		t.Error("removing a label the task does not carry sent a write")
	}

	adaptertest.AssertTrackerErrorKind(t, adapter.AddLabel(ctx, "a1", "  "), domain.ErrTrackerPayload)

	fake.spaceTags = []string{"bug"}
	adaptertest.AssertTrackerErrorKind(t, adapter.AddLabel(ctx, "a1", "unknown-tag"), domain.ErrTrackerPayload)
}

func TestErrorClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status int
		body   string
		want   domain.TrackerErrorKind
	}{
		{http.StatusUnauthorized, `{"err":"Token invalid","ECODE":"OAUTH_025"}`, domain.ErrTrackerAuth},
		{http.StatusUnauthorized, `{"err":"Task not found, deleted","ECODE":"ITEM_013"}`, domain.ErrTrackerNotFound},
		{http.StatusNotFound, `{}`, domain.ErrTrackerNotFound},
		{http.StatusTooManyRequests, `{"err":"Rate limit reached"}`, domain.ErrTrackerAPI},
		{http.StatusBadGateway, `bad gateway`, domain.ErrTrackerTransport},
		{http.StatusBadRequest, `{"err":"bad"}`, domain.ErrTrackerPayload},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status)+tt.body, func(t *testing.T) {
			t.Parallel()

			fake, srv := newFake(t)
			fake.failWith = tt.status
			fake.failBody = tt.body
			adapter := newTestAdapter(t, srv, nil)

			_, err := adapter.FetchCandidateIssues(context.Background())
			if errorKind(err) != tt.want {
				t.Errorf("error = %v, want kind %q", err, tt.want)
			}
		})
	}
}

func TestProjectURL(t *testing.T) {
	t.Parallel()

	_, srv := newFake(t)
	if got := newTestAdapter(t, srv, nil).ProjectURL(); got != "" {
		t.Errorf("ProjectURL without team_id = %q, want empty", got)
	}
	if got := newTestAdapter(t, srv, map[string]any{"team_id": "9012"}).ProjectURL(); got != "https://app.clickup.com/9012/v/li/901" {
		t.Errorf("ProjectURL = %q", got)
	}
}
