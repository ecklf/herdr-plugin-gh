package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type commandResult struct {
	Code           int
	Stdout, Stderr string
}
type commandRequest struct {
	Program    string
	Args       []string
	Cwd, Input string
	Env        map[string]string
}
type executor func(context.Context, commandRequest) (commandResult, error)

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if b.Len()+len(data) > 4*1024*1024 {
		return 0, errors.New("command output exceeds 4 MiB")
	}
	return b.Buffer.Write(data)
}

func runCommand(ctx context.Context, request commandRequest) (commandResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, request.Program, request.Args...)
	cmd.Dir = request.Cwd
	if request.Env != nil {
		for key, value := range request.Env {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		sort.Strings(cmd.Env)
	}
	cmd.Stdin = strings.NewReader(request.Input)
	var stdout, stderr limitedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 250 * time.Millisecond
	err := cmd.Run()
	result := commandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if ctx.Err() != nil {
		return result, fmt.Errorf("%s: %w", request.Program, ctx.Err())
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if exit.ExitCode() < 0 {
			return result, fmt.Errorf("%s: %w", request.Program, err)
		}
		result.Code = exit.ExitCode()
	} else if err != nil {
		return result, fmt.Errorf("%s: %w", request.Program, err)
	}
	return result, nil
}

func checked(result commandResult, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if result.Code != 0 {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = fmt.Sprintf("Command failed (%d)", result.Code)
		}
		return "", errors.New(clean(message))
	}
	return strings.TrimSpace(result.Stdout), nil
}

type Plugin struct {
	Env           map[string]string
	Execute       executor
	Now           func() time.Time
	Platform      string
	StateDir      string
	credentialsMu sync.Mutex
	credentials   map[string]*credentialLookup
}

func environment() map[string]string {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	return env
}

func newPlugin(env map[string]string) *Plugin {
	if env == nil {
		env = environment()
	}
	directory := env["HERDR_PLUGIN_STATE_DIR"]
	if directory == "" {
		base := env["XDG_STATE_HOME"]
		if base == "" {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, ".local", "state")
		}
		directory = filepath.Join(base, "herdr-plugin-gh")
	}
	return &Plugin{Env: env, Execute: runCommand, Now: time.Now, Platform: runtime.GOOS, StateDir: directory}
}

func (p *Plugin) command(ctx context.Context, program string, args []string, cwd, input string) (commandResult, error) {
	if err := ctx.Err(); err != nil {
		return commandResult{}, err
	}
	env := make(map[string]string, len(p.Env)+3)
	for key, value := range p.Env {
		env[key] = value
	}
	env["GH_PROMPT_DISABLED"], env["GH_PAGER"], env["GIT_TERMINAL_PROMPT"] = "1", "cat", "0"
	if program == "gh" {
		if err := p.authenticateGitHub(ctx, args, cwd, env); err != nil {
			return commandResult{}, err
		}
	}
	result, err := p.Execute(ctx, commandRequest{Program: program, Args: args, Cwd: cwd, Input: input, Env: env})
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, err
}

func (p *Plugin) herdrBin() string {
	if path := p.Env["HERDR_BIN_PATH"]; path != "" {
		return path
	}
	return "herdr"
}

func (p *Plugin) herdr(ctx context.Context, args []string, target any) error {
	text, err := checked(p.command(ctx, p.herdrBin(), args, "", ""))
	if err != nil || text == "" {
		return err
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &response); err != nil {
		return err
	}
	if response.Error != nil {
		return errors.New(response.Error.Message)
	}
	if target != nil {
		return json.Unmarshal(response.Result, target)
	}
	return nil
}

func (p *Plugin) pane(ctx context.Context, id string) (Pane, error) {
	var result struct {
		Pane Pane `json:"pane"`
	}
	err := p.herdr(ctx, []string{"pane", "get", id}, &result)
	return result.Pane, err
}

func (p *Plugin) target(ctx context.Context) (Pane, error) {
	var invocation struct {
		FocusedPaneID string `json:"focused_pane_id"`
	}
	if text := p.Env["HERDR_PLUGIN_CONTEXT_JSON"]; text != "" {
		if err := json.Unmarshal([]byte(text), &invocation); err != nil {
			return Pane{}, err
		}
	}
	for _, id := range []string{p.Env["HERDR_GH_PANE_ID"], invocation.FocusedPaneID, p.Env["HERDR_PANE_ID"], p.Env["HERDR_ACTIVE_PANE_ID"]} {
		if id != "" {
			return p.pane(ctx, id)
		}
	}
	var result struct {
		Pane Pane `json:"pane"`
	}
	err := p.herdr(ctx, []string{"pane", "current"}, &result)
	return result.Pane, err
}

func (p *Plugin) checkout(ctx context.Context, pane Pane) (State, error) {
	cwd := pane.Cwd
	if pane.ForegroundCwd != nil {
		cwd = *pane.ForegroundCwd
	}
	if cwd == "" {
		return State{Unavailable: "PR: no working directory"}, nil
	}
	root, err := p.command(ctx, "git", []string{"rev-parse", "--show-toplevel"}, cwd, "")
	if err != nil {
		return State{}, err
	}
	if root.Code != 0 && strings.Contains(strings.ToLower(root.Stderr), "not a git repository") {
		return State{Unavailable: "Not a Git repository"}, nil
	}
	rootName, err := checked(root, nil)
	if err != nil {
		return State{}, err
	}
	branch, err := p.command(ctx, "git", []string{"symbolic-ref", "--quiet", "--short", "HEAD"}, cwd, "")
	if err != nil {
		return State{}, err
	}
	if branch.Code == 1 {
		return State{Unavailable: "PR: detached HEAD"}, nil
	}
	branchName, err := checked(branch, nil)
	if err != nil {
		return State{}, err
	}
	head, err := p.command(ctx, "git", []string{"rev-parse", "--verify", "HEAD"}, cwd, "")
	if err != nil {
		return State{}, err
	}
	if head.Code != 0 {
		return State{Branch: branchName, Unavailable: "PR: no commits"}, nil
	}
	config, err := p.command(ctx, "git", []string{"config", "--get-regexp", `^(remote\.|branch\.)`}, cwd, "")
	if err != nil {
		return State{}, err
	}
	if config.Code != 0 && config.Code != 1 {
		_, err = checked(config, nil)
		return State{}, err
	}
	var identity bytes.Buffer
	encoder := json.NewEncoder(&identity)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode([]string{rootName, branchName, strings.TrimSpace(head.Stdout), config.Stdout})
	return State{Cwd: rootName, Branch: branchName, Identity: hash(strings.TrimSuffix(identity.String(), "\n")), githubHost: githubHostHint(config.Stdout, p.Env)}, nil
}

func (p *Plugin) report(ctx context.Context, paneID string, state State) error {
	args := []string{"pane", "report-metadata", paneID, "--source", "plugin:" + pluginID, "--ttl-ms", fmt.Sprint(metadataTTL.Milliseconds())}
	tokens := tokensFor(state)
	for _, key := range tokenOrder {
		if tokens[key] == "" {
			args = append(args, "--clear-token", key)
		} else {
			args = append(args, "--token", key+"="+clean(tokens[key]))
		}
	}
	return p.herdr(ctx, args, nil)
}

// Rename within the destination directory so readers always see complete JSON.
func writeJSON(file string, value any) error {
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(file), ".gh-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(value)
	closeErr := temporary.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temporary.Name(), file)
}

func (p *Plugin) settings(ctx context.Context, markdown *bool) (bool, error) {
	directory := p.Env["HERDR_PLUGIN_CONFIG_DIR"]
	if directory == "" {
		var err error
		directory, err = checked(p.command(ctx, p.herdrBin(), []string{"plugin", "config-dir", pluginID}, "", ""))
		if err != nil {
			return false, err
		}
	}
	file := filepath.Join(directory, "settings.json")
	settings := map[string]json.RawMessage{}
	data, err := os.ReadFile(file)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil || settings == nil {
			return false, fmt.Errorf("Invalid %s: expected a settings object", file)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("Cannot read %s: %w", file, err)
	}
	var enabled bool
	if value, exists := settings["markdown_mode"]; exists {
		if string(value) == "null" || json.Unmarshal(value, &enabled) != nil {
			return false, fmt.Errorf("Invalid %s: markdown_mode must be a boolean", file)
		}
	}
	if markdown != nil {
		enabled = *markdown
		settings["markdown_mode"], _ = json.Marshal(enabled)
		if err := writeJSON(file, settings); err != nil {
			return false, err
		}
	}
	return enabled, nil
}
