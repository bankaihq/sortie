package clickup

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

const testToken = "pk_test_token"

// fakeTask is a task the fake server holds.
type fakeTask struct {
	ID       string
	CustomID string
	Name     string
	Status   string
	Tags     []string
	Priority string
	Parent   string
	// WaitsOn lists the IDs of the tasks this one waits on. A nil
	// WaitsOn with OmitDeps set leaves the dependencies field out.
	WaitsOn  []string
	OmitDeps bool
	Comments []fakeComment
}

// fakeComment is a top-level comment and its replies.
type fakeComment struct {
	ID      string
	Text    string
	User    string
	UserID  int
	DateMS  int64
	Replies []fakeComment
}

// fakeClickUp is an in-memory ClickUp v2 API covering the routes the
// adapter calls. Requests are recorded for assertions.
type fakeClickUp struct {
	t      *testing.T
	listID string

	mu        sync.Mutex
	tasks     []*fakeTask
	spaceTags []string
	requests  []string
	nextID    int
	// numericCommentIDs makes comment create answer with a number id.
	numericCommentIDs bool
	// ignoreStatus makes a status update leave the status unchanged.
	ignoreStatus bool
	// failWith answers every request with this status when non-zero.
	failWith int
	failBody string
}

func newFake(t *testing.T, tasks ...*fakeTask) (*fakeClickUp, *httptest.Server) {
	t.Helper()
	f := &fakeClickUp{t: t, listID: "901", tasks: tasks, nextID: 5000}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeClickUp) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeClickUp) task(id string) *fakeTask {
	for _, t := range f.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (f *fakeClickUp) taskByCustomID(id string) *fakeTask {
	for _, t := range f.tasks {
		if t.CustomID == id {
			return t
		}
	}
	return nil
}

func (f *fakeClickUp) taskJSON(t *fakeTask) map[string]any {
	tags := make([]map[string]any, len(t.Tags))
	for i, name := range t.Tags {
		tags[i] = map[string]any{"name": name}
	}
	m := map[string]any{
		"id":                   t.ID,
		"custom_id":            nil,
		"name":                 t.Name,
		"description":          "plain " + t.Name,
		"markdown_description": "**" + t.Name + "**",
		"status":               map[string]any{"status": t.Status, "type": "custom"},
		"tags":                 tags,
		"priority":             nil,
		"assignees":            []map[string]any{{"id": 7, "username": "ada", "email": "ada@example.com"}},
		"url":                  "https://app.clickup.com/t/" + t.ID,
		"parent":               nil,
		"date_created":         "1700000000000",
		"date_updated":         "1700000100000",
	}
	if t.CustomID != "" {
		m["custom_id"] = t.CustomID
	}
	if t.Priority != "" {
		m["priority"] = map[string]any{"id": t.Priority, "priority": "high"}
	}
	if t.Parent != "" {
		m["parent"] = t.Parent
	}
	if !t.OmitDeps {
		deps := make([]map[string]any, 0, len(t.WaitsOn)+1)
		for _, other := range t.WaitsOn {
			deps = append(deps, map[string]any{"task_id": t.ID, "depends_on": other, "type": 1})
		}
		// A task waiting on this one appears with the roles swapped.
		for _, other := range f.tasks {
			if slices.Contains(other.WaitsOn, t.ID) {
				deps = append(deps, map[string]any{"task_id": other.ID, "depends_on": t.ID, "type": 0})
			}
		}
		m["dependencies"] = deps
	}
	return m
}

func commentJSON(c fakeComment) map[string]any {
	return map[string]any{
		"id":           c.ID,
		"comment_text": c.Text,
		"user":         map[string]any{"id": c.UserID, "username": c.User, "email": c.User + "@example.com"},
		"date":         strconv.FormatInt(c.DateMS, 10),
		"reply_count":  len(c.Replies),
	}
}

func (f *fakeClickUp) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("encode response: %v", err)
	}
}

func (f *fakeClickUp) notFound(w http.ResponseWriter) {
	f.writeJSON(w, http.StatusUnauthorized, map[string]string{"err": "Task not found, deleted", "ECODE": "ITEM_013"})
}

func (f *fakeClickUp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())

	if got := r.Header.Get("Authorization"); got != testToken {
		f.writeJSON(w, http.StatusUnauthorized, map[string]string{"err": "Token invalid", "ECODE": "OAUTH_025"})
		return
	}
	if f.failWith != 0 {
		w.WriteHeader(f.failWith)
		_, _ = w.Write([]byte(f.failBody))
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 3 && parts[0] == "list" && parts[2] == "task" && r.Method == http.MethodGet:
		f.listTasks(w, r, parts[1])
	case len(parts) == 2 && parts[0] == "task" && r.Method == http.MethodGet:
		f.getTask(w, r, parts[1])
	case len(parts) == 2 && parts[0] == "task" && r.Method == http.MethodPut:
		f.updateTask(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "task" && parts[2] == "comment" && r.Method == http.MethodGet:
		f.listComments(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "task" && parts[2] == "comment" && r.Method == http.MethodPost:
		f.createComment(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "comment" && parts[2] == "reply" && r.Method == http.MethodGet:
		f.listReplies(w, parts[1])
	case len(parts) == 4 && parts[0] == "task" && parts[2] == "tag":
		f.editTag(w, r, parts[1], parts[3])
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeClickUp) listTasks(w http.ResponseWriter, r *http.Request, listID string) {
	if listID != f.listID {
		f.writeJSON(w, http.StatusNotFound, map[string]string{"err": "List not found", "ECODE": "SUBCAT_016"})
		return
	}
	q := r.URL.Query()
	statuses := q["statuses[]"]
	tags := q["tags[]"]
	var matched []*fakeTask
	for _, t := range f.tasks {
		if len(statuses) > 0 && !slices.ContainsFunc(statuses, func(s string) bool { return strings.EqualFold(s, t.Status) }) {
			continue
		}
		if len(tags) > 0 && !slices.ContainsFunc(tags, func(tag string) bool { return slices.Contains(t.Tags, tag) }) {
			continue
		}
		matched = append(matched, t)
	}
	page, _ := strconv.Atoi(q.Get("page"))
	start := min(page*taskPageSize, len(matched))
	end := min(start+taskPageSize, len(matched))
	out := make([]map[string]any, 0, end-start)
	for _, t := range matched[start:end] {
		out = append(out, f.taskJSON(t))
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"tasks": out, "last_page": end == len(matched)})
}

func (f *fakeClickUp) lookup(r *http.Request, key string) *fakeTask {
	q := r.URL.Query()
	if q.Get("custom_task_ids") == "true" {
		if q.Get("team_id") == "" {
			f.t.Errorf("custom_task_ids without team_id for %s", key)
		}
		return f.taskByCustomID(key)
	}
	return f.task(key)
}

func (f *fakeClickUp) getTask(w http.ResponseWriter, r *http.Request, key string) {
	t := f.lookup(r, key)
	if t == nil {
		f.notFound(w)
		return
	}
	f.writeJSON(w, http.StatusOK, f.taskJSON(t))
}

func (f *fakeClickUp) updateTask(w http.ResponseWriter, r *http.Request, id string) {
	t := f.task(id)
	if t == nil {
		f.notFound(w)
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode update: %v", err)
	}
	if body.Status == "no such status" {
		f.writeJSON(w, http.StatusBadRequest, map[string]string{"err": "Status does not exist", "ECODE": "CRTSK_001"})
		return
	}
	if !f.ignoreStatus {
		t.Status = strings.ToLower(body.Status)
	}
	f.writeJSON(w, http.StatusOK, f.taskJSON(t))
}

func (f *fakeClickUp) listComments(w http.ResponseWriter, r *http.Request, id string) {
	t := f.task(id)
	if t == nil {
		f.notFound(w)
		return
	}
	// Newest first, commentPageSize per page, older pages by start_id.
	sorted := slices.Clone(t.Comments)
	slices.SortFunc(sorted, func(a, b fakeComment) int { return int(b.DateMS - a.DateMS) })
	if startID := r.URL.Query().Get("start_id"); startID != "" {
		idx := slices.IndexFunc(sorted, func(c fakeComment) bool { return c.ID == startID })
		sorted = sorted[idx+1:]
	}
	sorted = sorted[:min(commentPageSize, len(sorted))]
	out := make([]map[string]any, len(sorted))
	for i, c := range sorted {
		out[i] = commentJSON(c)
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"comments": out})
}

func (f *fakeClickUp) listReplies(w http.ResponseWriter, commentID string) {
	for _, t := range f.tasks {
		for _, c := range t.Comments {
			if c.ID == commentID {
				out := make([]map[string]any, len(c.Replies))
				for i, reply := range c.Replies {
					out[i] = commentJSON(reply)
				}
				f.writeJSON(w, http.StatusOK, map[string]any{"comments": out})
				return
			}
		}
	}
	f.writeJSON(w, http.StatusNotFound, map[string]string{"err": "Comment not found"})
}

func (f *fakeClickUp) createComment(w http.ResponseWriter, r *http.Request, id string) {
	t := f.task(id)
	if t == nil {
		f.notFound(w)
		return
	}
	var body struct {
		CommentText string `json:"comment_text"`
		NotifyAll   *bool  `json:"notify_all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode comment: %v", err)
	}
	if body.NotifyAll == nil || *body.NotifyAll {
		f.t.Errorf("comment create notify_all = %v, want false", body.NotifyAll)
	}
	f.nextID++
	c := fakeComment{ID: strconv.Itoa(f.nextID), Text: body.CommentText, User: "sortie", UserID: 1, DateMS: 1800000000000 + int64(f.nextID)}
	t.Comments = append(t.Comments, c)
	var idValue any = c.ID
	if f.numericCommentIDs {
		idValue = f.nextID
	}
	f.writeJSON(w, http.StatusOK, map[string]any{"id": idValue, "hist_id": "h1", "date": c.DateMS})
}

func (f *fakeClickUp) editTag(w http.ResponseWriter, r *http.Request, id, name string) {
	t := f.task(id)
	if t == nil {
		f.notFound(w)
		return
	}
	switch r.Method {
	case http.MethodPost:
		if f.spaceTags != nil && !slices.Contains(f.spaceTags, strings.ToLower(name)) {
			f.writeJSON(w, http.StatusBadRequest, map[string]string{"err": "Tag not found", "ECODE": "TAGS_001"})
			return
		}
		if !slices.Contains(t.Tags, strings.ToLower(name)) {
			t.Tags = append(t.Tags, strings.ToLower(name))
		}
	case http.MethodDelete:
		t.Tags = slices.DeleteFunc(t.Tags, func(tag string) bool { return tag == name })
	default:
		f.t.Errorf("unexpected tag method %s", r.Method)
	}
	f.writeJSON(w, http.StatusOK, map[string]any{})
}

// newTestAdapter builds an adapter against srv with the given extra config.
func newTestAdapter(t *testing.T, srv *httptest.Server, extra map[string]any) *ClickUpAdapter {
	t.Helper()
	cfg := map[string]any{
		"endpoint":        srv.URL,
		"api_key":         testToken,
		"project":         "901",
		"active_states":   []any{"to do", "in progress"},
		"terminal_states": []any{"complete"},
	}
	maps.Copy(cfg, extra)
	adapter, err := NewClickUpAdapter(cfg)
	if err != nil {
		t.Fatalf("NewClickUpAdapter: %v", err)
	}
	return adapter.(*ClickUpAdapter)
}

// manyComments returns n top-level comments with ascending dates.
func manyComments(n int) []fakeComment {
	out := make([]fakeComment, n)
	for i := range out {
		out[i] = fakeComment{ID: fmt.Sprintf("c%03d", i), Text: fmt.Sprintf("comment %d", i), User: "bob", UserID: 2, DateMS: 1700000000000 + int64(i)*1000}
	}
	return out
}

// errorKind returns the kind of a tracker error, or "" for another error.
func errorKind(err error) domain.TrackerErrorKind {
	if te, ok := err.(*domain.TrackerError); ok {
		return te.Kind
	}
	return ""
}
