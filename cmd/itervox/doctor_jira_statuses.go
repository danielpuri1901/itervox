package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/vnovick/itervox/internal/config"
	"github.com/vnovick/itervox/internal/logging"
	"github.com/vnovick/itervox/internal/tracker/jira"
)

// Jira status check for `itervox doctor`.
//
// The Jira tracker matches workflow states to Jira status names. An active
// state that is not a status of a project matches no issue of it, and a move
// to a working, completion or failed state that is not one fails on every
// run for its issues. Projects can have different workflows, so doctor
// checks each configured project and lists every configured state that is
// not a status of it, with the statuses that project has.

// JiraStatusCheck is the outcome of the Jira status check for one project.
type JiraStatusCheck struct {
	// Ran is false when the check does not apply (not a Jira tracker, or the
	// workflow did not validate) or the read failed.
	Ran     bool
	Project string
	Checked []string
	Missing []string
	// Available are the project's statuses, for the fix hint.
	Available []string
	// APIError is a redacted error from reading the statuses. Reported,
	// never fatal.
	APIError string
}

// checkJiraStatuses reads the statuses of each configured project and diffs
// them against the configured states, ignoring case like the adapter's
// transition lookup. It returns one check per project, in the configured
// order; a project it cannot read has its error and does not stop the others.
func checkJiraStatuses(ctx context.Context, cfg *config.Config, schemaPassed bool) []JiraStatusCheck {
	if cfg == nil || cfg.Tracker.Kind != "jira" || !schemaPassed {
		return nil // an invalid workflow: the schema error already says what is wrong
	}
	client := jira.NewClient(jira.ClientConfig{
		Endpoint:    cfg.Tracker.Endpoint,
		APIKey:      cfg.Tracker.APIKey,
		ProjectSlug: cfg.Tracker.ProjectSlug,
	})
	checked := trackerStateNames(cfg.Tracker, "")
	var checks []JiraStatusCheck
	for _, key := range client.ProjectKeys() {
		sc := JiraStatusCheck{Project: key, Checked: checked}
		if have, err := client.ProjectStatuses(ctx, key); err != nil {
			sc.APIError = logging.RedactString(err.Error())
		} else {
			sc.Ran, sc.Available = true, have
			sc.Missing = withoutLabels(sc.Checked, have)
		}
		checks = append(checks, sc)
	}
	return checks
}

// jiraStatusesMissing reports whether a configured state is not a status of
// some project (doctor fails).
func jiraStatusesMissing(checks []JiraStatusCheck) bool {
	return slices.ContainsFunc(checks, func(sc JiraStatusCheck) bool { return len(sc.Missing) > 0 })
}

// jiraProjectsLabel names the projects of a Jira tracker.project_slug in a
// doctor line: "project KAN" for one key, "projects KAN, OPS" for several.
func jiraProjectsLabel(slug string) string {
	keys := jira.ProjectKeys(slug)
	if len(keys) > 1 {
		return "projects " + strings.Join(keys, ", ")
	}
	return "project " + strings.Join(keys, "")
}

// renderJiraStatusChecks writes the lines of each project's check.
func renderJiraStatusChecks(b *strings.Builder, checks []JiraStatusCheck) {
	for _, sc := range checks {
		renderJiraStatusCheck(b, sc)
	}
}

func renderJiraStatusCheck(b *strings.Builder, sc JiraStatusCheck) {
	switch {
	case sc.APIError != "":
		fmt.Fprintf(b, "jira statuses: could not check project %s: %s\n", sc.Project, sc.APIError)
	case !sc.Ran:
		// Not a Jira tracker, or the workflow did not validate.
	case len(sc.Missing) == 0:
		fmt.Fprintf(b, "jira statuses: OK (%d configured states are statuses of project %s)\n", len(sc.Checked), sc.Project)
	default:
		quoted := make([]string, len(sc.Missing))
		for i, n := range sc.Missing {
			quoted[i] = strconv.Quote(n)
		}
		fmt.Fprintf(b, "ERROR: %d configured state(s) are not statuses of Jira project %s: %s. No issue is picked up in a missing active state, and a move to a missing state fails.\n",
			len(sc.Missing), sc.Project, strings.Join(quoted, ", "))
		fmt.Fprintf(b, "  statuses in %s: %s\n", sc.Project, strings.Join(sc.Available, ", "))
		fmt.Fprintf(b, "  Fix: change each one to a status above (case is ignored) in the tracker state settings (`itervox config show` names the file that sets it), or add the status to the project's workflow in Jira.\n")
	}
}
