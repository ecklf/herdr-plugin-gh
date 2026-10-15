package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type refreshOptions struct {
	Cached, AgentOnly, Transient, Compare, PROnly bool
	OnCheckout                                    func(State)
}

func (p *Plugin) cacheFile(paneID string) string {
	session := p.Env["HERDR_SOCKET_PATH"]
	if session == "" {
		session = p.Env["HERDR_SESSION"]
	}
	if session == "" {
		session = "default"
	}
	return filepath.Join(p.StateDir, hash(session+":"+paneID)+".json")
}

// A directory lock also serializes refreshes from independent Herdr hook processes.
func acquireLock(ctx context.Context, lock string, cached bool) (bool, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		err := os.Mkdir(lock, 0700)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return false, err
		}
		info, err := os.Stat(lock)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if time.Since(info.ModTime()) > metadataTTL {
			if err := os.RemoveAll(lock); err != nil {
				return false, err
			}
			continue
		}
		if cached {
			return false, nil
		}
		if time.Now().After(deadline) {
			return false, errors.New("A GitHub refresh is already running for this Agent. Try again shortly.")
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func samePR(a, b *PullRequest) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID && a.URL == b.URL && a.HeadRefOID == b.HeadRefOID
}

func sameBase(a, b *PullRequest) bool {
	if !samePR(a, b) {
		return false
	}
	return a == nil || (a.BaseRefName == b.BaseRefName && a.BaseRefOID == b.BaseRefOID)
}

func (p *Plugin) refresh(ctx context.Context, paneID string, options refreshOptions) (*State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file := p.cacheFile(paneID)
	if !options.Transient {
		if err := os.MkdirAll(p.StateDir, 0700); err != nil {
			return nil, err
		}
		locked, err := acquireLock(ctx, file+".lock", options.Cached)
		if err != nil || !locked {
			return nil, err
		}
		defer os.RemoveAll(file + ".lock")
	}
	pane, err := p.pane(ctx, paneID)
	if err != nil {
		return nil, err
	}
	if options.AgentOnly && (pane.Agent == nil || pane.Agent == "" || pane.Agent == false) {
		return nil, nil
	}
	checkout, err := p.checkout(ctx, pane)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx = withGitHubHost(ctx, checkout.githubHost)
	if options.OnCheckout != nil {
		options.OnCheckout(checkout)
	}
	if checkout.Unavailable != "" {
		if !options.Transient {
			if err := p.report(ctx, paneID, checkout); err != nil {
				return nil, err
			}
			if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		return &checkout, nil
	}
	state := State{Identity: checkout.Identity, Branch: checkout.Branch}
	if !options.Transient {
		data, err := os.ReadFile(file)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		var previous State
		if err == nil && json.Unmarshal(data, &previous) == nil && previous.Identity == checkout.Identity {
			state = previous
		}
	}
	fresh := func(at int64) bool {
		return options.Cached && at != 0 && p.Now().UnixMilli()-at < cacheTTL.Milliseconds()
	}
	legacyPR := state.PR != nil && (state.PR.ID == "" || (state.PR.State == "OPEN" && (state.PR.BaseRefName == "" || state.PR.BaseRefOID == "")))
	if !fresh(state.PRAt) || legacyPR {
		pr, err := p.readPR(ctx, checkout.Cwd)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			state.PRError, state.PR = err.Error(), nil
			state.Checks, state.ChecksAt = nil, 0
			state.Unresolved, state.ThreadsAt, state.ThreadsError = nil, 0, ""
		} else {
			if !sameBase(state.PR, pr) {
				state.Behind, state.BehindAt, state.BehindError = nil, 0, ""
			}
			if !samePR(state.PR, pr) {
				state.Checks, state.ChecksAt, state.ChecksError = nil, 0, ""
				state.Unresolved, state.ThreadsAt, state.ThreadsError = nil, 0, ""
			}
			state.PR, state.PRError = pr, ""
		}
		state.PRAt = p.Now().UnixMilli()
	}
	if state.PR == nil || state.PR.State != "OPEN" {
		state.Unresolved, state.ThreadsAt, state.ThreadsError = nil, 0, ""
		state.Behind, state.BehindAt, state.BehindError = nil, 0, ""
	}
	// These reads all depend on the PR, but not on one another. Collect updates
	// on this goroutine so one slow endpoint doesn't serialize the entire load.
	pr := state.PR
	var lookups []func() func(*State)
	if pr != nil && pr.State == "OPEN" {
		if !options.Transient && !fresh(state.ThreadsAt) {
			lookups = append(lookups, func() func(*State) {
				threads, err := p.reviewThreads(ctx, pr, checkout.Cwd, false)
				at := p.Now().UnixMilli()
				return func(state *State) {
					if err != nil {
						state.Unresolved, state.ThreadsError = nil, err.Error()
					} else {
						var count int64
						for _, thread := range threads {
							if !*thread.IsResolved {
								count++
							}
						}
						state.Unresolved, state.ThreadsError = &count, ""
					}
					state.ThreadsAt = at
				}
			})
		}
		if (!options.Transient || options.Compare) && !fresh(state.BehindAt) {
			lookups = append(lookups, func() func(*State) {
				behind, err := p.compareBranch(ctx, pr, checkout.Cwd)
				at := p.Now().UnixMilli()
				return func(state *State) {
					if err != nil {
						state.Behind, state.BehindError = nil, err.Error()
					} else {
						state.Behind, state.BehindError = &behind, ""
					}
					state.BehindAt = at
				}
			})
		}
	}
	if !options.PROnly && pr != nil && !fresh(state.ChecksAt) {
		lookups = append(lookups, func() func(*State) {
			checks, err := p.readChecks(ctx, pr, checkout.Cwd)
			at := p.Now().UnixMilli()
			return func(state *State) {
				if err != nil {
					state.Checks, state.ChecksError = nil, err.Error()
				} else {
					state.Checks, state.ChecksError = checks, ""
				}
				state.ChecksAt = at
			}
		})
	}
	updates := make(chan func(*State), len(lookups))
	var requests sync.WaitGroup
	defer requests.Wait()
	for _, lookup := range lookups {
		requests.Add(1)
		go func() { defer requests.Done(); updates <- lookup() }()
	}
	for range lookups {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case update := <-updates:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			update(&state)
		}
	}
	current, err := p.pane(ctx, paneID)
	if err != nil {
		return nil, err
	}
	currentCheckout, err := p.checkout(ctx, current)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if current.TerminalID != pane.TerminalID || currentCheckout.Identity != checkout.Identity {
		if !options.Transient {
			if currentCheckout.Unavailable == "" {
				currentCheckout.Unavailable = "PR: refresh needed"
			}
			if err := p.report(ctx, paneID, currentCheckout); err != nil {
				return nil, err
			}
			if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		return &State{Unavailable: "Repository changed during refresh. Refresh again."}, nil
	}
	if !options.Transient {
		if err := writeJSON(file, state); err != nil {
			return nil, err
		}
		if err := p.report(ctx, paneID, state); err != nil {
			return nil, err
		}
	}
	return &state, nil
}

func (p *Plugin) refreshAll(ctx context.Context, cached bool) error {
	var result struct {
		Agents []Pane `json:"agents"`
	}
	if err := p.herdr(ctx, []string{"agent", "list"}, &result); err != nil {
		return err
	}
	jobs := make(chan Pane)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	for range min(3, len(result.Agents)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pane := range jobs {
				state, err := p.refresh(ctx, pane.ID, refreshOptions{Cached: cached, AgentOnly: true})
				message := ""
				if err != nil {
					message = err.Error()
				} else if state != nil && state.Unavailable == "" {
					message = state.problem()
				}
				if message != "" {
					mu.Lock()
					failures = append(failures, fmt.Sprintf("%s: %s", pane.ID, message))
					mu.Unlock()
				}
			}
		}()
	}
	for _, pane := range result.Agents {
		select {
		case jobs <- pane:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "\n"))
	}
	return nil
}
