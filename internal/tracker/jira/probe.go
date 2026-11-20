package jira

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
)

// ProbeProjectAccess is a read-only credential and scope check for `itervox
// doctor --deploy`. It reads /rest/api/3/myself to prove the credentials,
// then /rest/api/3/project/<key> for each configured project, in order, to
// prove the account can see it; the first it cannot read fails the probe,
// named in the error. It returns the account's display name, or its account
// id when the name is empty. A client without a project key is refused
// before any request.
func (c *Client) ProbeProjectAccess(ctx context.Context) (account string, err error) {
	if len(c.keys) == 0 {
		return "", errors.New("jira_probe: tracker.project_slug is empty")
	}
	var me struct {
		AccountID   string `json:"accountId"`
		DisplayName string `json:"displayName"`
	}
	if err := c.do(ctx, "GET", "/rest/api/3/myself", nil, &me); err != nil {
		return "", fmt.Errorf("jira_probe: %w", err)
	}
	for _, key := range c.keys {
		if err := c.do(ctx, "GET", "/rest/api/3/project/"+url.PathEscape(key), nil, nil); err != nil {
			return "", fmt.Errorf("jira_probe: project %s: %w", key, err)
		}
	}
	if me.DisplayName != "" {
		return me.DisplayName, nil
	}
	return me.AccountID, nil
}

// ProjectStatuses is a read-only check for `itervox doctor`: it returns the
// name of every status the workflows of the project with this key use, from
// one GET /rest/api/3/project/<key>/statuses (Browse Projects permission).
// Doctor calls it for each of ProjectKeys. Jira groups the statuses by issue
// type; each name is returned once, in the order first seen. An empty key is
// refused before any request.
func (c *Client) ProjectStatuses(ctx context.Context, key string) ([]string, error) {
	if key == "" {
		return nil, errors.New("jira_statuses: no project key")
	}
	var issueTypes []struct {
		Statuses []struct {
			Name string `json:"name"`
		} `json:"statuses"`
	}
	if err := c.do(ctx, "GET", "/rest/api/3/project/"+url.PathEscape(key)+"/statuses", nil, &issueTypes); err != nil {
		return nil, fmt.Errorf("jira_statuses: project %s: %w", key, err)
	}
	var names []string
	for _, it := range issueTypes {
		for _, s := range it.Statuses {
			if s.Name != "" && !slices.Contains(names, s.Name) {
				names = append(names, s.Name)
			}
		}
	}
	return names, nil
}
