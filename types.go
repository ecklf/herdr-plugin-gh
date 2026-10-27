package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	pluginID    = "gh"
	cacheTTL    = 30 * time.Second
	metadataTTL = 5 * time.Minute
	prFields    = "id,number,url,state,headRefOid,baseRefName,baseRefOid,title,isDraft,additions,deletions"
)

type PullRequest struct {
	ID          string `json:"id"`
	Number      int64  `json:"number"`
	URL         string `json:"url"`
	State       string `json:"state"`
	HeadRefOID  string `json:"headRefOid"`
	BaseRefName string `json:"baseRefName"`
	BaseRefOID  string `json:"baseRefOid"`
	Title       string `json:"title"`
	IsDraft     bool   `json:"isDraft"`
	Additions   *int64 `json:"additions,omitempty"`
	Deletions   *int64 `json:"deletions,omitempty"`
}

type Check struct {
	Name   string `json:"name"`
	Bucket string `json:"bucket"`
	Link   string `json:"link"`
}

type Pane struct {
	ID            string            `json:"pane_id"`
	TerminalID    string            `json:"terminal_id"`
	Cwd           string            `json:"cwd"`
	ForegroundCwd *string           `json:"foreground_cwd"`
	Agent         any               `json:"agent"`
	Tokens        map[string]string `json:"tokens"`
}

// JSON field names retain compatibility with caches written by the original plugin.
type State struct {
	githubHost   string
	Identity     string       `json:"identity,omitempty"`
	Branch       string       `json:"branch,omitempty"`
	Cwd          string       `json:"cwd,omitempty"`
	Unavailable  string       `json:"unavailable,omitempty"`
	PR           *PullRequest `json:"pr"`
	PRAt         int64        `json:"prAt,omitempty"`
	PRError      string       `json:"prError,omitempty"`
	Checks       []Check      `json:"checks"`
	ChecksAt     int64        `json:"checksAt,omitempty"`
	ChecksError  string       `json:"checksError,omitempty"`
	Unresolved   *int64       `json:"unresolved,omitempty"`
	ThreadsAt    int64        `json:"threadsAt,omitempty"`
	ThreadsError string       `json:"threadsError,omitempty"`
	Behind       *int64       `json:"behind,omitempty"`
	BehindAt     int64        `json:"behindAt,omitempty"`
	BehindError  string       `json:"behindError,omitempty"`
	RerunCount   int          `json:"-"`
	BranchUpdate string       `json:"-"`
}

func (s State) problem() string {
	for _, message := range []string{s.Unavailable, s.PRError, s.ChecksError, s.ThreadsError, s.BehindError} {
		if message != "" {
			return message
		}
	}
	return ""
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Remove terminal control sequences before rendering data from Git or GitHub.
var ansiPattern = regexp.MustCompile(`(?:\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[P^_X][\s\S]*?\x1b\\|(?:\x1b\[|\x{009b})[0-?]*[ -/]*[@-~]|\x1b[ -/]*[@-~])`)

func clean(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || (r >= 127 && r <= 159) {
			return ' '
		}
		return r
	}, ansiPattern.ReplaceAllString(value, ""))
}

var icons = map[string]string{
	"branch": "\uf418", "pr": "\uf407", "failed": "\uf52f", "canceled": "\uf46e",
	"pending": "\uf43a", "passed": "\uf42e", "skipped": "\uf517", "unknown": "\ueb32", "comment": "\uf41f",
}

var checkOrder = []string{"failed", "pending", "passed", "canceled", "skipped", "unknown"}
var tokenOrder = []string{"gh_branch", "gh_pr", "gh_actions", "gh_checks", "gh_updated", "gh_failed", "gh_canceled", "gh_pending", "gh_passed", "gh_skipped", "gh_unknown", "gh_review", "gh_unresolved"}

func checkCounts(checks []Check) map[string]int {
	counts := make(map[string]int)
	labels := map[string]string{"fail": "failed", "cancel": "canceled", "pending": "pending", "pass": "passed", "skipping": "skipped"}
	for _, check := range checks {
		label := labels[check.Bucket]
		if label == "" {
			label = "unknown"
		}
		counts[label]++
	}
	return counts
}

// Empty values clear existing metadata rather than publishing stale counters.
func tokensFor(state State) map[string]string {
	tokens := make(map[string]string, len(tokenOrder))
	for _, key := range tokenOrder {
		tokens[key] = ""
	}
	if state.Unavailable == "Not a Git repository" {
		return tokens
	}
	if state.Branch != "" {
		tokens["gh_branch"] = icons["branch"] + " " + state.Branch
	}
	if state.Unavailable != "" {
		tokens["gh_pr"] = icons["pr"] + " " + strings.TrimPrefix(state.Unavailable, "PR: ")
		return tokens
	}
	if state.PRError != "" {
		tokens["gh_pr"] = icons["pr"] + " unavailable"
		return tokens
	}
	if state.PR == nil {
		return tokens
	}
	tokens["gh_pr"] = fmt.Sprintf("%s #%d", icons["pr"], state.PR.Number)
	if state.PR.State == "OPEN" && state.ThreadsError == "" && state.Unresolved != nil && *state.Unresolved >= 0 {
		tokens["gh_review"] = fmt.Sprintf("%s %d", icons["comment"], *state.Unresolved)
	}
	if state.ChecksError == "" {
		counts := checkCounts(state.Checks)
		for label, count := range counts {
			tokens["gh_"+label] = fmt.Sprintf("%d%s", count, icons[label])
		}
		for _, label := range []string{"failed", "pending", "passed"} {
			if count := counts[label]; count > 0 {
				tokens["gh_checks"] = fmt.Sprintf("%d %s", count, label)
				break
			}
		}
	}
	if state.PRAt != 0 {
		tokens["gh_updated"] = icons["pr"] + " " + time.UnixMilli(state.PRAt).UTC().Format("15:04") + " UTC"
	}
	return tokens
}
