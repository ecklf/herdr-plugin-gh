package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const threadsQuery = `query($id: ID!, $endCursor: String, $includeComments: Boolean!) {
  node(id: $id) {
    ... on PullRequest {
      reviewThreads(first: 100, after: $endCursor) {
        nodes {
          isResolved
          comments(first: 1) @include(if: $includeComments) { nodes { url } }
        }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`

var prPath = regexp.MustCompile(`^/([\w.-]+/[\w.-]+)/pull/([1-9]\d*)/?$`)
var noPRPattern = regexp.MustCompile(`(?i)^no pull requests? found for branch\b`)
var noChecksPattern = regexp.MustCompile(`(?i)^no checks reported on the ['"]?.+['"]? branch\s*$`)

func webURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return nil, errors.New("Invalid GitHub PR URL")
	}
	// WHATWG URLs normalize default ports; retain the same host/origin behavior.
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		u.Host = u.Hostname()
	}
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

func prRepository(pr *PullRequest) (*url.URL, string, error) {
	u, err := webURL(pr.URL)
	if err != nil {
		return nil, "", err
	}
	if u.Port() != "" {
		return nil, "", errors.New("GitHub CLI does not support custom GitHub ports.")
	}
	match := prPath.FindStringSubmatch(u.Path)
	if match == nil || match[2] != strconv.FormatInt(pr.Number, 10) {
		return nil, "", errors.New("Invalid GitHub PR URL")
	}
	return u, match[1], nil
}

type reviewThread struct {
	IsResolved *bool `json:"isResolved"`
	Comments   struct {
		Nodes []struct {
			URL string `json:"url"`
		} `json:"nodes"`
	} `json:"comments"`
}

func (p *Plugin) reviewThreads(ctx context.Context, pr *PullRequest, cwd string, comments bool) ([]reviewThread, error) {
	if pr.ID == "" {
		return nil, errors.New("Missing GitHub PR node ID")
	}
	u, _, err := prRepository(pr)
	if err != nil {
		return nil, err
	}
	text, err := checked(p.command(ctx, "gh", []string{"api", "graphql", "--hostname", u.Hostname(), "--paginate", "--slurp", "-f", "id=" + pr.ID, "-F", fmt.Sprintf("includeComments=%t", comments), "-f", "query=" + threadsQuery}, cwd, ""))
	if err != nil {
		return nil, err
	}
	var pages []struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Data struct {
			Node *struct {
				ReviewThreads *struct {
					Nodes    []reviewThread `json:"nodes"`
					PageInfo *struct {
						HasNextPage *bool `json:"hasNextPage"`
					} `json:"pageInfo"`
				} `json:"reviewThreads"`
			} `json:"node"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &pages); err != nil {
		return nil, fmt.Errorf("Invalid GitHub review threads JSON: %w", err)
	}
	if len(pages) == 0 {
		return nil, errors.New("Unexpected GitHub review threads response")
	}
	nodes := []reviewThread{}
	for index, page := range pages {
		if len(page.Errors) > 0 {
			messages := []string{}
			for _, e := range page.Errors {
				messages = append(messages, e.Message)
			}
			return nil, errors.New(strings.Join(messages, "; "))
		}
		if page.Data.Node == nil || page.Data.Node.ReviewThreads == nil {
			return nil, errors.New("Incomplete GitHub review threads response")
		}
		threads := page.Data.Node.ReviewThreads
		if threads.Nodes == nil || threads.PageInfo == nil || threads.PageInfo.HasNextPage == nil || *threads.PageInfo.HasNextPage != (index < len(pages)-1) {
			return nil, errors.New("Incomplete GitHub review threads response")
		}
		for _, thread := range threads.Nodes {
			if thread.IsResolved == nil {
				return nil, errors.New("Incomplete GitHub review threads response")
			}
		}
		nodes = append(nodes, threads.Nodes...)
	}
	return nodes, nil
}

func (p *Plugin) compareBranch(ctx context.Context, pr *PullRequest, cwd string) (int64, error) {
	u, repository, err := prRepository(pr)
	if err != nil {
		return 0, err
	}
	if pr.BaseRefName == "" || pr.BaseRefOID == "" || pr.HeadRefOID == "" {
		return 0, errors.New("Missing PR branch details.")
	}
	// QueryEscape also escapes slashes; encode spaces as %20 rather than form data.
	escape := func(value string) string { return strings.ReplaceAll(url.QueryEscape(value), "+", "%20") }
	endpoint := "repos/" + repository + "/compare/" + escape(pr.BaseRefOID) + "..." + escape(pr.HeadRefOID) + "?per_page=1"
	text, err := checked(p.command(ctx, "gh", []string{"api", "--hostname", u.Hostname(), "--method", "GET", "--jq", "{behind_by}", endpoint}, cwd, ""))
	if err != nil {
		return 0, err
	}
	var result struct {
		Behind *int64 `json:"behind_by"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return 0, fmt.Errorf("Unexpected GitHub branch comparison response: %w", err)
	}
	if result.Behind == nil || *result.Behind < 0 {
		return 0, errors.New("Unexpected GitHub branch comparison response")
	}
	return *result.Behind, nil
}

func (p *Plugin) readPR(ctx context.Context, cwd string) (*PullRequest, error) {
	result, err := p.command(ctx, "gh", []string{"pr", "view", "--json", prFields}, cwd, "")
	if err != nil {
		return nil, err
	}
	if result.Code != 0 && noPRPattern.MatchString(strings.TrimSpace(result.Stderr)) {
		return nil, nil
	}
	text, err := checked(result, nil)
	if err != nil {
		return nil, err
	}
	var pr *PullRequest
	if err := json.Unmarshal([]byte(text), &pr); err != nil {
		return nil, fmt.Errorf("Invalid GitHub PR JSON: %w", err)
	}
	if pr == nil || pr.Number <= 0 || pr.URL == "" {
		return nil, errors.New("Unexpected GitHub PR response")
	}
	return pr, nil
}

func (p *Plugin) readChecks(ctx context.Context, pr *PullRequest, cwd string) ([]Check, error) {
	result, err := p.command(ctx, "gh", []string{"pr", "checks", pr.URL, "--json", "name,bucket,link"}, cwd, "")
	if err != nil {
		return nil, err
	}
	if result.Code == 1 && noChecksPattern.MatchString(strings.TrimSpace(result.Stderr)) {
		return []Check{}, nil
	}
	if result.Code != 0 && result.Code != 1 && result.Code != 8 {
		_, err = checked(result, nil)
		return nil, err
	}
	var checks []Check
	if err := json.Unmarshal([]byte(result.Stdout), &checks); err != nil {
		return nil, fmt.Errorf("Invalid GitHub checks JSON: %w", err)
	}
	if checks == nil {
		return nil, errors.New("Unexpected GitHub checks response")
	}
	for _, check := range checks {
		if check.Bucket == "" {
			return nil, errors.New("Unexpected GitHub checks response")
		}
	}
	return checks, nil
}
