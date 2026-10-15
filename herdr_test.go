package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type synchronizedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(data)
}
func (b *synchronizedBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func TestHerdrIntegration(t *testing.T) {
	if os.Getenv("HERDR_GH_INTEGRATION") != "1" {
		t.Skip("set HERDR_GH_INTEGRATION=1 to run an isolated Herdr server")
	}
	herdr, err := exec.LookPath("herdr")
	requireNoError(t, err)
	root, err := os.Getwd()
	requireNoError(t, err)
	directory := t.TempDir()
	pluginDir := filepath.Join(directory, "plugin with spaces")
	requireNoError(t, os.MkdirAll(filepath.Join(pluginDir, "bin"), 0700))
	binary := filepath.Join(pluginDir, "bin", "herdr-plugin-gh")
	_, err = checked(runCommand(context.Background(), commandRequest{Program: "go", Args: []string{"build", "-trimpath", "-o", binary, "."}, Cwd: root}))
	requireNoError(t, err)
	for _, file := range []string{"herdr-plugin.toml", "config.example.toml"} {
		data, err := os.ReadFile(filepath.Join(root, file))
		requireNoError(t, err)
		requireNoError(t, os.WriteFile(filepath.Join(pluginDir, file), data, 0600))
	}
	env := environment()
	for key := range env {
		if strings.HasPrefix(key, "HERDR_") {
			delete(env, key)
		}
	}
	env["XDG_CONFIG_HOME"], env["XDG_STATE_HOME"] = filepath.Join(directory, "config"), filepath.Join(directory, "state")
	env["HERDR_CONFIG_PATH"], env["HERDR_SOCKET_PATH"] = filepath.Join(directory, "config.toml"), filepath.Join(directory, "api.sock")
	env["HERDR_DISABLE_SOUND"], env["HERDR_PLUGIN_STATE_DIR"], env["HERDR_BIN_PATH"] = "1", filepath.Join(directory, "cache"), herdr
	config, err := os.ReadFile(filepath.Join(root, "config.example.toml"))
	requireNoError(t, err)
	requireNoError(t, os.WriteFile(env["HERDR_CONFIG_PATH"], config, 0600))
	p := newPlugin(env)
	var linked struct {
		Plugin struct {
			PluginID string   `json:"plugin_id"`
			Warnings []string `json:"warnings"`
			Panes    []struct {
				Title  string `json:"title"`
				Height int    `json:"height"`
			} `json:"panes"`
			Startup []struct {
				Command []string `json:"command"`
			} `json:"startup"`
			Build []struct {
				Command []string `json:"command"`
			} `json:"build"`
		} `json:"plugin"`
	}
	requireNoError(t, p.herdr(context.Background(), []string{"plugin", "link", pluginDir}, &linked))
	if linked.Plugin.PluginID != "gh" || len(linked.Plugin.Warnings) != 0 || len(linked.Plugin.Panes) != 1 || linked.Plugin.Panes[0].Height != 11 {
		t.Fatalf("manifest rejected: %+v", linked)
	}
	if !reflect.DeepEqual(linked.Plugin.Startup[0].Command, []string{"./bin/herdr-plugin-gh", "refresh", "--cached"}) {
		t.Fatal(linked.Plugin.Startup)
	}
	server := exec.Command(herdr, "server")
	server.Dir = directory
	for key, value := range env {
		server.Env = append(server.Env, key+"="+value)
	}
	var logs synchronizedBuffer
	server.Stdout, server.Stderr = &logs, &logs
	requireNoError(t, server.Start())
	exited := make(chan struct{})
	go func() { _ = server.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = server.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			_ = server.Process.Kill()
			<-exited
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		connection, err := net.DialTimeout("unix", env["HERDR_SOCKET_PATH"], 50*time.Millisecond)
		if err == nil {
			connection.Close()
			break
		}
		select {
		case <-exited:
			t.Fatalf("Herdr exited: %s", logs.text())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("Herdr did not start: %s", logs.text())
		}
		time.Sleep(20 * time.Millisecond)
	}
	var workspace struct {
		RootPane Pane `json:"root_pane"`
	}
	requireNoError(t, p.herdr(context.Background(), []string{"workspace", "create", "--cwd", directory, "--label", "GitHub plugin test"}, &workspace))
	if workspace.RootPane.ID == "" {
		t.Fatal("workspace has no pane")
	}
	paneID := workspace.RootPane.ID
	result, err := runCommand(context.Background(), commandRequest{Program: binary, Args: []string{"refresh"}, Cwd: pluginDir, Env: env})
	requireNoError(t, err)
	if result.Code != 0 || !strings.Contains(result.Stdout, "Refreshed GitHub status for all Agents") {
		t.Fatal(result)
	}
	var actions struct {
		Actions []struct {
			ID string `json:"action_id"`
		} `json:"actions"`
	}
	requireNoError(t, p.herdr(context.Background(), []string{"plugin", "action", "list", "--plugin", "gh"}, &actions))
	var ids []string
	for _, action := range actions.Actions {
		ids = append(ids, action.ID)
	}
	slices.Sort(ids)
	if !reflect.DeepEqual(ids, []string{"copy-slack", "menu", "open", "open-checks", "refresh"}) {
		t.Fatal(ids)
	}
	enabled, err := p.settings(context.Background(), nil)
	requireNoError(t, err)
	if enabled {
		t.Fatal("unexpected default")
	}
	_, err = p.settings(context.Background(), ptr(true))
	requireNoError(t, err)
	enabled, err = newPlugin(env).settings(context.Background(), nil)
	requireNoError(t, err)
	if !enabled {
		t.Fatal("preference not persisted")
	}
	var reloaded struct {
		Diagnostics []json.RawMessage `json:"diagnostics"`
	}
	requireNoError(t, p.herdr(context.Background(), []string{"server", "reload-config"}, &reloaded))
	if len(reloaded.Diagnostics) != 0 {
		t.Fatal(reloaded)
	}
	state := State{PR: testPR(), Branch: "feature/checks", Unresolved: ptr(int64(3)), Checks: []Check{{Bucket: "fail"}, {Bucket: "pending"}, {Bucket: "pass"}, {Bucket: "cancel"}, {Bucket: "skipping"}, {Bucket: "future"}}}
	requireNoError(t, p.herdr(context.Background(), []string{"pane", "report-metadata", paneID, "--source", "plugin:gh", "--token", "gh_actions=old", "--token", "gh_unresolved=old"}, nil))
	requireNoError(t, p.report(context.Background(), paneID, state))
	pane, err := p.pane(context.Background(), paneID)
	requireNoError(t, err)
	for key, value := range tokensFor(state) {
		if pane.Tokens[key] != value {
			t.Errorf("%s: %q != %q", key, pane.Tokens[key], value)
		}
	}
	_, err = p.refresh(context.Background(), paneID, refreshOptions{})
	requireNoError(t, err)
	pane, err = p.pane(context.Background(), paneID)
	requireNoError(t, err)
	for _, key := range tokenOrder {
		if pane.Tokens[key] != "" {
			t.Error("stale token", key, pane.Tokens[key])
		}
	}
	env["HERDR_PLUGIN_CONTEXT_JSON"] = `{"focused_pane_id":"` + paneID + `"}`
	result, err = runCommand(context.Background(), commandRequest{Program: binary, Args: []string{"menu"}, Cwd: pluginDir, Env: env})
	requireNoError(t, err)
	if result.Code != 0 {
		t.Fatal(result)
	}
	// Let the real popup initialize its terminal and launch the Go event loop.
	time.Sleep(150 * time.Millisecond)
	connection, err := net.DialTimeout("unix", env["HERDR_SOCKET_PATH"], time.Second)
	requireNoError(t, err)
	defer connection.Close()
	requireNoError(t, connection.SetDeadline(time.Now().Add(5*time.Second)))
	requireNoError(t, json.NewEncoder(connection).Encode(map[string]any{"id": "close", "method": "popup.close", "params": map[string]any{}}))
	var closed struct {
		Error json.RawMessage `json:"error"`
	}
	requireNoError(t, json.NewDecoder(connection).Decode(&closed))
	if len(closed.Error) > 0 && string(closed.Error) != "null" {
		t.Fatal(string(closed.Error))
	}
}
