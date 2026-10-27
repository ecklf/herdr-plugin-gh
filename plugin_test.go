package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func ptr[T any](value T) *T { return &value }
func ok(value any) commandResult {
	if text, yes := value.(string); yes {
		return commandResult{Stdout: text}
	}
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return commandResult{Stdout: string(data)}
}
func failed(message string) commandResult { return commandResult{Code: 1, Stderr: message} }
func testPR() *PullRequest {
	return &PullRequest{ID: "PR_test", Number: 42, URL: "https://github.com/owner/repo/pull/42", Title: "Add Slack copy", State: "OPEN", HeadRefOID: "remote-head", BaseRefName: "main", BaseRefOID: "base-head", Additions: ptr(int64(12)), Deletions: ptr(int64(3))}
}
func threadPage(nodes any, more bool) map[string]any {
	return map[string]any{"data": map[string]any{"node": map[string]any{"reviewThreads": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": more}}}}}
}
func testRun(id int64) workflowRun {
	return workflowRun{ID: id, Attempt: 2, HeadSHA: "remote-head", Status: "completed", Conclusion: "failure"}
}

type fixture struct {
	p                                                               *Plugin
	mu                                                              sync.Mutex
	calls                                                           []commandRequest
	reports                                                         []map[string]string
	pane                                                            Pane
	root, branch, head, config                                      string
	pr, checks, threads, compare, branchUpdate                      commandResult
	runs                                                            map[string]commandResult
	upstream, ahead, headRepo                                       string
	rootError, branchError, headError, clipboardError, browserError *commandResult
	before                                                          func(context.Context, commandRequest) error
	now                                                             time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		pane: Pane{ID: "w1:p1", TerminalID: "term-1", Cwd: "/wrong-root", ForegroundCwd: ptr("/work/repo with spaces"), Agent: "opencode"},
		root: "/work/repo with spaces", branch: "feature/checks", head: "local-head", config: "remote.origin.url git@github.com:owner/repo.git",
		pr: ok(testPR()), checks: ok([]Check{{Name: "test", Bucket: "pass"}}),
		threads: ok([]any{threadPage([]any{map[string]bool{"isResolved": false}, map[string]bool{"isResolved": true}}, false)}),
		compare: ok(map[string]int{"behind_by": 2}), branchUpdate: ok(map[string]string{"message": "Updating pull request branch"}),
		runs: map[string]commandResult{"1": ok(testRun(1))}, upstream: "origin\x00refs/heads/feature/checks", ahead: "0", headRepo: "owner/repo", now: time.UnixMilli(1780000000000),
	}
	f.p = newPlugin(map[string]string{"HERDR_BIN_PATH": "herdr-test", "HERDR_PLUGIN_STATE_DIR": dir, "HERDR_PLUGIN_CONFIG_DIR": filepath.Join(dir, "config"), "HERDR_SOCKET_PATH": "/session.sock", "HERDR_PLUGIN_CONTEXT_JSON": `{"focused_pane_id":"w1:p1"}`, "GH_TOKEN": "test-token", "GH_ENTERPRISE_TOKEN": "enterprise-test-token"})
	f.p.Now = func() time.Time { return f.now }
	f.p.Platform = "darwin"
	f.p.Execute = f.execute
	return f
}

func (f *fixture) execute(ctx context.Context, request commandRequest) (commandResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, request)
	before := f.before
	f.mu.Unlock()
	if before != nil {
		if err := before(ctx, request); err != nil {
			return commandResult{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	a := request.Args
	choose := func(failure *commandResult, value string) (commandResult, error) {
		if failure != nil {
			return *failure, nil
		}
		return ok(value), nil
	}
	switch request.Program {
	case "/usr/bin/osascript", "/usr/bin/pbcopy", "wl-copy", "xclip":
		return choose(f.clipboardError, "")
	case "/usr/bin/open", "xdg-open":
		return choose(f.browserError, "")
	case "herdr-test":
		switch {
		case a[0] == "plugin" && a[1] == "config-dir":
			return ok(f.p.Env["HERDR_PLUGIN_CONFIG_DIR"]), nil
		case a[0] == "pane" && (a[1] == "get" || a[1] == "current"):
			return ok(map[string]any{"result": map[string]any{"pane": f.pane}}), nil
		case a[1] == "report-metadata":
			tokens := make(map[string]string)
			for i := 0; i < len(a); i++ {
				if a[i] == "--token" {
					i++
					key, value, _ := strings.Cut(a[i], "=")
					tokens[key] = value
				} else if a[i] == "--clear-token" {
					i++
					tokens[a[i]] = ""
				}
			}
			f.reports = append(f.reports, tokens)
			return ok(""), nil
		case a[0] == "agent":
			return ok(map[string]any{"result": map[string]any{"agents": []Pane{f.pane}}}), nil
		case a[0] == "plugin" || a[0] == "notification":
			return ok(""), nil
		}
	case "git":
		switch a[0] {
		case "rev-parse":
			if a[1] == "--show-toplevel" {
				return choose(f.rootError, f.root)
			}
			return choose(f.headError, f.head)
		case "symbolic-ref":
			return choose(f.branchError, f.branch)
		case "config":
			return ok(f.config), nil
		case "for-each-ref":
			return ok(f.upstream), nil
		case "rev-list":
			return ok(f.ahead), nil
		case "remote":
			return ok("git@github.com:owner/repo.git"), nil
		}
	case "gh":
		switch {
		case slices.Contains(a, "--web"):
			return choose(f.browserError, "")
		case a[0] == "pr" && a[1] == "view":
			return f.pr, nil
		case a[0] == "pr" && a[1] == "checks":
			return f.checks, nil
		case a[0] == "api" && a[1] == "graphql":
			return f.threads, nil
		case isCompare(request):
			return f.compare, nil
		case isUpdate(request):
			return f.branchUpdate, nil
		case a[0] == "api" && strings.Contains(a[len(a)-1], "/actions/runs/"):
			id := a[len(a)-1][strings.LastIndex(a[len(a)-1], "/")+1:]
			return f.runs[id], nil
		case isRerun(request):
			return ok(""), nil
		case a[0] == "repo":
			name := "owner/repo"
			if a[2] != "--json" {
				name = f.headRepo
			}
			return ok(map[string]string{"nameWithOwner": name}), nil
		}
	}
	return commandResult{}, fmt.Errorf("Unexpected command: %s %s", request.Program, strings.Join(a, " "))
}

func isCompare(r commandRequest) bool {
	return r.Program == "gh" && slices.ContainsFunc(r.Args, func(a string) bool { return strings.Contains(a, "/compare/") })
}
func isUpdate(r commandRequest) bool { return r.Program == "gh" && slices.Contains(r.Args, "PUT") }
func isRerun(r commandRequest) bool {
	return r.Program == "gh" && len(r.Args) > 1 && r.Args[0] == "run" && r.Args[1] == "rerun"
}
func isBrowser(r commandRequest) bool {
	return r.Program == "/usr/bin/open" || r.Program == "xdg-open" || slices.Contains(r.Args, "--web")
}
func (f *fixture) requests(match func(commandRequest) bool) []commandRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []commandRequest
	for _, r := range f.calls {
		if match == nil || match(r) {
			result = append(result, r)
		}
	}
	return result
}
func (f *fixture) ghCount() int {
	return len(f.requests(func(r commandRequest) bool { return r.Program == "gh" }))
}
func requireError(t *testing.T, err error, text string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), text) {
		t.Fatalf("expected error containing %q, got %v", text, err)
	}
}
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func refreshFixture(t *testing.T, f *fixture, options refreshOptions) *State {
	t.Helper()
	state, err := f.p.refresh(context.Background(), "w1:p1", options)
	requireNoError(t, err)
	return state
}
func actFixture(t *testing.T, f *fixture, action string, confirm confirmAction) *State {
	t.Helper()
	state, err := f.p.act(context.Background(), "w1:p1", action, confirm)
	requireNoError(t, err)
	return state
}
func approve(context.Context, confirmationDetails) (bool, error) { return true, nil }
func decline(context.Context, confirmationDetails) (bool, error) { return false, nil }

func TestRefreshMetadataAndLiveCwd(t *testing.T) {
	f := newFixture(t)
	pane, err := f.p.target(context.Background())
	requireNoError(t, err)
	if pane.ID != "w1:p1" {
		t.Fatal(pane)
	}
	state := refreshFixture(t, f, refreshOptions{})
	tokens := f.reports[len(f.reports)-1]
	for key, want := range map[string]string{"gh_pr": "\uf407 #42", "gh_branch": "\uf418 feature/checks", "gh_passed": "1\uf42e", "gh_checks": "1 passed", "gh_review": "\uf41f 1", "gh_actions": "", "gh_unresolved": ""} {
		if tokens[key] != want {
			t.Errorf("%s: %q != %q", key, tokens[key], want)
		}
	}
	if state.Behind == nil || *state.Behind != 2 {
		t.Fatal(state)
	}
	for _, r := range f.requests(func(r commandRequest) bool { return r.Program == "gh" }) {
		if r.Cwd != f.root || r.Env["GH_PROMPT_DISABLED"] != "1" {
			t.Fatal(r)
		}
	}
	reports := f.requests(func(r commandRequest) bool { return slices.Contains(r.Args, "report-metadata") })
	if !slices.Contains(reports[0].Args, "300000") || !slices.Contains(reports[0].Args, "plugin:gh") {
		t.Fatal(reports)
	}
}

func TestCacheTTLAndInvalidation(t *testing.T) {
	f := newFixture(t)
	options := refreshOptions{Cached: true}
	refreshFixture(t, f, options)
	refreshFixture(t, f, options)
	if f.ghCount() != 4 || len(f.reports) != 2 {
		t.Fatal(f.ghCount(), len(f.reports))
	}
	f.now = f.now.Add(30 * time.Second)
	refreshFixture(t, f, options)
	if f.ghCount() != 8 {
		t.Fatal(f.ghCount())
	}
	for _, field := range []*string{&f.branch, &f.head, &f.config, &f.root} {
		*field += "-changed"
		refreshFixture(t, f, options)
	}
	if f.ghCount() != 24 {
		t.Fatal(f.ghCount())
	}
	refreshFixture(t, f, refreshOptions{})
	if f.ghCount() != 28 {
		t.Fatal(f.ghCount())
	}
}

func TestCacheHeadBaseAndLegacyFields(t *testing.T) {
	for _, field := range []string{"id", "url", "headRefOid", "baseRefName", "baseRefOid", "missing-id", "missing-base"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t)
			refreshFixture(t, f, refreshOptions{Cached: true})
			file := f.p.cacheFile("w1:p1")
			data, err := os.ReadFile(file)
			requireNoError(t, err)
			var cached State
			requireNoError(t, json.Unmarshal(data, &cached))
			pr := testPR()
			switch field {
			case "id":
				pr.ID = "PR_other"
			case "url":
				pr.URL = "https://github.example.com/owner/repo/pull/42"
			case "headRefOid":
				pr.HeadRefOID = "new-head"
			case "baseRefName":
				pr.BaseRefName = "release"
			case "baseRefOid":
				pr.BaseRefOID = "new-base"
			case "missing-id":
				cached.PR.ID = ""
			case "missing-base":
				cached.PR.BaseRefOID = ""
			}
			if !strings.HasPrefix(field, "missing-") {
				cached.PRAt -= 30001
			}
			requireNoError(t, writeJSON(file, cached))
			f.pr, f.compare = ok(pr), ok(map[string]int{"behind_by": 0})
			state := refreshFixture(t, f, refreshOptions{Cached: true})
			if state.Behind == nil || *state.Behind != 0 || len(f.requests(isCompare)) != 2 {
				t.Fatal(state)
			}
			if strings.HasPrefix(field, "base") && len(f.requests(func(r commandRequest) bool { return len(r.Args) > 1 && r.Args[0] == "pr" && r.Args[1] == "checks" })) != 1 {
				t.Fatal("base-only change discarded checks")
			}
		})
	}
}

func TestMissingPRAndLookupFailuresClearMetadata(t *testing.T) {
	for _, response := range []commandResult{failed(`no pull requests found for branch "feature/checks"`), failed("Authentication failed"), ok("not JSON"), ok(nil)} {
		t.Run(response.Stdout+response.Stderr, func(t *testing.T) {
			f := newFixture(t)
			refreshFixture(t, f, refreshOptions{})
			f.pr = response
			state := refreshFixture(t, f, refreshOptions{})
			for _, key := range []string{"gh_failed", "gh_passed", "gh_review", "gh_checks", "gh_updated"} {
				if tokensFor(*state)[key] != "" {
					t.Fatal(key, state)
				}
			}
			if state.Behind != nil || state.Unresolved != nil {
				t.Fatal(state)
			}
			if response.Stderr == "Authentication failed" {
				_, err := f.p.act(context.Background(), "w1:p1", "open", nil)
				requireError(t, err, "Authentication failed")
				if len(f.requests(isBrowser)) != 0 {
					t.Fatal("lookup error opened browser")
				}
			}
		})
	}
}

func TestUnavailableCheckouts(t *testing.T) {
	for _, kind := range []string{"non-Git", "detached", "unborn", "missing cwd"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			switch kind {
			case "non-Git":
				f.rootError = ptr(failed("fatal: not a git repository"))
			case "detached":
				f.branchError = ptr(failed(""))
			case "unborn":
				f.headError = ptr(failed("no commits"))
			case "missing cwd":
				f.pane.Cwd, f.pane.ForegroundCwd = "", nil
			}
			var progress []State
			state := refreshFixture(t, f, refreshOptions{OnCheckout: func(s State) { progress = append(progress, s) }})
			if state.Unavailable == "" || len(progress) != 1 || state.Identity != "" || f.ghCount() != 0 {
				t.Fatal(state, progress)
			}
			files, err := os.ReadDir(f.p.StateDir)
			requireNoError(t, err)
			if len(files) != 0 {
				t.Fatal(files)
			}
			if kind == "non-Git" {
				for _, value := range tokensFor(*state) {
					if value != "" {
						t.Fatal(value)
					}
				}
			}
		})
	}
}

func TestRefreshRejectsMovingTarget(t *testing.T) {
	for _, change := range []string{"branch", "terminal"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			f.before = func(_ context.Context, r commandRequest) error {
				if r.Program == "gh" && r.Args[0] == "pr" && r.Args[1] == "view" {
					f.mu.Lock()
					if change == "branch" {
						f.branch = "other"
					} else {
						f.pane.TerminalID = "term-2"
					}
					f.mu.Unlock()
				}
				return nil
			}
			state := refreshFixture(t, f, refreshOptions{})
			if !strings.Contains(state.Unavailable, "Repository changed") || f.reports[0]["gh_pr"] != "\uf407 refresh needed" {
				t.Fatal(state)
			}
			files, _ := os.ReadDir(f.p.StateDir)
			if len(files) != 0 {
				t.Fatal(files)
			}
		})
	}
}

func TestChecksStatusesAndFailures(t *testing.T) {
	for _, code := range []int{0, 1, 8} {
		f := newFixture(t)
		f.checks = ok([]Check{{Bucket: "fail"}, {Bucket: "cancel"}, {Bucket: "pending"}, {Bucket: "pass"}, {Bucket: "skipping"}, {Bucket: "future"}})
		f.checks.Code = code
		state := refreshFixture(t, f, refreshOptions{})
		if state.ChecksError != "" {
			t.Fatal(state)
		}
		for _, label := range checkOrder {
			if tokensFor(*state)["gh_"+label] != "1"+icons[label] {
				t.Fatal(label, tokensFor(*state))
			}
		}
	}
	f := newFixture(t)
	f.checks = failed("no checks reported on the 'feature/checks' branch")
	state := refreshFixture(t, f, refreshOptions{})
	if state.Checks == nil || len(state.Checks) != 0 || state.ChecksError != "" {
		t.Fatal(state)
	}
	state = refreshFixture(t, f, refreshOptions{Cached: true})
	if state.Checks == nil {
		t.Fatal("empty cached checks became unknown")
	}
	for _, response := range []commandResult{failed("Checks denied"), ok(nil), ok([]any{nil}), ok("not JSON")} {
		f.checks = response
		state = refreshFixture(t, f, refreshOptions{})
		if state.ChecksError == "" || tokensFor(*state)["gh_passed"] != "" || state.PR == nil {
			t.Fatal(state)
		}
	}
}

func TestReviewPaginationValidationAndHost(t *testing.T) {
	f := newFixture(t)
	pr := testPR()
	pr.URL = "https://github.example.com:443/owner/repo/pull/42"
	nodes := make([]any, 100)
	for i := range nodes {
		nodes[i] = map[string]bool{"isResolved": i < 60}
	}
	f.threads = ok([]any{threadPage(nodes, true), threadPage([]any{map[string]bool{"isResolved": false}}, false)})
	f.pr = ok(pr)
	state := refreshFixture(t, f, refreshOptions{})
	if state.Unresolved == nil || *state.Unresolved != 41 {
		t.Fatal(state)
	}
	requests := f.requests(func(r commandRequest) bool { return len(r.Args) > 1 && r.Args[1] == "graphql" })
	if !slices.Contains(requests[0].Args, "github.example.com") || !slices.Contains(requests[0].Args, "includeComments=false") {
		t.Fatal(requests)
	}
	for _, response := range []any{nil, []any{}, []any{threadPage([]any{}, true)}, []any{threadPage([]any{}, false), threadPage([]any{}, false)}, []any{threadPage([]any{map[string]any{}}, false)}, []any{map[string]any{"data": map[string]any{"node": nil}}}, []any{map[string]any{"errors": []any{map[string]string{"message": "Denied"}}}}} {
		f.threads = ok(response)
		state = refreshFixture(t, f, refreshOptions{})
		if state.ThreadsError == "" || state.Unresolved != nil || tokensFor(*state)["gh_review"] != "" || tokensFor(*state)["gh_passed"] == "" {
			t.Fatal(state)
		}
	}
}

func TestCompareValidationAndImmutableCommits(t *testing.T) {
	f := newFixture(t)
	pr := testPR()
	pr.URL = "https://github.example.com:443/Base-Owner/Repo.Name/pull/42"
	pr.BaseRefOID, pr.HeadRefOID = "base/sha?#", "head+sha%"
	_, err := f.p.compareBranch(context.Background(), pr, f.root)
	requireNoError(t, err)
	r := f.requests(isCompare)[0]
	if r.Args[len(r.Args)-1] != "repos/Base-Owner/Repo.Name/compare/base%2Fsha%3F%23...head%2Bsha%25?per_page=1" || !slices.Contains(r.Args, "{behind_by}") {
		t.Fatal(r)
	}
	for _, raw := range []string{`null`, `{}`, `[]`, `{"behind_by":null}`, `{"behind_by":-1}`, `{"behind_by":1.5}`, `{"behind_by":"2"}`, `broken`} {
		f.compare = ok(raw)
		_, err := f.p.compareBranch(context.Background(), pr, f.root)
		requireError(t, err, "comparison response")
	}
	for _, value := range []string{"file:///owner/repo/pull/42", "https://user:secret@github.com/owner/repo/pull/42", "https://github.com/owner/repo/pull/43", "https://github.example.com:8443/owner/repo/pull/42"} {
		pr.URL = value
		_, err := f.p.compareBranch(context.Background(), pr, f.root)
		if err == nil {
			t.Fatal(value)
		}
	}
}

func TestPartialStatusFailuresAndClosedPRs(t *testing.T) {
	f := newFixture(t)
	refreshFixture(t, f, refreshOptions{})
	f.compare = failed("Comparison unavailable")
	state := refreshFixture(t, f, refreshOptions{})
	if state.Behind != nil || state.BehindError == "" || tokensFor(*state)["gh_passed"] != "1\uf42e" {
		t.Fatal(state)
	}
	requireError(t, f.p.refreshAll(context.Background(), true), "w1:p1: Comparison unavailable")
	for _, status := range []string{"CLOSED", "MERGED"} {
		pr := testPR()
		pr.State = status
		f.pr = ok(pr)
		state = refreshFixture(t, f, refreshOptions{})
		if state.Unresolved != nil || state.Behind != nil || state.ThreadsError != "" || state.BehindError != "" {
			t.Fatal(state)
		}
		actFixture(t, f, "open", nil)
		if len(f.requests(isBrowser)) == 0 {
			t.Fatal("did not open closed PR")
		}
	}
}

func TestConcurrentRefreshAndIndependentActions(t *testing.T) {
	f := newFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.before = func(ctx context.Context, r commandRequest) error {
		if r.Program == "gh" && len(r.Args) > 1 && r.Args[0] == "pr" && r.Args[1] == "checks" {
			block := false
			once.Do(func() { close(started); block = true })
			if block {
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := f.p.refresh(context.Background(), "w1:p1", refreshOptions{}); done <- err }()
	<-started
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	})
	waitFor(t, func() bool { return f.ghCount() == 4 })
	state, err := f.p.refresh(context.Background(), "w1:p1", refreshOptions{Cached: true})
	requireNoError(t, err)
	if state != nil {
		t.Fatal("concurrent cached refresh was not skipped")
	}
	before := f.ghCount()
	for _, action := range []string{"open", "copy-slack", "open-checks"} {
		actFixture(t, f, action, nil)
	}
	if f.ghCount()-before != 6 {
		t.Fatal("actions fetched unrelated status", f.ghCount()-before)
	}
	f.mu.Lock()
	reports := len(f.reports)
	f.mu.Unlock()
	if reports != 0 {
		t.Fatal("actions overwrote background metadata")
	}
}

func TestRefreshCancellationReleasesLock(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{})
	f.before = func(ctx context.Context, r commandRequest) error {
		if r.Program == "gh" {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := f.p.refresh(ctx, "w1:p1", refreshOptions{}); done <- err }()
	<-started
	cancel()
	if !errors.Is(<-done, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	files, err := os.ReadDir(f.p.StateDir)
	requireNoError(t, err)
	if len(files) != 0 || len(f.reports) != 0 {
		t.Fatal(files, f.reports)
	}
}

func TestCheckoutProgressBeforeGitHub(t *testing.T) {
	f := newFixture(t)
	var progress State
	refreshFixture(t, f, refreshOptions{OnCheckout: func(s State) {
		if f.ghCount() != 0 {
			t.Error("checkout arrived after GitHub request")
		}
		progress = s
	}})
	if progress.Branch != f.branch || progress.Cwd != f.root || len(progress.Identity) != 64 {
		t.Fatal(progress)
	}
}

func TestSettingsPersistenceAndInvalidFiles(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	enabled, err := f.p.settings(ctx, nil)
	requireNoError(t, err)
	if enabled {
		t.Fatal("default enabled")
	}
	_, err = f.p.settings(ctx, ptr(true))
	requireNoError(t, err)
	next := newPlugin(f.p.Env)
	enabled, err = next.settings(ctx, nil)
	requireNoError(t, err)
	if !enabled {
		t.Fatal("not persisted")
	}
	file := filepath.Join(f.p.Env["HERDR_PLUGIN_CONFIG_DIR"], "settings.json")
	for _, invalid := range []string{"not json", "null", "[]", `{"markdown_mode":"false"}`, `{"markdown_mode":null}`} {
		requireNoError(t, os.WriteFile(file, []byte(invalid), 0600))
		_, err := f.p.settings(ctx, ptr(false))
		requireError(t, err, "settings.json")
		actual, _ := os.ReadFile(file)
		if string(actual) != invalid {
			t.Fatal("invalid settings overwritten")
		}
	}
	requireNoError(t, os.WriteFile(file, []byte(`{"markdown_mode":false,"custom":"kept"}`), 0600))
	_, err = f.p.settings(ctx, ptr(true))
	requireNoError(t, err)
	actual, _ := os.ReadFile(file)
	if !bytes.Contains(actual, []byte(`"custom": "kept"`)) {
		t.Fatal(string(actual))
	}
}

func TestSlackPayload(t *testing.T) {
	for _, state := range []string{"OPEN", "CLOSED", "MERGED", "DRAFT"} {
		pr := testPR()
		pr.State = state
		pr.IsDraft = state == "DRAFT"
		emoji := map[string]string{"OPEN": ":pr:", "CLOSED": ":pr-closed:", "MERGED": ":pr-merged:", "DRAFT": ":pr-draft:"}[state]
		payload, err := slackPRPayload(pr, false)
		requireNoError(t, err)
		if payload.HTML != emoji+` <b>owner/repo</b> · <a href="`+pr.URL+`">Add Slack copy (#42)</a> +12/-3` {
			t.Fatal(payload)
		}
		payload, err = slackPRPayload(pr, true)
		requireNoError(t, err)
		if payload.HTML != "" || payload.Plain != emoji+" *owner/repo* · [Add Slack copy (#42)]("+pr.URL+")*\u2060* +12/-3" {
			t.Fatal(payload)
		}
	}
	pr := testPR()
	pr.URL += "/files?diff=split#anchor"
	pr.Title = "\x1b[31mFix [x] & <y>\x1b[0m\nnext"
	payload, err := slackPRPayload(pr, false)
	requireNoError(t, err)
	if !strings.Contains(payload.HTML, "Fix [x] &amp; &lt;y&gt; next (#42)") || strings.Contains(payload.HTML, "/files") {
		t.Fatal(payload)
	}
	payload, err = slackPRPayload(pr, true)
	requireNoError(t, err)
	if !strings.Contains(payload.Plain, `Fix \[x\] & <y> next`) {
		t.Fatal(payload)
	}
	for _, value := range []string{"javascript:alert(1)", "https://user:secret@github.com/o/r/pull/1", "https://github.com/o/r/issues/1", "https://github.com/o/r/pull/42wrong"} {
		pr.URL = value
		_, err := slackPRPayload(pr, false)
		requireError(t, err, "Invalid GitHub PR URL")
	}
}

func TestClipboardPlatformsAndFormats(t *testing.T) {
	for _, desktop := range []string{"macOS", "Wayland", "X11", "headless"} {
		t.Run(desktop, func(t *testing.T) {
			f := newFixture(t)
			program := "/usr/bin/osascript"
			if desktop != "macOS" {
				f.p.Platform = "linux"
			}
			if desktop == "Wayland" {
				f.p.Env["WAYLAND_DISPLAY"], program = "wayland-0", "wl-copy"
			}
			if desktop == "X11" {
				f.p.Env["DISPLAY"], program = ":0", "xclip"
			}
			if desktop == "headless" {
				_, err := f.p.act(context.Background(), "w1:p1", "copy-slack", nil)
				requireError(t, err, "desktop clipboard")
				return
			}
			actFixture(t, f, "copy-slack", nil)
			calls := f.requests(nil)
			last := calls[len(calls)-1]
			if last.Program != program || !strings.Contains(last.Input, "Add Slack copy") {
				t.Fatal(last)
			}
			if desktop == "macOS" {
				var data clipboardPayload
				requireNoError(t, json.Unmarshal([]byte(last.Input), &data))
				if data.HTML == "" || data.Plain == "" {
					t.Fatal(data)
				}
			}
			_, err := f.p.settings(context.Background(), ptr(true))
			requireNoError(t, err)
			actFixture(t, f, "copy-slack", nil)
			calls = f.requests(nil)
			last = calls[len(calls)-1]
			if desktop == "macOS" && last.Program != "/usr/bin/pbcopy" {
				t.Fatal(last)
			}
			if desktop != "macOS" && !slices.Contains(last.Args, "text/plain") {
				t.Fatal(last)
			}
			f.clipboardError = ptr(failed("Clipboard unavailable"))
			_, err = f.p.act(context.Background(), "w1:p1", "copy-slack", nil)
			requireError(t, err, "Could not copy PR")
		})
	}
}

func TestOpenChecksFreshFailuresAndFallback(t *testing.T) {
	for _, checks := range []commandResult{
		ok([]Check{{Bucket: "fail", Link: "https://ci.example.com/build/123"}}),
		ok([]Check{{Bucket: "fail", Link: "javascript:alert(1)"}, {Bucket: "fail", Link: "https://user:secret@ci.example.com/build"}}),
		ok([]Check{{Bucket: "pass"}}), failed("Network unavailable"), failed("no checks reported on the 'branch' branch"),
	} {
		f := newFixture(t)
		refreshFixture(t, f, refreshOptions{})
		f.checks = checks
		actFixture(t, f, "open-checks", nil)
		calls := f.requests(isBrowser)
		last := calls[len(calls)-1]
		if strings.Contains(checks.Stdout, "https://ci.example.com/build/123") {
			if last.Program != "/usr/bin/open" {
				t.Fatal(last)
			}
		} else if !reflect.DeepEqual(last.Args, []string{"pr", "checks", testPR().URL, "--web"}) {
			t.Fatal(last)
		}
		if f.reports[0]["gh_passed"] != "1\uf42e" || len(f.reports) != 1 {
			t.Fatal("action wrote status")
		}
	}
}

func TestCreatePublishedBranchAndFork(t *testing.T) {
	f := newFixture(t)
	f.pr = failed(`no pull requests found for branch "feature/checks"`)
	f.upstream = "\x00"
	_, err := f.p.act(context.Background(), "w1:p1", "open", nil)
	requireError(t, err, "upstream")
	f.upstream, f.ahead = "origin\x00refs/heads/feature/checks", "1"
	_, err = f.p.act(context.Background(), "w1:p1", "open", nil)
	requireError(t, err, "new commits")
	f.ahead = "0"
	for _, head := range []string{"owner/repo", "contributor/fork"} {
		f.headRepo = head
		actFixture(t, f, "open", nil)
		calls := f.requests(isBrowser)
		last := calls[len(calls)-1]
		want := "--head=feature/checks"
		if head != "owner/repo" {
			want = "--head=contributor:feature/checks"
		}
		if !slices.Contains(last.Args, want) {
			t.Fatal(last)
		}
	}
}

func TestCommentsLinksAndPagination(t *testing.T) {
	f := newFixture(t)
	link := testPR().URL + "/files#discussion_r123"
	invalid := []string{"javascript:alert(1)", "https://other.example.com/owner/repo/pull/42#r1", testPR().URL + "#issuecomment-1", testPR().URL + "/commits#r1", testPR().URL + "#r0", "https://user:secret@github.com/owner/repo/pull/42#r1"}
	var nodes []any
	for _, value := range invalid {
		nodes = append(nodes, map[string]any{"isResolved": false, "comments": map[string]any{"nodes": []any{map[string]string{"url": value}}}})
	}
	valid := map[string]any{"isResolved": false, "comments": map[string]any{"nodes": []any{map[string]string{"url": link}}}}
	f.threads = ok([]any{threadPage(nodes, true), threadPage([]any{valid}, false)})
	actFixture(t, f, "open-comments", nil)
	if got := f.requests(isBrowser); len(got) != 1 || got[0].Args[0] != link {
		t.Fatal(got)
	}
	f.threads = ok([]any{threadPage(nodes, false)})
	_, err := f.p.act(context.Background(), "w1:p1", "open-comments", nil)
	requireError(t, err, "No usable link")
	f.threads = ok([]any{threadPage([]any{}, false)})
	_, err = f.p.act(context.Background(), "w1:p1", "open-comments", nil)
	requireError(t, err, "No unresolved")
}

func TestMutationsRequireConfirmationAndRevalidation(t *testing.T) {
	for _, action := range []string{"rerun-failed", "update-branch"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			f.checks = ok([]Check{{Bucket: "fail", Link: "https://github.com/owner/repo/actions/runs/1/job/2"}})
			_, err := f.p.act(context.Background(), "w1:p1", action, nil)
			requireError(t, err, "requires confirmation")
			if len(f.requests(nil)) != 0 {
				t.Fatal("unconfirmed action started lookup")
			}
			actFixture(t, f, action, decline)
			if len(f.requests(isUpdate))+len(f.requests(isRerun)) != 0 {
				t.Fatal("cancellation mutated state")
			}
			for _, change := range []string{"branch", "terminal", "head", "base", "remote-head", "PR-state", "attempt"} {
				if action == "rerun-failed" && change == "base" || action == "update-branch" && change == "attempt" {
					continue
				}
				t.Run(change, func(t *testing.T) {
					f := newFixture(t)
					f.checks = ok([]Check{{Bucket: "fail", Link: "https://github.com/owner/repo/actions/runs/1"}})
					_, err := f.p.act(context.Background(), "w1:p1", action, func(context.Context, confirmationDetails) (bool, error) {
						switch change {
						case "branch":
							f.branch = "other"
						case "terminal":
							f.pane.TerminalID = "term-2"
						case "head":
							f.head = "other"
						case "base", "remote-head", "PR-state":
							pr := testPR()
							if change == "base" {
								pr.BaseRefOID = "other"
							}
							if change == "remote-head" {
								pr.HeadRefOID = "other"
							}
							if change == "PR-state" {
								pr.State = "CLOSED"
							}
							f.pr = ok(pr)
						case "attempt":
							run := testRun(1)
							run.Attempt++
							f.runs["1"] = ok(run)
						}
						return true, nil
					})
					if err == nil || len(f.requests(isUpdate))+len(f.requests(isRerun)) != 0 {
						t.Fatal(err, "changed target mutated")
					}
				})
			}
		})
	}
}

func TestUpdateBranchPinnedAndNoOp(t *testing.T) {
	f := newFixture(t)
	pr := testPR()
	pr.URL = "https://github.example.com/Base-Owner/Repo.Name/pull/42"
	f.pr = ok(pr)
	state := actFixture(t, f, "update-branch", func(_ context.Context, details confirmationDetails) (bool, error) {
		if details.Number != 42 || details.Base != "main" || details.Behind != 2 || len(f.requests(isUpdate)) != 0 {
			t.Fatal(details)
		}
		return true, nil
	})
	if state.BranchUpdate != "requested" {
		t.Fatal(state)
	}
	updates := f.requests(isUpdate)
	want := []string{"api", "--hostname", "github.example.com", "--method", "PUT", "repos/Base-Owner/Repo.Name/pulls/42/update-branch", "-f", "expected_head_sha=remote-head"}
	if len(updates) != 1 || !reflect.DeepEqual(updates[0].Args, want) {
		t.Fatal(updates)
	}
	f.compare = ok(map[string]int{"behind_by": 0})
	state = actFixture(t, f, "update-branch", func(context.Context, confirmationDetails) (bool, error) {
		t.Error("up-to-date branch prompted")
		return false, nil
	})
	if state.BranchUpdate != "up-to-date" || len(f.requests(isUpdate)) != 1 {
		t.Fatal(state)
	}
	for _, message := range []string{"HTTP 403: Permission denied", "HTTP 422: Merge conflict", "HTTP 409: Head changed"} {
		f.compare, f.branchUpdate = ok(map[string]int{"behind_by": 2}), failed(message)
		_, err := f.p.act(context.Background(), "w1:p1", "update-branch", approve)
		requireError(t, err, message)
	}
}

func TestRerunDeduplicationEligibilityAndRepository(t *testing.T) {
	f := newFixture(t)
	pr := testPR()
	pr.URL = "https://github.example.com:443/Base-Owner/Repo.Name/pull/42"
	f.pr = ok(pr)
	base := "https://github.example.com/Base-Owner/Repo.Name/actions/runs/"
	f.checks = ok([]Check{{Bucket: "fail", Link: base + "1/job/10"}, {Bucket: "fail", Link: base + "1/jobs/11"}, {Bucket: "fail", Link: base + "1/attempts/2"}, {Bucket: "fail", Link: base + "2"}, {Bucket: "fail", Link: base + "3"}, {Bucket: "pass", Link: base + "4"}, {Bucket: "fail", Link: "https://ci.example.com/build/1"}})
	run := testRun(2)
	run.Conclusion = "startup_failure"
	f.runs["2"] = ok(run)
	run = testRun(3)
	run.HeadSHA = "merge-head"
	f.runs["3"] = ok(map[string]any{"id": 3, "run_attempt": 2, "head_sha": "merge-head", "status": "completed", "conclusion": "failure", "pull_requests": []any{map[string]any{"number": 42, "head": map[string]string{"sha": "remote-head"}}}})
	state := actFixture(t, f, "rerun-failed", func(_ context.Context, details confirmationDetails) (bool, error) {
		if details.Runs != 2 || len(f.requests(isRerun)) != 0 {
			t.Fatal(details)
		}
		return true, nil
	})
	if state.RerunCount != 2 {
		t.Fatal(state)
	}
	runs := f.requests(isRerun)
	if len(runs) != 2 || runs[0].Args[2] != "1" || runs[1].Args[2] != "3" || runs[0].Args[5] != "github.example.com/Base-Owner/Repo.Name" {
		t.Fatal(runs)
	}
}

func TestRerunPartialFailure(t *testing.T) {
	f := newFixture(t)
	f.checks = ok([]Check{{Bucket: "fail", Link: "https://github.com/owner/repo/actions/runs/1"}, {Bucket: "fail", Link: "https://github.com/owner/repo/actions/runs/2"}})
	f.runs["2"] = ok(testRun(2))
	execute := f.p.Execute
	f.p.Execute = func(ctx context.Context, r commandRequest) (commandResult, error) {
		result, err := execute(ctx, r)
		if isRerun(r) && r.Args[2] == "2" {
			return failed("HTTP 403: Permission denied"), nil
		}
		return result, err
	}
	_, err := f.p.act(context.Background(), "w1:p1", "rerun-failed", approve)
	requireError(t, err, "Rerun requested for 1/2 runs. HTTP 403")
}

func TestActionCancellationAndInvalidPorts(t *testing.T) {
	for _, action := range []string{"open-comments", "rerun-failed", "update-branch"} {
		f := newFixture(t)
		pr := testPR()
		pr.URL = "https://github.example.com:8443/owner/repo/pull/42"
		f.pr = ok(pr)
		_, err := f.p.act(context.Background(), "w1:p1", action, approve)
		requireError(t, err, "custom GitHub ports")
		if len(f.requests(isBrowser))+len(f.requests(isUpdate))+len(f.requests(isRerun)) != 0 {
			t.Fatal("invalid host caused side effects")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		before := len(f.requests(nil))
		_, err = f.p.act(ctx, "w1:p1", action, approve)
		if !errors.Is(err, context.Canceled) || len(f.requests(nil)) != before {
			t.Fatal(err)
		}
	}
}

func TestRerunRejectsInvalidAndUnrelatedRuns(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `{"id":2,"run_attempt":2,"head_sha":"remote-head"}`,
		`{"id":1,"run_attempt":0,"head_sha":"remote-head"}`,
		`{"id":1,"run_attempt":"2","head_sha":"remote-head"}`,
		`{"id":1,"run_attempt":1.5,"head_sha":"remote-head"}`,
		`{"id":1,"run_attempt":2,"head_sha":""}`, `not JSON`,
	} {
		t.Run(raw, func(t *testing.T) {
			f := newFixture(t)
			f.checks = ok([]Check{{Bucket: "fail", Link: "https://github.com/owner/repo/actions/runs/1"}})
			f.runs["1"] = ok(raw)
			_, err := f.p.act(context.Background(), "w1:p1", "rerun-failed", func(context.Context, confirmationDetails) (bool, error) {
				t.Error("malformed run reached confirmation")
				return true, nil
			})
			requireError(t, err, "Actions run response")
			if len(f.requests(isRerun)) != 0 {
				t.Fatal("malformed run was rerun")
			}
		})
	}
	for _, change := range []string{"running", "success", "canceled", "startup_failure", "other-head"} {
		f := newFixture(t)
		f.checks = ok([]Check{{Bucket: "fail", Link: "https://github.com/owner/repo/actions/runs/1"}})
		run := testRun(1)
		switch change {
		case "running":
			run.Status = "in_progress"
		case "other-head":
			run.HeadSHA = "other-head"
		default:
			run.Conclusion = change
		}
		f.runs["1"] = ok(run)
		_, err := f.p.act(context.Background(), "w1:p1", "rerun-failed", approve)
		requireError(t, err, "No completed failed")
	}
}

func TestRerunRejectsUnsafeLinks(t *testing.T) {
	f := newFixture(t)
	var checks []Check
	for _, link := range []string{
		"https://ci.example.com/build/1", "http://github.com/owner/repo/actions/runs/1",
		"https://github.com:8443/owner/repo/actions/runs/1", "https://user:secret@github.com/owner/repo/actions/runs/1",
		"https://github.com/other/repo/actions/runs/1", "https://github.com/owner/repo/actions/runs/0",
		"https://github.com/owner/repo/actions/runs/1/other/2", "https://github.com/owner/repo/actions/runs/1/jobs/2/extra",
		"javascript:alert(1)", "file:///tmp/run", "not a URL",
	} {
		checks = append(checks, Check{Bucket: "fail", Link: link})
	}
	f.checks = ok(checks)
	_, err := f.p.act(context.Background(), "w1:p1", "rerun-failed", approve)
	requireError(t, err, "No completed failed")
	if len(f.requests(func(r commandRequest) bool { return r.Program == "gh" && r.Args[0] == "api" })) != 0 || len(f.requests(isRerun)) != 0 {
		t.Fatal("unsafe link reached GitHub Actions API")
	}
}

func TestMutationsCheckTargetAfterFinalRead(t *testing.T) {
	for _, action := range []string{"rerun-failed", "update-branch"} {
		for _, change := range []string{"branch", "terminal", "abort"} {
			t.Run(action+"/"+change, func(t *testing.T) {
				f := newFixture(t)
				f.checks = ok([]Check{{Bucket: "fail", Link: "https://github.com/owner/repo/actions/runs/1"}})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				reads := 0
				f.before = func(_ context.Context, r commandRequest) error {
					finalRead := action == "rerun-failed" && r.Program == "gh" && r.Args[0] == "api" && strings.Contains(r.Args[len(r.Args)-1], "/actions/runs/")
					if action == "update-branch" {
						finalRead = r.Program == "gh" && r.Args[0] == "pr" && r.Args[1] == "view"
					}
					if finalRead {
						reads++
						if reads == 2 {
							f.mu.Lock()
							switch change {
							case "branch":
								f.branch = "other"
							case "terminal":
								f.pane.TerminalID = "term-2"
							case "abort":
								cancel()
							}
							f.mu.Unlock()
						}
					}
					return nil
				}
				_, err := f.p.act(ctx, "w1:p1", action, approve)
				if err == nil || len(f.requests(isRerun))+len(f.requests(isUpdate)) != 0 {
					t.Fatal("target change after approval was ignored", err)
				}
				if change == "abort" && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestStaleLocksAndCorruptCachesRecover(t *testing.T) {
	f := newFixture(t)
	file := f.p.cacheFile("w1:p1")
	requireNoError(t, os.Mkdir(file+".lock", 0700))
	old := time.Now().Add(-6 * time.Minute)
	requireNoError(t, os.Chtimes(file+".lock", old, old))
	requireNoError(t, os.WriteFile(file, []byte("broken JSON"), 0600))
	state := refreshFixture(t, f, refreshOptions{Cached: true})
	if state.PR == nil || state.PR.Number != 42 {
		t.Fatal(state)
	}
	if _, err := os.Stat(file + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("lock not released", err)
	}
}

func TestTargetPriorityAndCLI(t *testing.T) {
	f := newFixture(t)
	f.p.Env["HERDR_GH_PANE_ID"] = "w2:p9"
	f.p.Env["HERDR_PANE_ID"] = "wrong"
	_, err := f.p.target(context.Background())
	requireNoError(t, err)
	if !reflect.DeepEqual(f.requests(nil)[0].Args, []string{"pane", "get", "w2:p9"}) {
		t.Fatal(f.requests(nil)[0])
	}
	for _, args := range [][]string{{"--help"}, {"refresh"}, {"refresh", "--cached", "--silent"}, {"menu"}} {
		var stdout, stderr bytes.Buffer
		if code := cli(context.Background(), f.p, args, &stdout, &stderr); code != 0 {
			t.Fatal(code, stderr.String())
		}
		if slices.Contains(args, "--silent") && stdout.Len() != 0 {
			t.Fatal(stdout.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if cli(context.Background(), f.p, []string{"unknown"}, &stdout, &stderr) != 1 || !strings.Contains(stderr.String(), "Unknown action") {
		t.Fatal(stderr.String())
	}
}

func TestRealGitWorktrees(t *testing.T) {
	directory := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		_, err := checked(runCommand(context.Background(), commandRequest{Program: "git", Args: args, Cwd: directory}))
		requireNoError(t, err)
	}
	git("init", "--initial-branch=main")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "test")
	worktree := filepath.Join(directory, "linked worktree")
	git("worktree", "add", "-b", "feature/worktree", worktree)
	p := newPlugin(nil)
	main, err := p.checkout(context.Background(), Pane{Cwd: directory})
	requireNoError(t, err)
	linked, err := p.checkout(context.Background(), Pane{Cwd: directory, ForegroundCwd: &worktree})
	requireNoError(t, err)
	if main.Branch != "main" || linked.Branch != "feature/worktree" || main.Identity == linked.Identity || !strings.HasSuffix(linked.Cwd, "linked worktree") {
		t.Fatal(main, linked)
	}
}

func TestCommandRunner(t *testing.T) {
	if os.Getenv("HERDR_GH_TEST_HELPER") == "1" {
		switch os.Args[len(os.Args)-1] {
		case "exit":
			fmt.Print("$(touch should-not-exist)")
			os.Exit(8)
		case "sleep":
			time.Sleep(time.Minute)
		}
		return
	}
	env := environment()
	env["HERDR_GH_TEST_HELPER"] = "1"
	r, err := runCommand(context.Background(), commandRequest{Program: os.Args[0], Args: []string{"-test.run=TestCommandRunner", "exit"}, Env: env})
	requireNoError(t, err)
	if r.Code != 8 || r.Stdout != "$(touch should-not-exist)" {
		t.Fatal(r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = runCommand(ctx, commandRequest{Program: os.Args[0], Args: []string{"-test.run=TestCommandRunner", "sleep"}, Env: env})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	_, err = runCommand(context.Background(), commandRequest{Program: "/definitely-missing-herdr-gh-command"})
	if err == nil {
		t.Fatal("missing command succeeded")
	}
}
