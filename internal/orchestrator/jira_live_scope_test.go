//go:build integration

package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vnovick/itervox/internal/config"
	"github.com/vnovick/itervox/internal/domain"
	"github.com/vnovick/itervox/internal/tracker"
	"github.com/vnovick/itervox/internal/tracker/jira"
	"github.com/vnovick/itervox/internal/tracker/store"
)

// liveJira sends one JSON request to the Jira site, outside the adapter, and
// decodes a 2xx answer into out when out is not nil. It returns the status.
func liveJira(t *testing.T, endpoint, apiKey, method, path string, in, out any) int {
	t.Helper()
	var body bytes.Buffer
	if in != nil {
		require.NoError(t, json.NewEncoder(&body).Encode(in))
	}
	req, err := http.NewRequestWithContext(t.Context(), method, strings.TrimSuffix(endpoint, "/")+path, &body)
	require.NoError(t, err)
	email, token, _ := strings.Cut(apiKey, ":")
	req.SetBasicAuth(email, token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	if out != nil && resp.StatusCode < 300 {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode
}

// moveJiraIssueLive moves the issue to the target project as its "Task"
// type through Jira Cloud's bulk move (POST /rest/api/3/bulk/issues/move),
// with Jira choosing the status and field values, and waits for the move
// task (GET /rest/api/3/bulk/queue/{taskId}) to complete.
func moveJiraIssueLive(t *testing.T, endpoint, apiKey, issueID, target string) {
	t.Helper()
	var proj struct {
		IssueTypes []struct{ ID, Name string } `json:"issueTypes"`
	}
	require.Equal(t, http.StatusOK, liveJira(t, endpoint, apiKey, http.MethodGet, "/rest/api/3/project/"+target, nil, &proj))
	i := slices.IndexFunc(proj.IssueTypes, func(it struct{ ID, Name string }) bool { return it.Name == "Task" })
	require.GreaterOrEqual(t, i, 0, "project %s has a Task type", target)
	var submitted struct {
		TaskID string `json:"taskId"`
	}
	require.Equal(t, http.StatusCreated, liveJira(t, endpoint, apiKey, http.MethodPost, "/rest/api/3/bulk/issues/move", map[string]any{
		"sendBulkNotification": false,
		"targetToSourcesMapping": map[string]any{target + "," + proj.IssueTypes[i].ID: map[string]any{
			"inferClassificationDefaults": true, "inferFieldDefaults": true, "inferStatusDefaults": true,
			"inferSubtaskTypeDefault": true, "issueIdsOrKeys": []string{issueID},
		}},
	}, &submitted))
	waitLive(t, 2*time.Minute, "bulk move task completes", func() bool {
		var task struct {
			Status string `json:"status"`
		}
		require.Equal(t, http.StatusOK, liveJira(t, endpoint, apiKey, http.MethodGet, "/rest/api/3/bulk/queue/"+submitted.TaskID, nil, &task))
		require.NotContains(t, []string{"FAILED", "CANCELLED", "CANCEL_REQUESTED"}, task.Status)
		return task.Status == "COMPLETE"
	})
}

// waitLive polls cond on the test goroutine, once a second, until it holds;
// it fails the test after timeout.
func waitLive(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("after %s: %s", timeout, what)
		}
		time.Sleep(time.Second)
	}
}

// TestJiraLiveClaimCheckWithSeveralProjects runs the dispatch claim check
// (admitClaimRead, leftTrackerScope) over the Jira adapter and the tracker
// store on a real Jira Cloud site with two projects, for an issue moved
// from the first project to the second after the store's full pull, so the
// store offers it under its old key. The claim read by id shows the new key.
// With both projects configured, the moved issue is still in the tracker
// scope and gets an agent. With only the first project configured, it left
// the scope and gets no agent, and the store forgets it. In both cases an
// unmoved issue is dispatched as before.
//
// It needs JIRA_ENDPOINT, JIRA_PROJECT, JIRA_SECOND_PROJECT and
// JIRA_API_KEY (email:api_token) and skips without them. Its issues carry a
// label of their own and the tracker is scoped by it, so no other issue is
// read or started. It moves every issue to the done state at the end and
// never prints the API key, the site or the account.
func TestJiraLiveClaimCheckWithSeveralProjects(t *testing.T) {
	endpoint := os.Getenv("JIRA_ENDPOINT")
	first := os.Getenv("JIRA_PROJECT")
	second := os.Getenv("JIRA_SECOND_PROJECT")
	apiKey := os.Getenv("JIRA_API_KEY")
	if endpoint == "" || first == "" || second == "" || apiKey == "" {
		t.Skip("set JIRA_ENDPOINT, JIRA_PROJECT, JIRA_SECOND_PROJECT and JIRA_API_KEY to run the live Jira claim-check test")
	}
	// The adapter and liveJira keep idle HTTP/2 connections in the default
	// transport; the package's goroutine leak check (TestMain) needs them
	// closed. Registered first, so it runs after every other cleanup.
	t.Cleanup(http.DefaultTransport.(*http.Transport).CloseIdleConnections)
	active := []string{"To Do", "In Progress"}
	for _, tc := range []struct {
		name, slug  string
		wantStarted bool
	}{
		{"both projects configured", first + ", " + second, true},
		{"only the first project configured", first, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			label := fmt.Sprintf("itervox-dispatch-%d", time.Now().UnixNano())
			up := jira.NewClient(jira.ClientConfig{
				Endpoint: endpoint, APIKey: apiKey, ProjectSlug: tc.slug, ActiveStates: active, Labels: []string{label},
			})
			create := func(name string) *domain.Issue {
				is, err := up.CreateIssue(ctx, "", tracker.NewIssue{Title: "itervox dispatch " + label + " " + name, Labels: []string{label}, State: active[0]})
				require.NoError(t, err)
				t.Cleanup(func() {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					if err := up.UpdateIssueState(cleanupCtx, is.ID, "Done"); err != nil {
						t.Logf("cleanup: move %s to Done: %v", is.ID, err)
					}
				})
				require.True(t, strings.HasPrefix(is.Identifier, first+"-"), is.Identifier)
				return is
			}
			moving, staying := create("moves"), create("stays")

			keysOf := func(list []domain.Issue) map[string]string { // issue id -> key
				out := map[string]string{}
				for _, is := range list {
					out[is.ID] = is.Identifier
				}
				return out
			}
			// The store's full pull, before the move. The store then pulls no
			// more, so it offers the moved issue under its old key, as a store
			// does between the move and its next full pull (an incremental
			// pull no longer lists an issue moved out of the projects). Search
			// lags the creates, so a new store is pulled until it lists both.
			var tr tracker.Tracker
			offered := func() map[string]string {
				cands, err := tr.FetchCandidateIssues(ctx)
				require.NoError(t, err)
				return keysOf(cands)
			}
			waitLive(t, 90*time.Second, "a store's full pull lists both new issues", func() bool {
				var err error
				tr, _, err = store.Wrap(up, store.Config{
					Path: filepath.Join(t.TempDir(), "tracker_store.json"), Scope: "jira-live|" + tc.slug,
					ActiveStates: active, SyncInterval: time.Hour, FullSyncInterval: time.Hour, ListingsLagWrites: true,
				})
				require.NoError(t, err)
				k := offered()
				return k[moving.ID] == moving.Identifier && k[staying.ID] == staying.Identifier
			})

			moveJiraIssueLive(t, endpoint, apiKey, moving.ID, second)
			fresh, err := up.FetchIssueStatesByIDs(ctx, []string{moving.ID})
			require.NoError(t, err)
			require.Len(t, fresh, 1)
			newKey := fresh[0].Identifier
			require.True(t, strings.HasPrefix(newKey, second+"-"), newKey)
			require.Equal(t, moving.Identifier, offered()[moving.ID], "the store offers the moved issue under its old key")
			t.Logf("%s moved to %s; the store still offers it as %s", moving.Identifier, newKey, moving.Identifier)

			cfg := &config.Config{}
			cfg.Tracker.Kind = "jira"
			cfg.Tracker.ActiveStates = active
			cfg.Tracker.TerminalStates = []string{"Done"}
			cfg.Agent.MaxConcurrentAgents = 3
			cfg.Agent.MaxConcurrentAgentsByState = map[string]int{}
			cfg.Agent.MaxTurns = 1
			o := New(cfg, tr, &parkingRunner{}, nil)
			t.Cleanup(func() {
				cancel()
				for _, id := range []string{moving.Identifier, newKey, staying.Identifier} {
					o.cancelAndCleanupWorker(id)
				}
				o.workersWg.Wait(5 * time.Second)
			})
			state := o.onTick(ctx, NewState(cfg))

			if assert.Contains(t, state.Running, staying.ID, "an unmoved issue is dispatched") {
				t.Logf("tick: started %s", state.Running[staying.ID].Issue.Identifier)
			}
			if tc.wantStarted {
				// The run starts from the store's copy, so it carries the key
				// the store had (mergeFresh keeps the candidate's identifier).
				if assert.Contains(t, state.Running, moving.ID, "with %s configured, the moved issue %s is in scope", tc.slug, newKey) {
					t.Logf("tick: started issue %s (now %s in Jira; the run entry says %s); scope %q",
						moving.ID, newKey, state.Running[moving.ID].Issue.Identifier, tc.slug)
				}
				return
			}
			assert.NotContains(t, state.Running, moving.ID, "with only %s configured, the moved issue %s left the scope", first, newKey)
			_, still := offered()[moving.ID]
			assert.False(t, still, "the store forgot the issue the claim check refused")
			t.Logf("tick: did not start %s (moved out of %s to %s); the store no longer offers it; scope %q", moving.Identifier, first, newKey, tc.slug)
		})
	}
}
