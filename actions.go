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

type confirmationDetails struct {
	Number int64
	Runs   int
	Base   string
	Behind int64
}
type confirmAction func(context.Context, confirmationDetails) (bool, error)

func (p *Plugin) openBrowser(ctx context.Context, link, cwd string) error {
	program := "xdg-open"
	if p.Platform == "darwin" {
		program = "/usr/bin/open"
	}
	_, err := checked(p.command(ctx, program, []string{link}, cwd, ""))
	return err
}

func (p *Plugin) act(ctx context.Context, paneID, action string, confirm confirmAction) (*State, error) {
	switch action {
	case "open", "open-checks", "copy-slack", "open-comments":
	case "rerun-failed", "update-branch":
		if confirm == nil {
			if action == "rerun-failed" {
				return nil, errors.New("Rerunning failed jobs requires confirmation in the PR menu.")
			}
			return nil, errors.New("Updating the PR branch requires confirmation in the PR menu.")
		}
	default:
		return nil, fmt.Errorf("Unknown action: %s", action)
	}
	state, err := p.refresh(ctx, paneID, refreshOptions{Transient: true, PROnly: action != "open-checks" && action != "rerun-failed", Compare: action == "update-branch"})
	if err != nil {
		return nil, err
	}
	if state.Unavailable != "" {
		return nil, errors.New(state.Unavailable)
	}
	if state.PRError != "" {
		return nil, errors.New(state.PRError)
	}
	if state.PR == nil && action != "open" {
		return nil, errors.New("No PR for this branch. Create a PR first.")
	}
	pane, err := p.pane(ctx, paneID)
	if err != nil {
		return nil, err
	}
	checkout, err := p.checkout(ctx, pane)
	if err != nil {
		return nil, err
	}
	if checkout.Identity != state.Identity {
		return nil, errors.New("Repository changed. Try the action again.")
	}
	ctx = withGitHubHost(ctx, checkout.githubHost)
	ensureCurrent := func() error {
		current, err := p.pane(ctx, paneID)
		if err != nil {
			return err
		}
		currentCheckout, err := p.checkout(ctx, current)
		if err != nil {
			return err
		}
		if current.TerminalID != pane.TerminalID || currentCheckout.Identity != state.Identity {
			return errors.New("Repository changed. Try the action again.")
		}
		return ctx.Err()
	}
	switch action {
	case "copy-slack":
		err = p.copyPR(ctx, state.PR, checkout.Cwd)
	case "open-comments":
		err = p.openComments(ctx, state.PR, checkout.Cwd, ensureCurrent)
	case "rerun-failed":
		err = p.rerunFailed(ctx, paneID, state, checkout.Cwd, confirm, ensureCurrent)
	case "update-branch":
		err = p.updateBranch(ctx, paneID, state, checkout.Cwd, confirm, ensureCurrent)
	case "open-checks":
		var failure string
		for _, check := range state.Checks {
			if check.Bucket == "fail" {
				if _, e := webURL(check.Link); e == nil {
					failure = check.Link
					break
				}
			}
		}
		if failure != "" {
			err = p.openBrowser(ctx, failure, checkout.Cwd)
		} else {
			_, err = checked(p.command(ctx, "gh", []string{"pr", "checks", state.PR.URL, "--web"}, checkout.Cwd, ""))
		}
	case "open":
		if state.PR != nil {
			_, err = checked(p.command(ctx, "gh", []string{"pr", "view", state.PR.URL, "--web"}, checkout.Cwd, ""))
		} else {
			err = p.createPR(ctx, checkout)
		}
	}
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (p *Plugin) createPR(ctx context.Context, checkout State) error {
	upstream, err := checked(p.command(ctx, "git", []string{"for-each-ref", "--format=%(upstream:remotename)%00%(upstream:remoteref)", "refs/heads/" + checkout.Branch}, checkout.Cwd, ""))
	if err != nil {
		return err
	}
	remoteName, ref, _ := strings.Cut(upstream, "\x00")
	if remoteName == "" || remoteName == "." || !strings.HasPrefix(ref, "refs/heads/") {
		return errors.New("Push this branch and set its upstream before creating a PR. This plugin never pushes or forks.")
	}
	ahead, err := checked(p.command(ctx, "git", []string{"rev-list", "--count", "@{upstream}..HEAD"}, checkout.Cwd, ""))
	if err != nil {
		return err
	}
	if ahead != "0" {
		return errors.New("Push the branch's new commits before creating a PR.")
	}
	remote, err := checked(p.command(ctx, "git", []string{"remote", "get-url", remoteName}, checkout.Cwd, ""))
	if err != nil {
		return err
	}
	readRepo := func(args []string) (string, error) {
		text, err := checked(p.command(ctx, "gh", args, checkout.Cwd, ""))
		if err != nil {
			return "", err
		}
		var repo struct {
			NameWithOwner string `json:"nameWithOwner"`
		}
		if err := json.Unmarshal([]byte(text), &repo); err != nil {
			return "", err
		}
		if len(strings.Split(repo.NameWithOwner, "/")) != 2 {
			return "", errors.New("Unexpected GitHub repository response")
		}
		return repo.NameWithOwner, nil
	}
	base, err := readRepo([]string{"repo", "view", "--json", "nameWithOwner"})
	if err != nil {
		return err
	}
	head, err := readRepo([]string{"repo", "view", remote, "--json", "nameWithOwner"})
	if err != nil {
		return err
	}
	headRef := strings.TrimPrefix(ref, "refs/heads/")
	if base != head {
		owner, _, _ := strings.Cut(head, "/")
		headRef = owner + ":" + headRef
	}
	_, err = checked(p.command(ctx, "gh", []string{"pr", "create", "--web", "--head=" + headRef}, checkout.Cwd, ""))
	return err
}

var reviewAnchor = regexp.MustCompile(`^(?:discussion_r|r)[1-9]\d*$`)

func sameOrigin(a, b *url.URL) bool { return a.Scheme == b.Scheme && a.Host == b.Host }

func (p *Plugin) openComments(ctx context.Context, pr *PullRequest, cwd string, ensureCurrent func() error) error {
	prURL, repository, err := prRepository(pr)
	if err != nil {
		return err
	}
	threads, err := p.reviewThreads(ctx, pr, cwd, true)
	if err != nil {
		return err
	}
	unresolved := false
	for _, thread := range threads {
		if *thread.IsResolved {
			continue
		}
		unresolved = true
		if len(thread.Comments.Nodes) == 0 {
			continue
		}
		link := thread.Comments.Nodes[0].URL
		u, err := webURL(link)
		if err != nil {
			continue
		}
		base := fmt.Sprintf("/%s/pull/%d", repository, pr.Number)
		if sameOrigin(u, prURL) && (u.Path == base || u.Path == base+"/files" || u.Path == base+"/changes") && reviewAnchor.MatchString(u.Fragment) {
			if err := ensureCurrent(); err != nil {
				return err
			}
			return p.openBrowser(ctx, link, cwd)
		}
	}
	if !unresolved {
		return errors.New("No unresolved review threads.")
	}
	return errors.New("No usable link for unresolved review threads.")
}

func (p *Plugin) revalidatePR(ctx context.Context, paneID string, original *State, base bool) error {
	latest, err := p.refresh(ctx, paneID, refreshOptions{Transient: true, PROnly: true})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if latest.Unavailable != "" || latest.PRError != "" || latest.Identity != original.Identity || latest.PR == nil || latest.PR.State != "OPEN" || !samePR(original.PR, latest.PR) || (base && !sameBase(original.PR, latest.PR)) {
		return errors.New("PR changed since confirmation. Try the action again.")
	}
	return nil
}

func (p *Plugin) updateBranch(ctx context.Context, paneID string, state *State, cwd string, confirm confirmAction, ensureCurrent func() error) error {
	u, repository, err := prRepository(state.PR)
	if err != nil {
		return err
	}
	if state.PR.State != "OPEN" {
		return errors.New("Updating the branch requires an open PR.")
	}
	if state.BehindError != "" {
		return errors.New(state.BehindError)
	}
	if state.Behind == nil || *state.Behind < 0 {
		return errors.New("Branch status is unavailable.")
	}
	if err := ensureCurrent(); err != nil {
		return err
	}
	if *state.Behind == 0 {
		state.BranchUpdate = "up-to-date"
		return nil
	}
	approved, err := confirm(ctx, confirmationDetails{Number: state.PR.Number, Base: state.PR.BaseRefName, Behind: *state.Behind})
	if err != nil {
		return err
	}
	if !approved {
		state.BranchUpdate = "canceled"
		return nil
	}
	if err := ensureCurrent(); err != nil {
		return err
	}
	if err := p.revalidatePR(ctx, paneID, state, true); err != nil {
		return err
	}
	if err := ensureCurrent(); err != nil {
		return err
	}
	_, err = checked(p.command(ctx, "gh", []string{"api", "--hostname", u.Hostname(), "--method", "PUT", fmt.Sprintf("repos/%s/pulls/%d/update-branch", repository, state.PR.Number), "-f", "expected_head_sha=" + state.PR.HeadRefOID}, cwd, ""))
	if err == nil {
		state.BranchUpdate = "requested"
	}
	return err
}

type workflowRun struct {
	ID           int64  `json:"id"`
	Attempt      int64  `json:"run_attempt"`
	HeadSHA      string `json:"head_sha"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
	PullRequests []struct {
		Number int64 `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_requests"`
}

func (r workflowRun) eligible(pr *PullRequest) bool {
	if r.Status != "completed" || (r.Conclusion != "failure" && r.Conclusion != "timed_out") {
		return false
	}
	if r.HeadSHA == pr.HeadRefOID {
		return true
	}
	for _, linked := range r.PullRequests {
		if linked.Number == pr.Number && linked.Head.SHA == pr.HeadRefOID {
			return true
		}
	}
	return false
}

var runPath = regexp.MustCompile(`^/([\w.-]+/[\w.-]+)/actions/runs/([1-9]\d*)(?:/(?:jobs?/[1-9]\d*|attempts/[1-9]\d*))?/?$`)

func (p *Plugin) rerunFailed(ctx context.Context, paneID string, state *State, cwd string, confirm confirmAction, ensureCurrent func() error) error {
	u, repository, err := prRepository(state.PR)
	if err != nil {
		return err
	}
	if state.PR.State != "OPEN" {
		return errors.New("Rerunning checks requires an open PR.")
	}
	if state.ChecksError != "" {
		return errors.New(state.ChecksError)
	}
	if state.PR.HeadRefOID == "" {
		return errors.New("Missing PR head commit.")
	}
	readRun := func(id string) (workflowRun, error) {
		text, err := checked(p.command(ctx, "gh", []string{"api", "--hostname", u.Hostname(), "repos/" + repository + "/actions/runs/" + id}, cwd, ""))
		if err != nil {
			return workflowRun{}, err
		}
		var run workflowRun
		if err := json.Unmarshal([]byte(text), &run); err != nil {
			return run, fmt.Errorf("Unexpected GitHub Actions run response: %w", err)
		}
		if strconv.FormatInt(run.ID, 10) != id || run.Attempt < 1 || run.HeadSHA == "" {
			return run, errors.New("Unexpected GitHub Actions run response")
		}
		return run, nil
	}
	seen := make(map[string]bool)
	var runs []workflowRun
	for _, check := range state.Checks {
		if check.Bucket != "fail" {
			continue
		}
		link, err := webURL(check.Link)
		if err != nil || !sameOrigin(link, u) {
			continue
		}
		match := runPath.FindStringSubmatch(link.Path)
		if match == nil || !strings.EqualFold(match[1], repository) || seen[match[2]] {
			continue
		}
		seen[match[2]] = true
		run, err := readRun(match[2])
		if err != nil {
			return err
		}
		if run.eligible(state.PR) {
			runs = append(runs, run)
		}
	}
	if len(runs) == 0 {
		return errors.New("No completed failed GitHub Actions runs for this PR.")
	}
	if err := ensureCurrent(); err != nil {
		return err
	}
	approved, err := confirm(ctx, confirmationDetails{Number: state.PR.Number, Runs: len(runs)})
	if err != nil {
		return err
	}
	if !approved {
		return nil
	}
	if err := ensureCurrent(); err != nil {
		return err
	}
	if err := p.revalidatePR(ctx, paneID, state, false); err != nil {
		return err
	}
	for _, run := range runs {
		err := func() error {
			id := strconv.FormatInt(run.ID, 10)
			current, err := readRun(id)
			if err != nil {
				return err
			}
			if !current.eligible(state.PR) || current.Attempt != run.Attempt || current.HeadSHA != run.HeadSHA {
				return fmt.Errorf("Run %d changed since confirmation. Try again.", run.ID)
			}
			if err := ensureCurrent(); err != nil {
				return err
			}
			_, err = checked(p.command(ctx, "gh", []string{"run", "rerun", id, "--failed", "--repo", u.Hostname() + "/" + repository}, cwd, ""))
			return err
		}()
		if err != nil {
			if state.RerunCount > 0 {
				return fmt.Errorf("Rerun requested for %d/%d runs. %w", state.RerunCount, len(runs), err)
			}
			return err
		}
		state.RerunCount++
	}
	return nil
}
