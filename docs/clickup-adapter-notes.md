# ClickUp adapter notes

Notes for a developer changing Sortie's ClickUp tracker adapter: the decisions it encodes, the places ClickUp's model does not line up with ours, and what to check against the live API.

Last updated: 2026-11-20

## Setup an operator writes

```yaml
tracker:
  kind: clickup
  api_key: $CLICKUP_TOKEN        # personal token, pk_...
  project: "901234567"           # list ID, the number after /li/ in the list's URL
  query_filter: "tags[]=agent-ready"
  active_states: [to do, in progress]
  in_progress_state: in progress
  handoff_state: review
  terminal_states: [complete]

clickup:
  team_id: "9012345"             # workspace ID; optional
```

`clickup.team_id` is optional. It turns on custom task ID lookups (`ENG-42`) for the by-identifier read and gives the project a web address. Everything else works without it.

## One list, statuses as states

The adapter reads one list. ClickUp statuses are list-scoped names with no transition graph, so they map onto Sortie's state names the way Jira statuses do, and `transition_issue` is a single task update. ClickUp stores status names in lower case and matches them without regard to case; the adapter compares case-insensitively too and passes the configured names through unchanged.

The configured names are not checked against the list at startup. A misspelled active status selects nothing, and a misspelled handoff status fails the transition with `tracker_payload_error`. A startup check would need the list's statuses (`GET /list/{id}`) and is the obvious next step if that confusion shows up in practice.

The list read always sends `include_closed=true` and `subtasks=true`. Without the first, a status of type `closed` (usually the terminal one) is invisible to the by-states read. Without the second, subtasks never become candidates.

## Identifiers

A task has an opaque ID (`86c0abcd`) and, in workspaces that turn custom IDs on, a custom ID (`ENG-42`). `Issue.ID` is always the opaque ID, because every write route takes it without extra parameters. `Issue.Identifier` prefers the custom ID, since that is what people see and what names the workspace directory.

Reading by custom ID needs `custom_task_ids=true` and `team_id` on the request. The by-identifier read adds both only when `team_id` is configured and the identifier has the custom-ID shape; anything else is read as a task ID. Without `team_id` a custom identifier is not found, so startup cleanup of terminal workspaces skips it rather than misreading it.

## No batch read

ClickUp has no route that reads many tasks by ID. The state-batch reads issue one `GET /task/{id}` per task through the shared bounded reader (eight at a time). On a busy instance this is the adapter's largest request cost, and ClickUp's rate limits are per token and per minute (100 a minute on the lower plans). A 429 maps to the retryable `tracker_api_error`, and the message carries `X-RateLimit-Reset`.

## Blockers come from dependencies

Task dependencies are the blocking relation. A task's `dependencies` array lists the dependencies in both directions: an entry whose `task_id` is the task itself means the task waits on `depends_on`, and an entry whose `depends_on` is the task itself means another task waits on it. Only the first kind is a blocker. The adapter keys on the direction, not on the entry's `type` field.

The array names blockers by ID and carries no status, so the adapter declares the per-issue blocker source. A candidate whose array proves it waits on nothing is answered from the listing; every other candidate, including one whose payload lacks the array, is marked unresolved and read with `fetch_issue_blockers`. That read costs one request for the task plus one per blocker. A deleted blocker is dropped instead of holding the issue forever.

## Comments

`GET /task/{id}/comment` returns top-level comments newest first, 25 at a time; older pages are requested with `start` (the oldest comment's date) and `start_id` (its ID). Replies live under `GET /comment/{id}/reply` and are fetched for each comment with a non-zero `reply_count`. Everything is merged, deduplicated by ID, and sorted oldest first. People answer the factory in threads, so leaving replies out would hide most answers.

ClickUp sends dates as Unix milliseconds in strings and IDs as strings or numbers depending on the route (the comment-create response has used both). The adapter decodes IDs with a type that accepts either, and renders dates as RFC 3339 UTC with millisecond precision.

ClickUp reports no comment edit time and no bot flag, so the comment change read is not implemented and `author_kind` is always unknown. The comment feed therefore reads the full list each turn.

## Tags as labels

Tags are the labels. They are space-scoped: `POST /task/{id}/tag/{name}` attaches a tag the space defines and rejects one it does not. A stage label or the parking label must therefore exist in the space before Sortie writes it, and the error tells the operator so. Neither tag write returns the task's tags, so every write is confirmed by a read.

Workspaces on plans without tags often use a Labels custom field instead. Reading those as labels is not implemented.

## Errors

Error bodies are `{"err": "...", "ECODE": "..."}`. ClickUp reports a missing or deleted task as `ECODE` `ITEM_013`, and has sent that code with HTTP 401 as well as 404, so the classifier checks the code before the status. A real authorization failure (`OAUTH_*`) is still `tracker_auth_error`.

## Checking against the live API

`internal/tracker/clickup/integration_test.go` runs against a real list when `SORTIE_CLICKUP_TEST=1`, with `SORTIE_CLICKUP_API_KEY`, `SORTIE_CLICKUP_LIST`, and optionally `SORTIE_CLICKUP_ACTIVE_STATES`, `SORTIE_CLICKUP_TEAM_ID`, and `SORTIE_CLICKUP_QUERY_FILTER`. The read test changes nothing. The write test posts one comment on the task named by `SORTIE_CLICKUP_WRITE_TASK` and runs only when that variable is set.

The unit tests use an in-memory fake of the routes above. The behaviors worth confirming on a live workspace after any change: the shape of `dependencies` in the list response, the `ITEM_013` status code, the comment pagination parameters, and whether a tag write for an undefined tag is rejected or creates the tag.
