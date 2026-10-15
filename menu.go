package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type inputEvent struct {
	Key  string
	X, Y int
}

var mouseInput = regexp.MustCompile(`^\x1b\[<(\d+);(\d+);(\d+)([Mm])`)
var incompleteInput = regexp.MustCompile(`^\x1b(?:\[(?:<\d*(?:;\d*){0,2})?)?$`)
var csiInput = regexp.MustCompile(`^\x1b\[[0-9;?]*[ -/]*[@-~]`)

func parseInput(buffer string) ([]inputEvent, string) {
	var events []inputEvent
	for buffer != "" {
		if mouse := mouseInput.FindStringSubmatch(buffer); mouse != nil {
			if mouse[1] == "0" && mouse[4] == "M" {
				x, _ := strconv.Atoi(mouse[2])
				y, _ := strconv.Atoi(mouse[3])
				events = append(events, inputEvent{X: x, Y: y})
			}
			buffer = buffer[len(mouse[0]):]
		} else if strings.HasPrefix(buffer, "\x1b[A") || strings.HasPrefix(buffer, "\x1b[B") {
			key := "up"
			if buffer[2] == 'B' {
				key = "down"
			}
			events = append(events, inputEvent{Key: key})
			buffer = buffer[3:]
		} else if incompleteInput.MatchString(buffer) {
			break
		} else if strings.HasPrefix(buffer, "\x1b[") {
			if sequence := csiInput.FindString(buffer); sequence != "" {
				buffer = buffer[len(sequence):]
			} else {
				buffer = buffer[1:]
			}
		} else {
			if !utf8.FullRuneInString(buffer) {
				break
			}
			_, size := utf8.DecodeRuneInString(buffer)
			events = append(events, inputEvent{Key: buffer[:size]})
			buffer = buffer[size:]
		}
	}
	return events, buffer
}

type menuChoice struct{ Label, Action string }
type menuOptions struct {
	Selected, Frame, Columns int
	Loading, Markdown        bool
	Message                  string
}
type menuView struct {
	Lines    []string
	Choices  []menuChoice
	Disabled map[string]bool
}

func padRight(text string, width int) string {
	return text + strings.Repeat(" ", max(0, width-utf8.RuneCountInString(text)))
}

func buildMenu(state State, options menuOptions) menuView {
	columns := options.Columns
	if columns == 0 {
		columns = 64
	}
	tokens := tokensFor(state)
	notGit := state.Unavailable == "Not a Git repository" || options.Message == "Not a Git repository"
	unavailable := notGit || state.Unavailable != ""
	noPR := !unavailable && state.PRError == "" && state.PR == nil && state.PRAt != 0
	closedPR := state.PRError == "" && state.PR != nil && (state.PR.State == "CLOSED" || state.PR.State == "MERGED")
	checks, comments, branchStatus := "", "", ""
	if !options.Loading && !unavailable && state.PRError == "" && state.PR != nil {
		switch {
		case state.ChecksError != "":
			checks = "Checks unavailable"
		case state.Checks == nil:
			checks = "Checks not fetched"
		case len(state.Checks) == 0:
			checks = "No checks"
		default:
			var counts []string
			for _, label := range checkOrder {
				if token := tokens["gh_"+label]; token != "" {
					counts = append(counts, token)
				}
			}
			checks = strings.Join(counts, " ")
		}
		if state.PR.State == "OPEN" {
			switch {
			case state.BehindError != "":
				branchStatus = "unavailable"
			case state.Behind != nil && *state.Behind >= 0:
				branchStatus = "\uf111"
			default:
				branchStatus = "not checked"
			}
		}
	}
	if !options.Loading && !notGit {
		comments = tokens["gh_review"]
	}
	format := "\U000f06d6 standard"
	if options.Markdown {
		format = "\U000f06d6 markdown"
	}
	align := func(label, detail string) string {
		if detail == "" {
			return label
		}
		return padRight(label+"  ", columns-7-utf8.RuneCountInString(detail)) + detail
	}
	openLabel := "Open PR"
	if noPR {
		openLabel = "Create PR"
	}
	view := menuView{
		Choices:  []menuChoice{{openLabel, "open"}, {align("Copy PR for Slack", format), "copy-slack"}, {align("View Checks", checks), "open-checks"}, {align("View Comments", comments), "open-comments"}, {align("Update branch", branchStatus), "update-branch"}},
		Disabled: make(map[string]bool),
	}
	for _, choice := range view.Choices {
		if (options.Loading && (choice.Action != "open" || state.Branch == "")) || unavailable || (noPR && choice.Action != "open") || (closedPR && choice.Action == "update-branch") {
			view.Disabled[choice.Action] = true
		}
	}
	if options.Loading || unavailable || noPR || closedPR {
		view.Disabled["rerun-failed"] = true
	}
	if view.Disabled["copy-slack"] {
		view.Disabled["toggle-markdown"] = true
	}
	header := tokens["gh_pr"]
	if branch := tokens["gh_branch"]; branch != "" {
		if header != "" {
			header += "  "
		}
		header += branch
	}
	if notGit {
		header = "Not a Git repository"
	}
	if options.Loading {
		spinner := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
		header = string(spinner[options.Frame%len(spinner)]) + " Loading"
	}
	view.Lines = []string{header, ""}
	for index, choice := range view.Choices {
		prefix, number := " ", strconv.Itoa(index+1)
		if options.Selected == index {
			prefix = ">"
		}
		if view.Disabled[choice.Action] {
			number = "-"
		}
		view.Lines = append(view.Lines, prefix+" ["+number+"] "+choice.Label)
	}
	message := options.Message
	if notGit && message == "Not a Git repository" {
		message = ""
	}
	if message == "" {
		message = "1-5 | r Refresh state"
		if !view.Disabled["rerun-failed"] {
			message += " | f Rerun failed checks"
		}
		if options.Selected == 1 && !view.Disabled["toggle-markdown"] {
			message += " | Space MD"
		}
	}
	view.Lines = append(view.Lines, "", message)
	return view
}

var checkRow = regexp.MustCompile(`^[ >] \[3\] View Checks  `)
var branchRow = regexp.MustCompile(`^[ >] \[5\] Update branch  `)
var coloredCount = regexp.MustCompile(`\d+[\x{f52f}\x{f43a}\x{f42e}]`)
var loadingLine = regexp.MustCompile(`^[\x{2800}-\x{28ff}] Loading$`)

func renderMenuLine(line string, columns int, behind *int64, disabled bool) string {
	plain := clean(line)
	runes := []rune(plain)
	text := string(runes[:min(len(runes), max(0, columns-1))])
	if disabled {
		return "\x1b[38;5;240m" + text + "\x1b[39m"
	}
	if loadingLine.MatchString(plain) {
		return "\x1b[2m" + text + "\x1b[22m"
	}
	if checkRow.MatchString(plain) {
		text = coloredCount.ReplaceAllStringFunc(text, func(pair string) string {
			color := 32
			if strings.HasSuffix(pair, icons["failed"]) {
				color = 91
			} else if strings.HasSuffix(pair, icons["pending"]) {
				color = 33
			}
			return fmt.Sprintf("\x1b[%dm%s\x1b[39m", color, pair)
		})
	}
	if branchRow.MatchString(plain) && behind != nil && *behind >= 0 {
		color := 33
		if *behind == 0 {
			color = 32
		}
		text = strings.ReplaceAll(text, "\uf111", fmt.Sprintf("\x1b[%dm\uf111\x1b[39m", color))
	}
	return strings.ReplaceAll(text, icons["branch"], "\x1b[38;2;192;192;192m"+icons["branch"]+"\x1b[39m")
}

type menuBackend interface {
	settings(context.Context, *bool) (bool, error)
	refresh(context.Context, string, refreshOptions) (*State, error)
	refreshAll(context.Context, bool) error
	act(context.Context, string, string, confirmAction) (*State, error)
}

type terminalInput struct {
	Data string
	Err  error
}
type uiEvent struct {
	Kind, Action, Notice   string
	Version, StatusVersion int
	State                  *State
	Err                    error
	Markdown               bool
	Details                confirmationDetails
	Answer                 chan bool
}

// All UI state belongs to this event loop; workers communicate only through events.
func runMenu(ctx context.Context, backend menuBackend, paneID string, input <-chan terminalInput, output io.Writer, size func() (int, int), resized <-chan os.Signal) error {
	markdown, err := backend.settings(ctx, nil)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	events := make(chan uiEvent, 32)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	send := func(event uiEvent) bool {
		select {
		case events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}
	launch := func(work func()) { workers.Add(1); go func() { defer workers.Done(); work() }() }
	var state State
	selected, frame, version, statusVersion := 0, 0, 0, 0
	loading := false
	message, mutation, buffer := "", "", ""
	active := make(map[string]int)
	var confirmation *uiEvent
	var view menuView
	var messageTimer, escapeTimer *time.Timer
	var messageTimeout, escapeTimeout <-chan time.Time
	stopMessage := func() {
		if messageTimer != nil {
			messageTimer.Stop()
		}
		messageTimeout = nil
	}
	stopEscape := func() {
		if escapeTimer != nil {
			escapeTimer.Stop()
		}
		escapeTimeout = nil
	}
	defer func() { stopMessage(); stopEscape() }()
	expireMessage := func() { stopMessage(); messageTimer = time.NewTimer(2 * time.Second); messageTimeout = messageTimer.C }
	draw := func() error {
		columns, rows := size()
		view = buildMenu(state, menuOptions{Selected: selected, Frame: frame, Columns: columns, Loading: loading, Markdown: markdown, Message: message})
		lines := make([]string, min(len(view.Lines), max(0, rows)))
		for index := range lines {
			disabled := index >= 2 && index < 7 && view.Disabled[view.Choices[index-2].Action]
			lines[index] = renderMenuLine(view.Lines[index], columns, state.Behind, disabled)
		}
		_, err := io.WriteString(output, "\x1b[H\x1b[2J"+strings.Join(lines, "\r\n"))
		return err
	}
	startLoad := func(all, cached bool, owner int, action, notice string) {
		statusVersion++
		loadVersion := statusVersion
		loading, frame = true, 0
		launch(func() {
			var allErr error
			if all {
				allErr = backend.refreshAll(ctx, false)
			}
			value, loadErr := backend.refresh(ctx, paneID, refreshOptions{Cached: cached, OnCheckout: func(checkout State) {
				send(uiEvent{Kind: "checkout", State: &checkout, StatusVersion: loadVersion})
			}})
			if allErr != nil {
				loadErr = allErr
			}
			if loadErr == nil && value != nil && value.problem() != "" {
				loadErr = errors.New(value.problem())
			}
			send(uiEvent{Kind: "status", State: value, Err: loadErr, Version: owner, StatusVersion: loadVersion, Action: action, Notice: notice})
		})
	}
	perform := func(action string) {
		if view.Disabled[action] || active[action] != 0 || (action == "refresh" && loading) {
			return
		}
		version++
		owner := version
		active[action] = owner
		stopMessage()
		progress := map[string]string{"copy-slack": "Copying PR...", "toggle-markdown": "Saving preference...", "refresh": "", "open-comments": "Finding unresolved comments...", "rerun-failed": "Finding failed Actions runs...", "update-branch": "Checking branch status..."}
		var exists bool
		message, exists = progress[action]
		if !exists {
			message = "Opening in your browser..."
		}
		if action == "refresh" {
			startLoad(true, true, owner, action, "")
			return
		}
		nextMarkdown := !markdown
		launch(func() {
			if action == "toggle-markdown" {
				value, err := backend.settings(ctx, &nextMarkdown)
				send(uiEvent{Kind: "settings", Action: action, Markdown: value, Err: err, Version: owner})
				return
			}
			value, err := backend.act(ctx, paneID, action, func(ctx context.Context, details confirmationDetails) (bool, error) {
				answer := make(chan bool, 1)
				if !send(uiEvent{Kind: "confirm", Action: action, Details: details, Answer: answer, Version: owner}) {
					return false, ctx.Err()
				}
				select {
				case approved := <-answer:
					return approved, nil
				case <-ctx.Done():
					return false, ctx.Err()
				}
			})
			send(uiEvent{Kind: "action", Action: action, State: value, Err: err, Version: owner})
		})
	}
	answerConfirmation := func(approved bool) {
		if confirmation == nil {
			return
		}
		if approved {
			mutation = confirmation.Action
		}
		if confirmation.Action == "update-branch" {
			if approved {
				message = "Updating PR branch..."
			} else {
				message = "Branch update canceled."
			}
		} else {
			if approved {
				message = "Requesting failed-job reruns..."
			} else {
				message = "Rerun canceled."
			}
		}
		confirmation.Answer <- approved
		confirmation = nil
	}
	startLoad(false, true, version, "", "")
	if err := draw(); err != nil {
		return err
	}
	spinner := time.NewTicker(80 * time.Millisecond)
	defer spinner.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-resized:
		case <-spinner.C:
			if !loading {
				continue
			}
			frame++
		case <-messageTimeout:
			message, messageTimeout = "", nil
		case <-escapeTimeout:
			buffer, escapeTimeout = "", nil
			if confirmation != nil {
				answerConfirmation(false)
			} else {
				return nil
			}
		case event := <-events:
			switch event.Kind {
			case "checkout":
				if event.StatusVersion != statusVersion {
					continue
				}
				checkout := *event.State
				if checkout.Unavailable != "" || checkout.Identity != state.Identity {
					state = checkout
				} else {
					state.Branch = checkout.Branch
				}
			case "status":
				if event.StatusVersion == statusVersion {
					loading = false
					if event.State != nil {
						state = *event.State
					}
				}
				if event.Version == version && event.StatusVersion == statusVersion {
					message = ""
					if event.Action != "" {
						message = "State refreshed."
					}
					if event.Err != nil {
						message = event.Err.Error()
					}
					if event.Notice != "" {
						if event.Err != nil {
							message = event.Notice + " Refresh failed: " + event.Err.Error()
						} else {
							message = event.Notice + " State refreshed."
						}
					}
					if event.Action != "" && event.Err == nil {
						expireMessage()
					}
				}
				if event.Action != "" && active[event.Action] == event.Version {
					delete(active, event.Action)
					if mutation == event.Action {
						mutation = ""
					}
				}
			case "confirm":
				if event.Version != version {
					event.Answer <- false
					continue
				}
				confirmation = &event
				if event.Action == "update-branch" {
					message = fmt.Sprintf("Update PR #%d from its base branch? y/N", event.Details.Number)
				} else {
					suffix := "s"
					if event.Details.Runs == 1 {
						suffix = ""
					}
					message = fmt.Sprintf("Rerun failed jobs for #%d (%d run%s)? y/N", event.Details.Number, event.Details.Runs, suffix)
				}
			case "settings", "action":
				if event.Kind == "settings" && event.Err == nil {
					markdown = event.Markdown
				}
				if event.Version != version {
					if active[event.Action] == event.Version {
						delete(active, event.Action)
					}
					if event.Kind == "settings" && event.Err == nil {
						if err := draw(); err != nil {
							return err
						}
					}
					continue
				}
				if event.Err != nil {
					message = event.Err.Error()
				} else {
					switch event.Action {
					case "toggle-markdown":
						message = "Markdown mode disabled."
						if markdown {
							message = "Markdown mode enabled."
						}
					case "copy-slack":
						message = "Copied PR for Slack."
					case "rerun-failed":
						message = "Rerun canceled."
						if event.State != nil && event.State.RerunCount > 0 {
							suffix := "s"
							if event.State.RerunCount == 1 {
								suffix = ""
							}
							message = fmt.Sprintf("Rerun requested for %d run%s.", event.State.RerunCount, suffix)
						}
					case "update-branch":
						message = "Branch update canceled."
						if event.State != nil {
							if event.State.BranchUpdate == "requested" {
								message = "Branch update requested."
							}
							if event.State.BranchUpdate == "up-to-date" {
								message = "Branch is up to date."
							}
						}
					default:
						message = "Opened in your browser."
					}
				}
				if event.Err == nil && event.State != nil && (event.State.RerunCount > 0 || event.State.BranchUpdate == "requested" || event.State.BranchUpdate == "up-to-date") {
					notice := message
					message += " Refreshing state..."
					startLoad(false, false, event.Version, event.Action, notice)
				} else {
					delete(active, event.Action)
					if mutation == event.Action {
						mutation = ""
					}
					if event.Err == nil && message != "" {
						expireMessage()
					}
				}
			}
		case read, ok := <-input:
			if !ok || read.Err != nil {
				return nil
			}
			stopEscape()
			buffer += read.Data
			parsed, rest := parseInput(buffer)
			buffer = rest
			for _, event := range parsed {
				key := event.Key
				if key == "q" || key == "\x03" {
					return nil
				}
				if confirmation != nil {
					if key == "y" || key == "Y" || key == "n" || key == "N" || key == "\r" || key == "\n" || key == "\x1b" {
						buffer = ""
						answerConfirmation(key == "y" || key == "Y")
						break
					}
					continue
				}
				if key == "\x1b" {
					return nil
				}
				if mutation != "" {
					continue
				}
				switch key {
				case "up", "k":
					selected = (selected + 4) % 5
				case "down", "j", "\t":
					selected = (selected + 1) % 5
				case " ":
					if selected == 1 {
						perform("toggle-markdown")
					}
				case "r":
					perform("refresh")
				case "c":
					perform("open-comments")
				case "f":
					perform("rerun-failed")
				case "\r", "\n":
					perform(view.Choices[selected].Action)
				case "1", "2", "3", "4", "5":
					selected = int(key[0] - '1')
					perform(view.Choices[selected].Action)
				case "":
					index := event.Y - 3
					if index >= 0 && index < 5 && event.X > 0 && event.X <= utf8.RuneCountInString(view.Choices[index].Label)+6 {
						selected = index
						perform(view.Choices[selected].Action)
					}
				}
				// Availability changes immediately when a refresh starts within this batch.
				if err := draw(); err != nil {
					return err
				}
			}
			if buffer == "\x1b" {
				escapeTimer = time.NewTimer(40 * time.Millisecond)
				escapeTimeout = escapeTimer.C
			}
		}
		if err := draw(); err != nil {
			return err
		}
	}
}

func menu(ctx context.Context, backend menuBackend, paneID string, input, output *os.File) error {
	if !term.IsTerminal(int(input.Fd())) || !term.IsTerminal(int(output.Fd())) {
		return errors.New("The PR menu needs a terminal. Open it with the gh.menu action.")
	}
	raw, err := term.MakeRaw(int(input.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(input.Fd()), raw)
	if _, err := io.WriteString(output, "\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h"); err != nil {
		return err
	}
	defer io.WriteString(output, "\x1b[?1000l\x1b[?1006l\x1b[?25h\x1b[?1049l")
	ctx, cancel := context.WithCancel(ctx)
	inputEvents := make(chan terminalInput, 8)
	var reader sync.WaitGroup
	reader.Add(1)
	go func() {
		defer reader.Done()
		defer close(inputEvents)
		fds := []unix.PollFd{{Fd: int32(input.Fd()), Events: unix.POLLIN}}
		data := make([]byte, 4096)
		for ctx.Err() == nil {
			count, err := unix.Poll(fds, 100)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err == nil && count == 0 {
				continue
			}
			var n int
			if err == nil {
				n, err = unix.Read(int(input.Fd()), data)
				if n == 0 && err == nil {
					err = io.EOF
				}
			}
			select {
			case inputEvents <- terminalInput{Data: string(data[:max(0, n)]), Err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() { cancel(); reader.Wait() }()
	resized := make(chan os.Signal, 1)
	signal.Notify(resized, syscall.SIGWINCH)
	defer signal.Stop(resized)
	return runMenu(ctx, backend, paneID, inputEvents, output, func() (int, int) {
		columns, rows, err := term.GetSize(int(output.Fd()))
		if err != nil {
			return 64, 9
		}
		return columns, rows
	}, resized)
}
