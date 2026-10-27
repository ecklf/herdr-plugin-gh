package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestInputParser(t *testing.T) {
	events, rest := parseInput("\x1b[<0;12;")
	if len(events) != 0 || rest != "\x1b[<0;12;" {
		t.Fatal(events, rest)
	}
	events, rest = parseInput(rest + "6M2\x1b[B\r")
	want := []inputEvent{{X: 12, Y: 6}, {Key: "2"}, {Key: "down"}, {Key: "\r"}}
	if !reflect.DeepEqual(events, want) || rest != "" {
		t.Fatal(events, rest)
	}
	events, rest = parseInput("\x1b[<0;12;6m\x1b[Aq\x1b")
	if !reflect.DeepEqual(events, []inputEvent{{Key: "up"}, {Key: "q"}}) || rest != "\x1b" {
		t.Fatal(events, rest)
	}
	for _, sequence := range []string{"\x1b[", "\x1b[<", "\x1b[<0;4;"} {
		_, rest := parseInput(sequence)
		if rest != sequence {
			t.Fatal(rest)
		}
	}
}

func TestMenuAvailability(t *testing.T) {
	all := []string{"open", "copy-slack", "open-checks", "open-comments", "update-branch", "rerun-failed", "toggle-markdown"}
	for _, tc := range []struct {
		name     string
		state    State
		loading  bool
		disabled []string
		label    string
	}{
		{"unknown", State{}, false, nil, "Open PR"},
		{"initial", State{}, true, all, "Open PR"},
		{"known branch loading", State{Branch: "feature"}, true, all[1:], "Open PR"},
		{"no PR", State{PRAt: 1, Branch: "new"}, false, all[1:], "Create PR"},
		{"no PR reload", State{PRAt: 1, Branch: "new"}, true, all[1:], "Create PR"},
		{"non-Git", State{Unavailable: "Not a Git repository"}, false, all, "Open PR"},
		{"unborn", State{Unavailable: "PR: no commits", Branch: "new"}, true, all, "Open PR"},
		{"lookup error", State{PRAt: 1, PRError: "offline"}, false, nil, "Open PR"},
		{"open", State{PR: testPR()}, false, nil, "Open PR"},
		{"checks error", State{PR: testPR(), ChecksError: "offline"}, false, nil, "Open PR"},
		{"closed", State{PR: &PullRequest{State: "CLOSED"}}, false, []string{"update-branch", "rerun-failed"}, "Open PR"},
		{"merged", State{PR: &PullRequest{State: "MERGED"}}, false, []string{"update-branch", "rerun-failed"}, "Open PR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := buildMenu(tc.state, menuOptions{Loading: tc.loading, Columns: 64})
			if len(view.Lines) != 9 || len(view.Choices) != 5 || view.Lines[1] != "" || view.Lines[7] != "" || view.Choices[0].Label != tc.label {
				t.Fatal(view)
			}
			for _, action := range all {
				want := false
				for _, disabled := range tc.disabled {
					if disabled == action {
						want = true
					}
				}
				if view.Disabled[action] != want {
					t.Errorf("%s disabled=%v, want %v", action, view.Disabled[action], want)
				}
			}
		})
	}
}

func TestMenuAlignmentAndColors(t *testing.T) {
	for _, columns := range []int{12, 24, 40, 64, 120} {
		state := State{PR: testPR(), Branch: "feature/checks", Unresolved: ptr(int64(3)), Behind: ptr(int64(2)), Checks: []Check{{Bucket: "fail"}, {Bucket: "pending"}, {Bucket: "pass"}, {Bucket: "cancel"}, {Bucket: "skipping"}, {Bucket: "future"}}}
		view := buildMenu(state, menuOptions{Columns: columns, Selected: 1})
		if len(view.Lines) != 9 {
			t.Fatal(view)
		}
		for _, line := range view.Lines {
			rendered := renderMenuLine(line, columns, state.Behind, false)
			runes := []rune(clean(line))
			expected := string(runes[:min(len(runes), columns-1)])
			if clean(rendered) != expected {
				t.Fatal(rendered, expected)
			}
		}
		if columns >= 40 {
			for _, row := range []int{3, 4, 5, 6} {
				if utf8.RuneCountInString(view.Lines[row]) != columns-1 {
					t.Fatal(columns, row, view.Lines[row])
				}
			}
			if !strings.HasSuffix(view.Lines[3], "\U000f06d6 standard") || !strings.HasSuffix(view.Lines[5], "\uf41f 3") {
				t.Fatal(view)
			}
			if !strings.Contains(renderMenuLine(view.Lines[4], columns, nil, false), "\x1b[91m1\uf52f\x1b[39m") {
				t.Fatal(view.Lines[4])
			}
			if !strings.HasSuffix(renderMenuLine(view.Lines[6], columns, state.Behind, false), "\x1b[33m\uf111\x1b[39m") {
				t.Fatal(view.Lines[6])
			}
		}
	}
	line := "\x1b[31m> [3] View Checks  1\uf52f\x1b[0m"
	if got := renderMenuLine(line, 64, nil, true); got != "\x1b[38;5;240m> [3] View Checks  1\uf52f\x1b[39m" {
		t.Fatal(got)
	}
	if got := renderMenuLine("A\U000f06d6B", 3, nil, false); got != "A\U000f06d6" {
		t.Fatal(got)
	}
	if got := clean("\x1b]8;;https://example.com\x07linked\x1b]8;;\x07\ntext"); got != "linked text" {
		t.Fatal(got)
	}
}

func TestMenuStatusLabels(t *testing.T) {
	for _, tc := range []struct {
		state          State
		checks, branch string
	}{
		{State{PR: testPR()}, "Checks not fetched", "not checked"},
		{State{PR: testPR(), Checks: []Check{}}, "No checks", "not checked"},
		{State{PR: testPR(), ChecksError: "offline", BehindError: "offline", Behind: ptr(int64(0))}, "Checks unavailable", "unavailable"},
	} {
		view := buildMenu(tc.state, menuOptions{})
		if !strings.HasSuffix(view.Lines[4], tc.checks) || !strings.HasSuffix(view.Lines[6], tc.branch) {
			t.Fatal(view)
		}
		loading := buildMenu(tc.state, menuOptions{Loading: true})
		if strings.Contains(strings.Join(loading.Lines, "\n"), tc.checks) || strings.Contains(loading.Lines[6], "not checked") {
			t.Fatal(loading)
		}
	}
}

type loadReply struct {
	state *State
	err   error
}
type loadRequest struct {
	options refreshOptions
	reply   chan loadReply
}
type actionRequest struct {
	action  string
	ctx     context.Context
	confirm confirmAction
	reply   chan loadReply
}
type fakeMenuBackend struct {
	loads          chan loadRequest
	actions        chan actionRequest
	refreshes      atomic.Int64
	settingsWrites atomic.Int64
	markdown       atomic.Bool
	settingsGate   <-chan struct{}
}

func (f *fakeMenuBackend) settings(ctx context.Context, value *bool) (bool, error) {
	if value != nil {
		if f.settingsGate != nil {
			select {
			case <-f.settingsGate:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		f.markdown.Store(*value)
		f.settingsWrites.Add(1)
	}
	return f.markdown.Load(), nil
}
func (f *fakeMenuBackend) refresh(ctx context.Context, _ string, options refreshOptions) (*State, error) {
	r := loadRequest{options: options, reply: make(chan loadReply, 1)}
	select {
	case f.loads <- r:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case result := <-r.reply:
		return result.state, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *fakeMenuBackend) refreshAll(context.Context, bool) error { f.refreshes.Add(1); return nil }
func (f *fakeMenuBackend) act(ctx context.Context, _ string, action string, confirm confirmAction) (*State, error) {
	r := actionRequest{action: action, ctx: ctx, confirm: confirm, reply: make(chan loadReply, 1)}
	select {
	case f.actions <- r:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case result := <-r.reply:
		return result.state, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type screenWriter struct {
	mu   sync.Mutex
	text string
}

func (s *screenWriter) Write(data []byte) (int, error) {
	s.mu.Lock()
	s.text = string(data)
	s.mu.Unlock()
	return len(data), nil
}
func (s *screenWriter) read() string { s.mu.Lock(); defer s.mu.Unlock(); return s.text }

type popupHarness struct {
	f       *fakeMenuBackend
	input   chan terminalInput
	screen  *screenWriter
	done    chan error
	cancel  context.CancelFunc
	columns atomic.Int64
	resized chan os.Signal
}

func popup(t *testing.T) *popupHarness {
	t.Helper()
	h := &popupHarness{f: &fakeMenuBackend{loads: make(chan loadRequest, 16), actions: make(chan actionRequest, 16)}, input: make(chan terminalInput, 16), screen: &screenWriter{}, done: make(chan error, 1), resized: make(chan os.Signal, 1)}
	h.columns.Store(64)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		h.done <- runMenu(ctx, h.f, "w1:p1", h.input, h.screen, func() (int, int) { return int(h.columns.Load()), 9 }, h.resized)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.done:
			requireNoError(t, err)
		case <-time.After(3 * time.Second):
			t.Error("menu did not clean up workers")
		}
	})
	return h
}
func (h *popupHarness) send(keys string) { h.input <- terminalInput{Data: keys} }
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for popup")
		}
		time.Sleep(time.Millisecond)
	}
}
func (h *popupHarness) contains(t *testing.T, text string) {
	t.Helper()
	waitFor(t, func() bool { return strings.Contains(clean(h.screen.read()), text) })
}
func (h *popupHarness) nextLoad(t *testing.T) loadRequest {
	t.Helper()
	select {
	case r := <-h.f.loads:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("no status request")
		return loadRequest{}
	}
}
func (h *popupHarness) nextAction(t *testing.T, want string) actionRequest {
	t.Helper()
	select {
	case r := <-h.f.actions:
		if r.action != want {
			t.Fatalf("action %s, want %s", r.action, want)
		}
		return r
	case <-time.After(3 * time.Second):
		t.Fatalf("no %s action", want)
		return actionRequest{}
	}
}
func (h *popupHarness) loaded(t *testing.T, state *State) {
	t.Helper()
	r := h.nextLoad(t)
	h.contains(t, "Loading")
	r.reply <- loadReply{state: state}
	waitFor(t, func() bool { return !strings.Contains(h.screen.read(), "Loading") })
}
func loadedState() *State {
	return &State{PR: testPR(), PRAt: 1, Identity: "checkout", Branch: "feature/checks", Behind: ptr(int64(2)), Checks: []Check{{Bucket: "pass"}}, Unresolved: ptr(int64(3))}
}
func (h *popupHarness) ask(t *testing.T, r actionRequest) chan bool {
	t.Helper()
	answer := make(chan bool, 1)
	go func() {
		approved, _ := r.confirm(r.ctx, confirmationDetails{Number: 42, Runs: 2, Base: "main", Behind: 2})
		answer <- approved
	}()
	h.contains(t, "? y/N")
	return answer
}

func TestPopupEarlyCheckoutAndCancellation(t *testing.T) {
	h := popup(t)
	load := h.nextLoad(t)
	h.contains(t, "> [-] Open PR")
	h.send("2345cf2 ")
	load.options.OnCheckout(State{Identity: "checkout", Branch: "feature/early", Cwd: "/repo"})
	h.contains(t, "[1] Open PR")
	h.send("1")
	action := h.nextAction(t, "open")
	action.reply <- loadReply{state: &State{}}
	h.contains(t, "Opened in your browser.")
	if h.f.settingsWrites.Load() != 0 {
		t.Fatal("disabled copy preference changed")
	}
	load.reply <- loadReply{state: loadedState()}
	h.contains(t, "#42")
	h.send("q")
}

func TestPopupDisabledTargetsAndRetryableErrors(t *testing.T) {
	for _, kind := range []string{"non-Git", "no PR", "closed", "error"} {
		t.Run(kind, func(t *testing.T) {
			h := popup(t)
			state := loadedState()
			switch kind {
			case "non-Git":
				state = &State{Unavailable: "Not a Git repository"}
			case "no PR":
				state = &State{PRAt: 1, Branch: "new"}
			case "closed":
				state.PR.State = "CLOSED"
			case "error":
				state = &State{PRError: "Lookup offline"}
			}
			h.loaded(t, state)
			if kind == "non-Git" || kind == "no PR" {
				h.send("2345\rcf2 ")
				h.contains(t, "> [-] Copy PR for Slack")
				select {
				case r := <-h.f.actions:
					t.Fatal("disabled action invoked", r.action)
				default:
				}
				if h.f.settingsWrites.Load() != 0 {
					t.Fatal("disabled toggle saved")
				}
			} else if kind == "closed" {
				h.send("5\rf")
				h.contains(t, "> [-] Update branch")
				select {
				case r := <-h.f.actions:
					t.Fatal("closed PR mutation invoked", r.action)
				default:
				}
			} else {
				h.send("1")
				r := h.nextAction(t, "open")
				r.reply <- loadReply{err: errors.New("Still offline")}
				h.contains(t, "Still offline")
			}
			h.send("r")
			load := h.nextLoad(t)
			if !load.options.Cached || h.f.refreshes.Load() != 1 {
				t.Fatal("refresh behavior changed")
			}
			load.reply <- loadReply{state: loadedState()}
			h.contains(t, "State refreshed.")
		})
	}
}

func TestPopupSelectionPreferencesMouseAndResize(t *testing.T) {
	h := popup(t)
	h.loaded(t, loadedState())
	h.send("j ")
	h.contains(t, "Markdown mode enabled.")
	if h.f.settingsWrites.Load() != 1 || !h.f.markdown.Load() {
		t.Fatal("preference not saved")
	}
	h.send("\r")
	r := h.nextAction(t, "copy-slack")
	r.reply <- loadReply{state: &State{}}
	h.contains(t, "Copied PR for Slack.")
	h.columns.Store(120)
	h.resized <- os.Interrupt
	waitFor(t, func() bool {
		lines := strings.Split(h.screen.read(), "\r\n")
		return len(lines) == 9 && utf8.RuneCountInString(clean(lines[3])) == 119
	})
	h.send("\x1b[<0;119;6M")
	r = h.nextAction(t, "open-comments")
	r.reply <- loadReply{state: &State{}}
	h.contains(t, "> [4] View Comments")
}

func TestPopupMutationConfirmationAndTargetedRefresh(t *testing.T) {
	for _, action := range []string{"rerun-failed", "update-branch"} {
		t.Run(action, func(t *testing.T) {
			h := popup(t)
			h.loaded(t, loadedState())
			key := "f"
			if action == "update-branch" {
				key = "5"
			}
			h.send("j" + key + key)
			r := h.nextAction(t, action)
			answer := h.ask(t, r)
			prompt := h.screen.read()
			h.send("jkrcf12345\x1b[<0;4;3M")
			time.Sleep(10 * time.Millisecond)
			if h.screen.read() != prompt {
				t.Fatal("confirmation accepted unrelated input")
			}
			h.send("y\r5rcf\x1b")
			if !<-answer {
				t.Fatal("approval lost")
			}
			result := &State{RerunCount: 2}
			notice := "Rerun requested for 2 runs."
			if action == "update-branch" {
				result = &State{BranchUpdate: "requested"}
				notice = "Branch update requested."
			}
			r.reply <- loadReply{state: result}
			load := h.nextLoad(t)
			h.contains(t, notice+" Refreshing state...")
			if load.options.Cached || h.f.refreshes.Load() != 0 {
				t.Fatal("mutation used global/cached refresh")
			}
			h.send("\rjk12345rcf ")
			time.Sleep(10 * time.Millisecond)
			select {
			case extra := <-h.f.actions:
				t.Fatal("mutation lock bypassed", extra.action)
			default:
			}
			fresh := loadedState()
			fresh.Behind = ptr(int64(0))
			load.reply <- loadReply{state: fresh}
			h.contains(t, notice+" State refreshed.")
			h.send("c")
			read := h.nextAction(t, "open-comments")
			read.reply <- loadReply{state: &State{}}
		})
	}
}

func TestPopupConfirmationDefaultsToCancel(t *testing.T) {
	for _, key := range []string{"n", "N", "\r", "\n", "\x1b"} {
		t.Run(key, func(t *testing.T) {
			h := popup(t)
			h.loaded(t, loadedState())
			h.send("jf")
			r := h.nextAction(t, "rerun-failed")
			answer := h.ask(t, r)
			h.send(key)
			if <-answer {
				t.Fatal("default approved")
			}
			r.reply <- loadReply{state: &State{}}
			h.contains(t, "Rerun canceled.")
			h.send("\r")
			copy := h.nextAction(t, "copy-slack")
			copy.reply <- loadReply{state: &State{}}
		})
	}
}

func TestPopupSupersededLookupsCannotPromptOrUnlockMutation(t *testing.T) {
	h := popup(t)
	h.loaded(t, loadedState())
	h.send("f")
	old := h.nextAction(t, "rerun-failed")
	h.send("5")
	current := h.nextAction(t, "update-branch")
	answer := h.ask(t, current)
	h.send("y")
	if !<-answer {
		t.Fatal("approval lost")
	}
	approved, err := old.confirm(old.ctx, confirmationDetails{Number: 42, Runs: 2})
	requireNoError(t, err)
	if approved {
		t.Fatal("stale lookup approved")
	}
	old.reply <- loadReply{state: &State{}}
	h.send("rcf\rj")
	time.Sleep(10 * time.Millisecond)
	select {
	case extra := <-h.f.actions:
		t.Fatal("old completion unlocked newer mutation", extra.action)
	default:
	}
	current.reply <- loadReply{state: &State{BranchUpdate: "requested"}}
	load := h.nextLoad(t)
	load.reply <- loadReply{state: loadedState()}
	h.contains(t, "Branch update requested. State refreshed.")
}

func TestPopupPostMutationFailureKeepsAcknowledgement(t *testing.T) {
	h := popup(t)
	h.loaded(t, loadedState())
	h.send("f")
	r := h.nextAction(t, "rerun-failed")
	answer := h.ask(t, r)
	h.send("y")
	<-answer
	r.reply <- loadReply{state: &State{RerunCount: 2}}
	load := h.nextLoad(t)
	state := loadedState()
	state.ChecksError = "offline"
	load.reply <- loadReply{state: state}
	h.contains(t, "Rerun requested for 2 runs. Refresh failed: offline")
	h.send("r")
	load = h.nextLoad(t)
	load.reply <- loadReply{state: loadedState()}
	h.contains(t, "State refreshed.")
}

func TestPopupTimersAndActionDeduplication(t *testing.T) {
	h := popup(t)
	h.loaded(t, loadedState())
	h.send("222")
	copy := h.nextAction(t, "copy-slack")
	h.send("1")
	open := h.nextAction(t, "open")
	open.reply <- loadReply{state: &State{}}
	h.contains(t, "Opened in your browser.")
	copy.reply <- loadReply{state: &State{}}
	h.contains(t, "Opened in your browser.")
	select {
	case extra := <-h.f.actions:
		t.Fatal("duplicate action", extra.action)
	default:
	}
	waitFor(t, func() bool {
		return strings.HasSuffix(clean(h.screen.read()), "1-5 | r Refresh state | f Rerun failed checks")
	})
}

func TestPopupLatePreferenceSaveUpdatesIndicatorOnly(t *testing.T) {
	h := popup(t)
	h.loaded(t, loadedState())
	gate := make(chan struct{})
	h.f.settingsGate = gate
	h.send("j ")
	h.contains(t, "Saving preference...")
	h.send("1")
	open := h.nextAction(t, "open")
	open.reply <- loadReply{state: &State{}}
	h.contains(t, "Opened in your browser.")
	close(gate)
	h.contains(t, "\U000f06d6 markdown")
	h.contains(t, "Opened in your browser.")
}

func TestPopupCloseCancelsPendingWork(t *testing.T) {
	for _, stage := range []string{"loading", "confirmation", "mutation"} {
		t.Run(stage, func(t *testing.T) {
			h := popup(t)
			if stage == "loading" {
				h.nextLoad(t)
			} else {
				h.loaded(t, loadedState())
				h.send("f")
				r := h.nextAction(t, "rerun-failed")
				answer := h.ask(t, r)
				if stage == "mutation" {
					h.send("y")
					<-answer
				}
				h.send("q")
				waitFor(t, func() bool { return r.ctx.Err() != nil })
				if stage == "confirmation" && <-answer {
					t.Fatal("closing approved mutation")
				}
				return
			}
			h.send("q")
		})
	}
}
