package jira_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vnovick/itervox/internal/domain"
	"github.com/vnovick/itervox/internal/tracker"
	"github.com/vnovick/itervox/internal/tracker/jira"
)

// A Jira tracker can work on several projects: tracker.project_slug holds
// one project key or several, separated by commas, the form a Linear
// tracker takes for several projects.

// TestProjectJQLForOneAndSeveralProjects: one key keeps the query it always
// had; several keys are one "project in (...)" clause, in the configured
// order, each key quoted, with the assignee and the labels ANDed after it.
func TestProjectJQLForOneAndSeveralProjects(t *testing.T) {
	const states = `status in ("To Do", "In Progress") ORDER BY created ASC`
	cases := []struct {
		name, slug, assignee string
		labels               []string
		want                 string
	}{
		{"one key", "PROJ", "", nil, `project = "PROJ" AND ` + states},
		{"one key with assignee and labels", "PROJ", "me", []string{"agent"}, `project = "PROJ" AND assignee = currentUser() AND labels in ("agent") AND ` + states},
		{"two keys", "PROJ, OPS", "", nil, `project in ("PROJ", "OPS") AND ` + states},
		{"two keys without a space", "PROJ,OPS", "", nil, `project in ("PROJ", "OPS") AND ` + states},
		{"three keys, configured order", "OPS, PROJ, DATA_1", "", nil, `project in ("OPS", "PROJ", "DATA_1") AND ` + states},
		{"spaces and a repeated key", " PROJ , OPS,PROJ ", "", nil, `project in ("PROJ", "OPS") AND ` + states},
		{"two keys, assignee me", "PROJ, OPS", "me", nil, `project in ("PROJ", "OPS") AND assignee = currentUser() AND ` + states},
		{"two keys, account id", "PROJ, OPS", "acc-ada", nil, `project in ("PROJ", "OPS") AND assignee = "acc-ada" AND ` + states},
		{"two keys, labels", "PROJ, OPS", "", []string{"agent", "bot"}, `project in ("PROJ", "OPS") AND labels in ("agent", "bot") AND ` + states},
		{"two keys, assignee and labels", "PROJ, OPS", "acc-ada", []string{"agent"}, `project in ("PROJ", "OPS") AND assignee = "acc-ada" AND labels in ("agent") AND ` + states},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, seen := newProjectsJiraServer(t, tc.slug, tc.assignee, tc.labels, fakeScopeSite)

			_, err := c.FetchCandidateIssues(t.Context())

			require.NoError(t, err)
			assert.Equal(t, tc.want, searchJQL(t, seen()))
		})
	}

	t.Run("FetchIssuesByStates", func(t *testing.T) {
		c, seen := newProjectsJiraServer(t, "PROJ, OPS", "", nil, fakeScopeSite)

		_, err := c.FetchIssuesByStates(t.Context(), []string{"Backlog", "Done"})

		require.NoError(t, err)
		assert.Equal(t, `project in ("PROJ", "OPS") AND status in ("Backlog", "Done") ORDER BY created ASC`, searchJQL(t, seen()))
	})
}

// TestIncrementalJQLForOneAndSeveralProjects: the incremental pull names
// the same projects as the list reads, and only them; the assignee and the
// labels are applied in memory, whatever the number of projects.
func TestIncrementalJQLForOneAndSeveralProjects(t *testing.T) {
	cases := []struct {
		name, slug, assignee string
		labels               []string
		want                 string
	}{
		{"one key", "PROJ", "", nil, fmt.Sprintf(`project = "PROJ" AND updated >= %d ORDER BY updated DESC`, sinceWindowMs)},
		{"two keys", "PROJ, OPS", "", nil, fmt.Sprintf(`project in ("PROJ", "OPS") AND updated >= %d ORDER BY updated DESC`, sinceWindowMs)},
		{"two keys with assignee and labels", "PROJ, OPS", "me", []string{"agent"}, fmt.Sprintf(`project in ("PROJ", "OPS") AND updated >= %d ORDER BY updated DESC`, sinceWindowMs)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, seen := newProjectsJiraServer(t, tc.slug, tc.assignee, tc.labels, fakeScopeSite)

			_, _, err := c.FetchIssuesUpdatedSince(t.Context(), sinceFixture)

			require.NoError(t, err)
			assert.Equal(t, tc.want, searchJQL(t, seen()))
		})
	}
}

// TestIncrementalWithSeveralProjectsAppliesTheScopeToEach: an issue of
// either project that matches the labels is changed; one that lost its
// label is gone, whichever project it is in.
func TestIncrementalWithSeveralProjectsAppliesTheScopeToEach(t *testing.T) {
	listing := []any{
		scopedIssue("1", "PROJ-1", "To Do", "", "agent"),
		scopedIssue("2", "OPS-2", "In Progress", "", "agent"),
		scopedIssue("3", "OPS-3", "To Do", ""),
		scopedIssue("4", "PROJ-4", "To Do", ""),
	}
	c, _ := newProjectsJiraServer(t, "PROJ, OPS", "", []string{"agent"}, func(n int, r recorded, w http.ResponseWriter) {
		if r.Path == "/rest/api/3/search/jql" {
			writeJSON(w, map[string]any{"issues": listing, "isLast": true})
			return
		}
		fakeScopeSite(n, r, w)
	})

	changed, gone, err := c.FetchIssuesUpdatedSince(t.Context(), sinceFixture)

	require.NoError(t, err)
	assert.Equal(t, []string{"1", "2"}, ids(changed))
	assert.Equal(t, []string{"3", "4"}, gone)
}

// TestMatchesFilterAcceptsEveryConfiguredProject: an issue read by id is in
// the scope when its key is in any configured project, so an issue moved
// from one configured project to another stays in scope; a key of any other
// project, or one that only starts with a configured key, is not.
func TestMatchesFilterAcceptsEveryConfiguredProject(t *testing.T) {
	c, seen := newProjectsJiraServer(t, "PROJ, OPS", "", nil, fakeScopeSite)
	for _, ck := range []struct {
		key   string
		match bool
	}{
		{"PROJ-1", true},
		{"OPS-7", true},
		{"OTHER-1", false},
		{"PROJECT-1", false},
		{"OPSX-1", false},
		{"XOPS-1", false},
	} {
		match, known := c.MatchesFilter(domain.Issue{Identifier: ck.key})
		assert.Equal(t, ck.match, match, ck.key)
		assert.True(t, known, ck.key)
	}

	labelled, _ := newProjectsJiraServer(t, "PROJ, OPS", "", []string{"agent"}, fakeScopeSite)
	match, _ := labelled.MatchesFilter(domain.Issue{Identifier: "OPS-7", Labels: []string{"Agent"}})
	assert.True(t, match, "the second project with the label")
	match, _ = labelled.MatchesFilter(domain.Issue{Identifier: "OPS-7"})
	assert.False(t, match, "the second project without the label")
	assert.Empty(t, seen(), "MatchesFilter makes no request")
}

// fakeIssueSite answers GET /rest/api/3/issue/{key} with the issue under
// the key in moved (a moved issue: Jira follows the old key), else under
// the key asked for, and every comment read with no comments.
func fakeIssueSite(moved map[string]string) func(int, recorded, http.ResponseWriter) {
	return func(_ int, r recorded, w http.ResponseWriter) {
		key := strings.TrimPrefix(r.Path, "/rest/api/3/issue/")
		switch {
		case strings.HasSuffix(key, "/comment"):
			writeJSON(w, map[string]any{"comments": []any{}, "total": 0})
		case r.Method == http.MethodGet && !strings.Contains(key, "/"):
			if to, ok := moved[key]; ok {
				key = to
			}
			writeJSON(w, issueFixture("1"+strings.Map(digitsOnly, key), key, "To Do"))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}
}

func digitsOnly(r rune) rune {
	if r >= '0' && r <= '9' {
		return r
	}
	return -1
}

// TestFetchIssueByIdentifierAcceptsEveryConfiguredProject: a key of any
// configured project is read; a key of another project is refused before
// any request; an issue Jira reports under a key of another configured
// project (moved between them) is returned, and one moved out of every
// configured project is not found.
func TestFetchIssueByIdentifierAcceptsEveryConfiguredProject(t *testing.T) {
	c, seen := newProjectsJiraServer(t, "PROJ, OPS", "", nil, fakeIssueSite(map[string]string{
		"OPS-4": "PROJ-9",
		"OPS-5": "OTHER-1",
	}))

	for _, key := range []string{"PROJ-1", "OPS-2"} {
		got, err := c.FetchIssueByIdentifier(t.Context(), key)
		require.NoError(t, err, key)
		assert.Equal(t, key, got.Identifier)
	}

	before := len(seen())
	got, err := c.FetchIssueByIdentifier(t.Context(), "OTHER-3")
	require.ErrorIs(t, err, tracker.ErrNotFound)
	assert.Nil(t, got)
	assert.Len(t, seen(), before, "another project's key is refused before any request")

	got, err = c.FetchIssueByIdentifier(t.Context(), "OPS-4")
	require.NoError(t, err)
	assert.Equal(t, "PROJ-9", got.Identifier, "moved between two configured projects")

	got, err = c.FetchIssueByIdentifier(t.Context(), "OPS-5")
	require.ErrorIs(t, err, tracker.ErrNotFound, "moved out of every configured project")
	assert.Nil(t, got)
}

// createInProjectsHandler serves CreateIssue for a client with several
// projects. The source issue read answers with the key in sourceKeys (by
// the id or key asked for), a Story type; the create answers 10050 / NEW-1.
func createInProjectsHandler(sourceKeys map[string]string) func(int, recorded, http.ResponseWriter) {
	return func(_ int, r recorded, w http.ResponseWriter) {
		switch {
		case r.Method == http.MethodGet && r.Query == "fields=issuetype":
			ref := strings.TrimPrefix(r.Path, "/rest/api/3/issue/")
			writeJSON(w, map[string]any{"id": "10001", "key": sourceKeys[ref], "fields": map[string]any{
				"issuetype": map[string]any{"id": "10002", "name": "Story", "subtask": false},
			}})
		case r.Method == http.MethodPost && r.Path == "/rest/api/3/issue":
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{"id": "10050", "key": "NEW-1"})
		case r.Path == "/rest/api/3/issue/bulkfetch":
			writeJSON(w, map[string]any{"issues": []any{issueFixture("10050", "NEW-1", "To Do")}})
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}
}

// TestCreateIssuePutsAFollowUpInTheSourceIssueProject: a follow-up goes to
// the project its source issue is in now (read from the key Jira returns,
// so a numeric id and a moved issue count too), when that project is
// configured; with no source, or a source outside the configured projects,
// it goes to the first configured project.
func TestCreateIssuePutsAFollowUpInTheSourceIssueProject(t *testing.T) {
	sources := map[string]string{
		"OPS-4":   "OPS-4",
		"10077":   "OPS-4",
		"PROJ-1":  "PROJ-1",
		"PROJ-2":  "OPS-8", // moved from PROJ to OPS: Jira follows the old key
		"OTHER-3": "OTHER-3",
	}
	cases := []struct {
		name, slug, source, wantProject string
	}{
		{"source in the second project", "PROJ, OPS", "OPS-4", "OPS"},
		{"source by numeric id", "PROJ, OPS", "10077", "OPS"},
		{"source in the first project", "PROJ, OPS", "PROJ-1", "PROJ"},
		{"source moved between the projects", "PROJ, OPS", "PROJ-2", "OPS"},
		{"no source", "PROJ, OPS", "", "PROJ"},
		{"no source, other order", "OPS, PROJ", "", "OPS"},
		{"source outside the projects", "PROJ, OPS", "OTHER-3", "PROJ"},
		{"one project, source outside it", "PROJ", "OTHER-3", "PROJ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, seen := newProjectsJiraServer(t, tc.slug, "", nil, createInProjectsHandler(sources))

			got, err := c.CreateIssue(t.Context(), tc.source, tracker.NewIssue{Title: "Follow up"})

			require.NoError(t, err)
			require.NotNil(t, got)
			var created map[string]any
			for _, r := range seen() {
				if r.Method == http.MethodPost && r.Path == "/rest/api/3/issue" {
					created = r.jsonBody(t)
				}
			}
			require.NotNil(t, created, "the create request was sent")
			fields := created["fields"].(map[string]any)
			assert.Equal(t, map[string]any{"key": tc.wantProject}, fields["project"])
		})
	}
}

// TestProbeProjectAccessReadsEveryProject: the doctor probe reads the
// account once, then each configured project in order, and an unreadable
// project fails it with that project named.
func TestProbeProjectAccessReadsEveryProject(t *testing.T) {
	site := func(missing string) func(int, recorded, http.ResponseWriter) {
		return func(_ int, r recorded, w http.ResponseWriter) {
			switch r.Path {
			case "/rest/api/3/myself":
				writeJSON(w, map[string]any{"accountId": "abc", "displayName": "Ada Lovelace"})
			case "/rest/api/3/project/" + missing:
				w.WriteHeader(http.StatusNotFound)
			case "/rest/api/3/project/PROJ", "/rest/api/3/project/OPS":
				writeJSON(w, map[string]any{"key": strings.TrimPrefix(r.Path, "/rest/api/3/project/")})
			default:
				w.WriteHeader(http.StatusTeapot)
			}
		}
	}

	c, seen := newProjectsJiraServer(t, "PROJ, OPS", "", nil, site(""))
	account, err := c.ProbeProjectAccess(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Ada Lovelace", account)
	var paths []string
	for _, r := range seen() {
		paths = append(paths, r.Method+" "+r.Path)
	}
	assert.Equal(t, []string{"GET /rest/api/3/myself", "GET /rest/api/3/project/PROJ", "GET /rest/api/3/project/OPS"}, paths)

	c, _ = newProjectsJiraServer(t, "PROJ, OPS", "", nil, site("OPS"))
	_, err = c.ProbeProjectAccess(t.Context())
	require.ErrorIs(t, err, tracker.ErrNotFound)
	assert.Contains(t, err.Error(), "jira_probe: project OPS: ")
}

// TestProjectStatusesReadsTheNamedProject: the status check reads the
// project it is given, so doctor can check each configured project, and
// names it in an error.
func TestProjectStatusesReadsTheNamedProject(t *testing.T) {
	c, seen := newProjectsJiraServer(t, "PROJ, OPS", "", nil, func(_ int, r recorded, w http.ResponseWriter) {
		if r.Path != "/rest/api/3/project/OPS/statuses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"1","name":"Task","statuses":[{"id":"10","name":"Open"},{"id":"12","name":"Closed"}]}]`))
	})

	got, err := c.ProjectStatuses(t.Context(), "OPS")
	require.NoError(t, err)
	assert.Equal(t, []string{"Open", "Closed"}, got)
	assert.Equal(t, "/rest/api/3/project/OPS/statuses", seen()[0].Path)

	_, err = c.ProjectStatuses(t.Context(), "PROJ")
	require.ErrorIs(t, err, tracker.ErrNotFound)
	assert.Contains(t, err.Error(), "jira_statuses: project PROJ: ")
}

// TestProjectKeys: the configured keys, trimmed, without repeats, in the
// configured order.
func TestProjectKeys(t *testing.T) {
	for slug, want := range map[string][]string{
		"PROJ":               {"PROJ"},
		"PROJ, OPS":          {"PROJ", "OPS"},
		" OPS ,PROJ, OPS, ,": {"OPS", "PROJ"},
		"":                   nil,
	} {
		assert.Equal(t, want, jira.ProjectKeys(slug), slug)
		assert.Equal(t, want, jira.NewClient(jira.ClientConfig{ProjectSlug: slug}).ProjectKeys(), slug)
	}
}
