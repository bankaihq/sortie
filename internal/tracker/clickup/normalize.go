package clickup

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
)

// clickupTask is the subset of a ClickUp task the adapter reads, as the
// list-tasks and get-task routes return it.
type clickupTask struct {
	ID                  string          `json:"id"`
	CustomID            string          `json:"custom_id"`
	Name                string          `json:"name"`
	Description         string          `json:"description"`
	MarkdownDescription string          `json:"markdown_description"`
	Status              clickupStatus   `json:"status"`
	Tags                []clickupTag    `json:"tags"`
	Priority            *clickupPrio    `json:"priority"`
	Assignees           []clickupUser   `json:"assignees"`
	URL                 string          `json:"url"`
	Parent              string          `json:"parent"`
	DateCreated         string          `json:"date_created"`
	DateUpdated         string          `json:"date_updated"`
	Dependencies        json.RawMessage `json:"dependencies"`
}

type clickupStatus struct {
	Status string `json:"status"`
	Type   string `json:"type"`
}

type clickupTag struct {
	Name string `json:"name"`
}

type clickupPrio struct {
	ID       string `json:"id"`
	Priority string `json:"priority"`
}

type clickupUser struct {
	ID       flexID `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

// clickupDependency is one entry of a task's dependencies array. An entry
// whose TaskID is the task itself names a task it waits on in DependsOn;
// an entry whose DependsOn is the task itself names a task waiting on it.
type clickupDependency struct {
	TaskID    string `json:"task_id"`
	DependsOn string `json:"depends_on"`
}

type clickupTaskList struct {
	Tasks    []clickupTask `json:"tasks"`
	LastPage *bool         `json:"last_page"`
}

type clickupComment struct {
	ID          flexID      `json:"id"`
	CommentText string      `json:"comment_text"`
	User        clickupUser `json:"user"`
	Date        json.Number `json:"date"`
	ReplyCount  json.Number `json:"reply_count"`
}

type clickupCommentList struct {
	Comments []clickupComment `json:"comments"`
}

// clickupCommentCreated is the create-comment response. ClickUp has sent
// the id both as a number and as a string, so it decodes either.
type clickupCommentCreated struct {
	ID flexID `json:"id"`
}

// flexID is an identifier ClickUp sends as a JSON string or a JSON number.
// It holds the text of either; null and an absent field leave it empty.
type flexID string

// UnmarshalJSON accepts a string, a number, or null.
func (f *flexID) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*f = ""
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = flexID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*f = flexID(n.String())
	return nil
}

// String returns the identifier text.
func (f flexID) String() string { return string(f) }

// identifier returns the human-readable key of a task: its custom task ID
// when the workspace assigns one, otherwise the task ID.
func identifier(t clickupTask) string {
	if t.CustomID != "" {
		return t.CustomID
	}
	return t.ID
}

// normalizeTask maps a ClickUp task to a [domain.Issue]. Comments are nil
// (not fetched). The blocker fields follow [waitingOn]: a task whose payload
// proves it waits on nothing gets an empty authoritative list, any other
// task is marked unresolved for the per-issue blocker read.
func normalizeTask(t clickupTask) domain.Issue {
	tags := make([]string, 0, len(t.Tags))
	for _, tag := range t.Tags {
		tags = append(tags, tag.Name)
	}

	description := t.MarkdownDescription
	if description == "" {
		description = t.Description
	}

	var priority *int
	if t.Priority != nil {
		priority = issuekit.ParsePriorityIntFromString(t.Priority.ID)
	}

	var assignee string
	if len(t.Assignees) > 0 {
		assignee = t.Assignees[0].Username
		if assignee == "" {
			assignee = t.Assignees[0].Email
		}
	}

	var parent *domain.ParentRef
	if t.Parent != "" {
		parent = &domain.ParentRef{ID: t.Parent, Identifier: t.Parent}
	}

	issue := domain.Issue{
		ID:          t.ID,
		Identifier:  identifier(t),
		Title:       t.Name,
		Description: description,
		Priority:    priority,
		State:       t.Status.Status,
		URL:         t.URL,
		Labels:      issuekit.NormalizeLabels(tags),
		Assignee:    assignee,
		Parent:      parent,
		BlockedBy:   []domain.BlockerRef{},
		CreatedAt:   msToRFC3339(t.DateCreated),
		UpdatedAt:   msToRFC3339(t.DateUpdated),
	}

	if blockers, known := waitingOn(t); !known || len(blockers) > 0 {
		issue.BlockersUnresolved = true
	}
	return issue
}

// waitingOn returns the IDs of the tasks t waits on, in payload order and
// each once. known is false when the payload carries no dependencies array
// at all or one that does not decode, so the caller cannot tell whether
// the task waits on anything.
func waitingOn(t clickupTask) (ids []string, known bool) {
	raw := strings.TrimSpace(string(t.Dependencies))
	if raw == "" || raw == "null" {
		return nil, false
	}
	var deps []clickupDependency
	if err := json.Unmarshal(t.Dependencies, &deps); err != nil {
		return nil, false
	}
	seen := make(map[string]struct{}, len(deps))
	ids = make([]string, 0, len(deps))
	for _, d := range deps {
		if d.TaskID != t.ID || d.DependsOn == "" || d.DependsOn == t.ID {
			continue
		}
		if _, dup := seen[d.DependsOn]; dup {
			continue
		}
		seen[d.DependsOn] = struct{}{}
		ids = append(ids, d.DependsOn)
	}
	return ids, true
}

// normalizeComment maps a ClickUp comment to the adapter-local staging
// shape. ClickUp reports times as Unix milliseconds, which become RFC 3339
// UTC text. ClickUp reports no edit time and no account kind.
func normalizeComment(c clickupComment) issuekit.SourceComment {
	author := c.User.Username
	if author == "" {
		author = c.User.Email
	}
	return issuekit.SourceComment{
		ID:        c.ID.String(),
		Author:    author,
		AuthorID:  c.User.ID.String(),
		Body:      c.CommentText,
		CreatedAt: msToRFC3339(c.Date.String()),
	}
}

// msToRFC3339 converts a Unix-milliseconds string to RFC 3339 UTC text with
// millisecond precision. Empty or non-numeric input yields "".
func msToRFC3339(ms string) string {
	if ms == "" {
		return ""
	}
	n, err := strconv.ParseInt(ms, 10, 64)
	if err != nil {
		return ""
	}
	return time.UnixMilli(n).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}
