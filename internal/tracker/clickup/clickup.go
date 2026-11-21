// Package clickup implements [domain.TrackerAdapter] for ClickUp. It reads
// the tasks of one ClickUp list through the v2 REST API, maps task statuses
// to tracker states and task tags to labels, and reads blockers from task
// dependencies per issue. Registered under kind "clickup" via an init
// function.
package clickup

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
	"github.com/sortie-ai/sortie/internal/issuekit"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Trackers.RegisterWithMeta("clickup", NewClickUpAdapter, registry.TrackerMeta{
		RequiresProject:       true,
		RequiresAPIKey:        true,
		ValidateTrackerConfig: validateConfig,
		BlockerSource:         registry.BlockersPerIssue,
	})
}

// Compile-time interface satisfaction check.
var (
	_ domain.TrackerAdapter = (*ClickUpAdapter)(nil)
	_ domain.BlockerReader  = (*ClickUpAdapter)(nil)
	_ domain.ProjectLinker  = (*ClickUpAdapter)(nil)
)

// defaultEndpoint is the ClickUp API base used when tracker.endpoint is
// empty.
const defaultEndpoint = "https://api.clickup.com/api/v2"

// taskPageSize is the number of tasks ClickUp returns per list-tasks page.
const taskPageSize = 100

// commentPageSize is the number of comments ClickUp returns per page of a
// task's comments.
const commentPageSize = 25

// maxTaskPages bounds one list read, so a list whose last_page flag never
// turns true cannot loop forever.
const maxTaskPages = 200

// customIDPattern matches the shape of a ClickUp custom task ID, such as
// "ENG-42".
var customIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)

// ClickUpAdapter implements [domain.TrackerAdapter] against one ClickUp
// list. Its configuration is immutable after construction. Safe for
// concurrent use.
type ClickUpAdapter struct {
	*issuekit.StateSource
	client      *httpkit.Client
	listID      string
	teamID      string
	queryFilter url.Values
	metrics     domain.Metrics // nil-safe: check before calling
}

// NewClickUpAdapter creates a [ClickUpAdapter] from adapter configuration.
// Required config keys: "api_key" (a personal token, pk_...) and "project"
// (the list ID). Optional: "endpoint" (default the ClickUp v2 API),
// "active_states", "query_filter" (a URL query fragment for the
// list-tasks route), and "team_id" (the workspace ID, which lets custom
// task IDs such as "ENG-42" be looked up and gives the project a web
// address).
func NewClickUpAdapter(config map[string]any) (domain.TrackerAdapter, error) {
	endpoint, fault := typeutil.StringField(config, "endpoint")
	if fault != nil {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
	}
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	endpoint = strings.TrimRight(endpoint, "/")
	if !validEndpoint(endpoint) {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("endpoint %q must be a URL with a scheme and host", httpkit.RedactURLUserinfo(endpoint)),
		}
	}

	apiKey, fault := typeutil.StringField(config, "api_key")
	if fault != nil {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
	}
	if apiKey == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerAPIKey,
			Message: "missing required config key: api_key",
		}
	}

	listID, err := idField(config, "project")
	if err != nil {
		return nil, err
	}
	if listID == "" {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrMissingTrackerProject,
			Message: "missing required config key: project",
		}
	}

	teamID, err := idField(config, "team_id")
	if err != nil {
		return nil, err
	}

	states, err := issuekit.TrackerStatesFromConfig(config)
	if err != nil {
		return nil, err
	}

	rawFilter, fault := typeutil.StringField(config, "query_filter")
	if fault != nil {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
	}
	queryFilter, err := parseQueryFilter(rawFilter)
	if err != nil {
		return nil, err
	}

	userAgent, fault := typeutil.StringField(config, "user_agent")
	if fault != nil {
		return nil, &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: fault.Error()}
	}
	if userAgent == "" {
		userAgent = "sortie/dev"
	}

	return &ClickUpAdapter{
		StateSource: issuekit.NewStateSource(states),
		client:      newClickUpClient(endpoint, apiKey, userAgent),
		listID:      listID,
		teamID:      teamID,
		queryFilter: queryFilter,
	}, nil
}

// idField reads a ClickUp ID from config. YAML decodes an unquoted numeric
// ID as a number, so a string, an int, or a whole-number float64 is
// accepted. An absent key yields "".
func idField(config map[string]any, key string) (string, error) {
	switch v := config[key].(type) {
	case nil:
		return "", nil
	case string:
		return strings.TrimSpace(v), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10), nil
		}
	}
	return "", &domain.TrackerError{
		Kind:    domain.ErrTrackerPayload,
		Message: fmt.Sprintf("%s must be a ClickUp ID, got %s", key, typeutil.DescribeYAMLType(config[key])),
	}
}

// validEndpoint reports whether endpoint parses as a URL carrying both a
// scheme and a host.
func validEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	return err == nil && parsed.Scheme != "" && parsed.Host != ""
}

// ProjectURL returns the web address of the configured list. It needs the
// workspace ID, so it is empty when team_id is not configured.
func (a *ClickUpAdapter) ProjectURL() string {
	if a.teamID == "" {
		return ""
	}
	return "https://app.clickup.com/" + url.PathEscape(a.teamID) + "/v/li/" + url.PathEscape(a.listID)
}

// SetMetrics configures the metrics recorder for tracker API call
// instrumentation. When not called or called with nil, the adapter
// operates without recording metrics. Not safe to call concurrently with
// adapter operations.
func (a *ClickUpAdapter) SetMetrics(m domain.Metrics) {
	a.metrics = m
}

// FetchCandidateIssues returns the list's tasks in a configured active
// state, narrowed by query_filter. Comments are nil. An empty active list
// returns no issue and sends no request.
func (a *ClickUpAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	active := a.TrackerStates().Active
	if len(active) == 0 {
		return []domain.Issue{}, nil
	}

	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_candidates", func() error {
		var fetchErr error
		issues, fetchErr = a.listTasks(ctx, active, a.queryFilter)
		return fetchErr
	})
	return issues, err
}

// FetchIssuesByStates returns the list's tasks in the given states,
// compared case-insensitively. query_filter is not applied. Returns
// immediately when states is empty.
func (a *ClickUpAdapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	if len(states) == 0 {
		return []domain.Issue{}, nil
	}

	issues := make([]domain.Issue, 0)
	err := trackermetrics.Track(a.metrics, "fetch_by_states", func() error {
		var fetchErr error
		issues, fetchErr = a.listTasks(ctx, states, nil)
		return fetchErr
	})
	return issues, err
}

// listTasks reads every page of the list's tasks in the given statuses and
// keeps those whose status names one of them, ignoring letter case.
// Closed tasks and subtasks are included, so a closed status can be read.
func (a *ClickUpAdapter) listTasks(ctx context.Context, states []string, filter url.Values) ([]domain.Issue, error) {
	issues := make([]domain.Issue, 0)
	path := "/list/" + url.PathEscape(a.listID) + "/task"

	for page := 0; ; page++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if page == maxTaskPages {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf("list %s has more than %d pages of tasks in the requested states", a.listID, maxTaskPages),
			}
		}

		params := url.Values{}
		for k, v := range filter {
			params[k] = slices.Clone(v)
		}
		for _, s := range states {
			params.Add("statuses[]", s)
		}
		params.Set("include_closed", "true")
		params.Set("subtasks", "true")
		params.Set("archived", "false")
		params.Set("include_markdown_description", "true")
		params.Set("page", strconv.Itoa(page))

		body, _, err := a.client.Get(ctx, path, params)
		if err != nil {
			return nil, err
		}

		var resp clickupTaskList
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse list tasks response",
				Err:     err,
			}
		}

		for _, t := range resp.Tasks {
			if !stateIn(t.Status.Status, states) {
				continue
			}
			issues = append(issues, normalizeTask(t))
		}

		lastPage := len(resp.Tasks) < taskPageSize
		if resp.LastPage != nil {
			lastPage = *resp.LastPage
		}
		if lastPage || len(resp.Tasks) == 0 {
			return issues, nil
		}
	}
}

// stateIn reports whether state names one of states, ignoring letter case.
func stateIn(state string, states []string) bool {
	return slices.ContainsFunc(states, func(s string) bool { return strings.EqualFold(s, state) })
}

// FetchIssueByID returns a fully populated issue: the task, its resolved
// blockers, and its comments with their replies, oldest first. issueID is
// the ClickUp task ID.
func (a *ClickUpAdapter) FetchIssueByID(ctx context.Context, issueID string) (domain.Issue, error) {
	var issue domain.Issue
	err := trackermetrics.Track(a.metrics, "fetch_issue", func() error {
		task, err := a.getTask(ctx, issueID, nil)
		if err != nil {
			return err
		}
		fetched := normalizeTask(task)

		blockers, err := a.resolveBlockers(ctx, task)
		if err != nil {
			return err
		}
		fetched.BlockedBy = blockers
		fetched.BlockersUnresolved = false

		comments, err := a.fetchComments(ctx, issueID)
		if err != nil {
			return err
		}
		fetched.Comments = comments

		issue = fetched
		return nil
	})
	return issue, err
}

// getTask reads one task. A missing task returns a
// [domain.ErrTrackerNotFound] error naming key.
func (a *ClickUpAdapter) getTask(ctx context.Context, key string, params url.Values) (clickupTask, error) {
	if params == nil {
		params = url.Values{}
	}
	params.Set("include_markdown_description", "true")

	body, _, err := a.client.Get(ctx, "/task/"+url.PathEscape(key), params)
	if err != nil {
		if domain.IsNotFound(err) {
			return clickupTask{}, &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("issue not found: %s", key),
				Err:     err,
			}
		}
		return clickupTask{}, err
	}

	var task clickupTask
	if err := json.Unmarshal(body, &task); err != nil {
		return clickupTask{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: fmt.Sprintf("failed to parse task response for %s", key),
			Err:     err,
		}
	}
	return task, nil
}

// FetchIssueBlockers returns the tasks the issue waits on, read from its
// dependencies, each with its current status. A dependency that no longer
// exists is left out.
func (a *ClickUpAdapter) FetchIssueBlockers(ctx context.Context, issueID string) ([]domain.BlockerRef, error) {
	blockers := make([]domain.BlockerRef, 0)
	err := trackermetrics.Track(a.metrics, "fetch_blockers", func() error {
		task, err := a.getTask(ctx, issueID, nil)
		if err != nil {
			return err
		}
		resolved, err := a.resolveBlockers(ctx, task)
		if err != nil {
			return err
		}
		blockers = resolved
		return nil
	})
	return blockers, err
}

// resolveBlockers reads each task t waits on, in dependency order. A
// payload without a dependencies array is read as waiting on nothing,
// because the get-task route always carries the array when there is one.
func (a *ClickUpAdapter) resolveBlockers(ctx context.Context, t clickupTask) ([]domain.BlockerRef, error) {
	ids, _ := waitingOn(t)
	blockers := make([]domain.BlockerRef, 0, len(ids))
	for _, id := range ids {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		blocker, err := a.getTask(ctx, id, nil)
		if err != nil {
			if domain.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		blockers = append(blockers, domain.BlockerRef{
			ID:         blocker.ID,
			Identifier: identifier(blocker),
			State:      blocker.Status.Status,
		})
	}
	return blockers, nil
}

// FetchIssueStatesByIDs returns the current status of each requested task
// ID. Tasks that do not exist are omitted.
func (a *ClickUpAdapter) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	if len(issueIDs) == 0 {
		return map[string]string{}, nil
	}

	var states map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_ids", func() error {
		reads, err := a.readStates(ctx, issueIDs, a.taskParams)
		if err != nil {
			return err
		}
		states = issuekit.StatesOf(reads)
		return nil
	})
	return states, err
}

// FetchIssueStatesAndLabelsByIDs returns the current status and tags of
// each requested task ID. It sends the same requests as
// FetchIssueStatesByIDs. Tasks that do not exist are omitted.
func (a *ClickUpAdapter) FetchIssueStatesAndLabelsByIDs(ctx context.Context, issueIDs []string) (map[string]domain.IssueStateAndLabels, error) {
	if len(issueIDs) == 0 {
		return map[string]domain.IssueStateAndLabels{}, nil
	}

	var reads map[string]domain.IssueStateAndLabels
	err := trackermetrics.Track(a.metrics, "fetch_states_by_ids", func() error {
		var fetchErr error
		reads, fetchErr = a.readStates(ctx, issueIDs, a.taskParams)
		return fetchErr
	})
	if err != nil {
		return nil, err
	}
	return reads, nil
}

// FetchIssueStatesByIdentifiers returns the current status of each
// requested identifier. An identifier shaped like a custom task ID is
// looked up as one when team_id is configured; any other is read as a
// task ID. Tasks that do not exist are omitted.
func (a *ClickUpAdapter) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error) {
	if len(identifiers) == 0 {
		return map[string]string{}, nil
	}

	var states map[string]string
	err := trackermetrics.Track(a.metrics, "fetch_states_by_identifiers", func() error {
		reads, err := a.readStates(ctx, identifiers, a.identifierParams)
		if err != nil {
			return err
		}
		states = issuekit.StatesOf(reads)
		return nil
	})
	return states, err
}

// taskParams returns the query parameters for reading a task by ID.
func (a *ClickUpAdapter) taskParams(string) url.Values {
	return url.Values{}
}

// identifierParams returns the query parameters for reading a task by
// identifier: a custom task ID lookup when team_id is configured and the
// identifier has the custom ID shape.
func (a *ClickUpAdapter) identifierParams(key string) url.Values {
	params := url.Values{}
	if a.teamID != "" && customIDPattern.MatchString(key) {
		params.Set("custom_task_ids", "true")
		params.Set("team_id", a.teamID)
	}
	return params
}

// readStates reads each key's task concurrently and reports its status and
// tags, omitting keys whose task does not exist.
func (a *ClickUpAdapter) readStates(ctx context.Context, keys []string, params func(string) url.Values) (map[string]domain.IssueStateAndLabels, error) {
	return issuekit.ReadStates(ctx, keys, func(ctx context.Context, key string) (domain.IssueStateAndLabels, bool, error) {
		task, err := a.getTask(ctx, key, params(key))
		if err != nil {
			if domain.IsNotFound(err) {
				return domain.IssueStateAndLabels{}, false, nil
			}
			return domain.IssueStateAndLabels{}, false, err
		}
		issue := normalizeTask(task)
		return domain.IssueStateAndLabels{State: issue.State, Labels: issue.Labels}, true, nil
	})
}

// FetchIssueComments returns the task's comments and their replies, oldest
// first. Returns an empty non-nil slice when there are none.
func (a *ClickUpAdapter) FetchIssueComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	comments := make([]domain.Comment, 0)
	err := trackermetrics.Track(a.metrics, "fetch_comments", func() error {
		var fetchErr error
		comments, fetchErr = a.fetchComments(ctx, issueID)
		return fetchErr
	})
	return comments, err
}

// fetchComments reads every page of the task's top-level comments, newest
// first as ClickUp serves them, then the replies of each comment that has
// any, and returns them all oldest first.
func (a *ClickUpAdapter) fetchComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	path := "/task/" + url.PathEscape(issueID) + "/comment"
	var top []clickupComment
	params := url.Values{}

	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		page, err := a.getComments(ctx, path, params, issueID)
		if err != nil {
			return nil, err
		}
		top = append(top, page...)
		if len(page) < commentPageSize {
			break
		}
		oldest := page[len(page)-1]
		if params.Get("start_id") == oldest.ID.String() {
			break
		}
		params = url.Values{"start": {oldest.Date.String()}, "start_id": {oldest.ID.String()}}
	}

	all := make([]issuekit.SourceComment, 0, len(top))
	for _, c := range top {
		all = append(all, normalizeComment(c))
		if n, err := c.ReplyCount.Int64(); err != nil || n == 0 {
			continue
		}
		replies, err := a.getComments(ctx, "/comment/"+url.PathEscape(c.ID.String())+"/reply", nil, issueID)
		if err != nil {
			return nil, err
		}
		for _, r := range replies {
			all = append(all, normalizeComment(r))
		}
	}

	return orderComments(issuekit.NormalizeComments(all))
}

// getComments reads one page of comments from path. A missing task returns
// a [domain.ErrTrackerNotFound] error naming issueID.
func (a *ClickUpAdapter) getComments(ctx context.Context, path string, params url.Values, issueID string) ([]clickupComment, error) {
	body, _, err := a.client.Get(ctx, path, params)
	if err != nil {
		if domain.IsNotFound(err) {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerNotFound,
				Message: fmt.Sprintf("issue not found: %s", issueID),
				Err:     err,
			}
		}
		return nil, err
	}

	var resp clickupCommentList
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, &domain.TrackerError{
			Kind:    domain.ErrTrackerPayload,
			Message: "failed to parse comments response",
			Err:     err,
		}
	}
	return resp.Comments, nil
}

// orderComments keeps the first occurrence of each ID and sorts oldest
// created first, ties by ID. It always returns a non-nil slice.
func orderComments(comments []domain.Comment) ([]domain.Comment, error) {
	type datedComment struct {
		domain.Comment
		created time.Time
	}

	seen := make(map[string]struct{}, len(comments))
	dated := make([]datedComment, 0, len(comments))
	for _, c := range comments {
		if _, dup := seen[c.ID]; dup {
			continue
		}
		seen[c.ID] = struct{}{}

		created, err := c.CreatedTime()
		if err != nil {
			return nil, &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse comment created time",
				Err:     err,
			}
		}
		dated = append(dated, datedComment{Comment: c, created: created})
	}

	slices.SortFunc(dated, func(x, y datedComment) int {
		return cmp.Or(x.created.Compare(y.created), cmp.Compare(len(x.ID), len(y.ID)), strings.Compare(x.ID, y.ID))
	})
	ordered := make([]domain.Comment, len(dated))
	for i, d := range dated {
		ordered[i] = d.Comment
	}
	return ordered, nil
}

// TransitionIssue sets the task's status to targetState. ClickUp answers
// with the updated task, and a status that does not name targetState
// afterwards is reported as [domain.ErrTrackerPayload].
func (a *ClickUpAdapter) TransitionIssue(ctx context.Context, issueID string, targetState string) error {
	return trackermetrics.Track(a.metrics, "transition", func() error {
		payload, err := json.Marshal(map[string]string{"status": targetState})
		if err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to marshal status update",
				Err:     err,
			}
		}

		body, err := a.client.Send(ctx, "PUT", "/task/"+url.PathEscape(issueID), bytes.NewReader(payload))
		if err != nil {
			return err
		}

		var task clickupTask
		if err := json.Unmarshal(body, &task); err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf("failed to parse status update response for issue %s", issueID),
				Err:     err,
			}
		}
		if !strings.EqualFold(task.Status.Status, targetState) {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf("issue %s reads status %q after the update, want %q", issueID, task.Status.Status, targetState),
			}
		}
		return nil
	})
}

// CommentIssue posts text as a comment on the task without notifying its
// watchers, and returns the created comment's ID.
func (a *ClickUpAdapter) CommentIssue(ctx context.Context, issueID string, text string) (string, error) {
	var commentID string
	err := trackermetrics.Track(a.metrics, "comment", func() error {
		payload, err := json.Marshal(map[string]any{"comment_text": text, "notify_all": false})
		if err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to marshal comment request",
				Err:     err,
			}
		}

		body, err := a.client.Send(ctx, "POST", "/task/"+url.PathEscape(issueID)+"/comment", bytes.NewReader(payload))
		if err != nil {
			return err
		}

		var created clickupCommentCreated
		if err := json.Unmarshal(body, &created); err != nil {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "failed to parse comment create response",
				Err:     err,
			}
		}
		if created.ID.String() == "" {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: "comment create response carries no comment id",
			}
		}
		commentID = created.ID.String()
		return nil
	})
	if err != nil {
		return "", err
	}
	return commentID, nil
}

// AddLabel adds a tag to the task and confirms it with a read of the task.
// ClickUp adds tags that exist in the list's space; a tag missing there is
// reported as [domain.ErrTrackerPayload].
func (a *ClickUpAdapter) AddLabel(ctx context.Context, issueID string, label string) error {
	return trackermetrics.Track(a.metrics, "add_label", func() error {
		return issuekit.AddIssueLabel(ctx, label, a.labelOps(issueID))
	})
}

// RemoveLabel removes every tag of the task that names label, ignoring
// letter case, and confirms the removal with a read of the task. A task
// that carries none receives no write.
func (a *ClickUpAdapter) RemoveLabel(ctx context.Context, issueID string, label string) error {
	return trackermetrics.Track(a.metrics, "remove_label", func() error {
		return issuekit.RemoveIssueLabel(ctx, label, a.labelOps(issueID))
	})
}

// labelOps returns the ClickUp calls behind the shared label drivers.
// ClickUp answers a tag edit with an empty object, so every outcome is
// confirmed by a read.
func (a *ClickUpAdapter) labelOps(issueID string) issuekit.IssueLabelOps {
	tagPath := func(name string) string {
		return "/task/" + url.PathEscape(issueID) + "/tag/" + url.PathEscape(name)
	}
	return issuekit.IssueLabelOps{
		Read: func(ctx context.Context) ([]issuekit.IssueLabel, error) {
			task, err := a.getTask(ctx, issueID, nil)
			if err != nil {
				return nil, err
			}
			labels := make([]issuekit.IssueLabel, len(task.Tags))
			for i, tag := range task.Tags {
				labels[i] = issuekit.IssueLabel{Name: tag.Name}
			}
			return labels, nil
		},
		Add: func(ctx context.Context, label string) (issuekit.LabelWrite, error) {
			_, err := a.client.Send(ctx, "POST", tagPath(label), nil)
			return issuekit.LabelWrite{}, err
		},
		Remove: func(ctx context.Context, labels []issuekit.IssueLabel) (issuekit.LabelWrite, error) {
			var firstRejection error
			for _, l := range labels {
				_, err := a.client.Send(ctx, "DELETE", tagPath(l.Name), nil)
				if err == nil {
					continue
				}
				if !domain.IsNotFound(err) && !isPayload(err) {
					return issuekit.LabelWrite{}, err
				}
				if firstRejection == nil {
					firstRejection = err
				}
			}
			return issuekit.LabelWrite{}, firstRejection
		},
	}
}

// isPayload reports whether err is a tracker payload rejection.
func isPayload(err error) bool {
	te, ok := errors.AsType[*domain.TrackerError](err)
	return ok && te.Kind == domain.ErrTrackerPayload
}
