package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubHostHints(t *testing.T) {
	for _, tc := range []struct {
		config string
		env    map[string]string
		host   string
	}{
		{"remote.origin.url git@github.com:owner/repo.git", nil, "github.com"},
		{"remote.origin.url ssh://git@github.example.com/owner/repo.git", nil, "github.example.com"},
		{"remote.origin.url https://github.com/owner/repo.git\nremote.upstream.url git@github.com:base/repo.git", nil, "github.com"},
		{"remote.origin.url git@github.com:owner/repo.git\nremote.other.url git@github.example.com:owner/repo.git", nil, ""},
		{"remote.origin.url /local/repository", nil, ""},
		{"remote.origin.url git@github.com:owner/repo.git", map[string]string{"GH_REPO": "other.example.com/owner/repo"}, ""},
		{"remote.origin.url git@github.com:owner/repo.git", map[string]string{"GH_HOST": "other.example.com"}, ""},
	} {
		if got := githubHostHint(tc.config, tc.env); got != tc.host {
			t.Errorf("%s: host %q, want %q", tc.config, got, tc.host)
		}
	}
	ctx := withGitHubHost(context.Background(), "github.com")
	for _, tc := range []struct {
		args []string
		host string
	}{
		{[]string{"pr", "view", "--json", prFields}, "github.com"},
		{[]string{"api", "graphql", "--hostname", "github.example.com"}, "github.example.com"},
		{[]string{"pr", "checks", "https://github.example.com/owner/repo/pull/42"}, "github.example.com"},
		{[]string{"repo", "view", "git@github.example.com:owner/repo.git"}, "github.example.com"},
		{[]string{"run", "rerun", "1", "--repo", "github.example.com/owner/repo"}, "github.example.com"},
		{[]string{"auth", "token"}, ""},
	} {
		if got := githubCommandHost(ctx, tc.args); got != tc.host {
			t.Errorf("%v: host %q, want %q", tc.args, got, tc.host)
		}
	}
}

func TestCredentialLookupSharedAndHostScoped(t *testing.T) {
	p := newPlugin(map[string]string{})
	started := make(chan string, 2)
	release := make(chan struct{})
	var lookups atomic.Int64
	p.Execute = func(ctx context.Context, r commandRequest) (commandResult, error) {
		if r.Args[0] == "auth" {
			lookups.Add(1)
			host := r.Args[len(r.Args)-1]
			started <- host
			select {
			case <-release:
			case <-ctx.Done():
				return commandResult{}, ctx.Err()
			}
			return ok("token-for-" + host), nil
		}
		host := githubCommandHost(ctx, r.Args)
		primary, _ := tokenVariables(host)
		if r.Env[primary] != "token-for-"+host {
			t.Error("credential was not scoped to the command's host")
		}
		return ok(""), nil
	}
	var workers sync.WaitGroup
	ctx := withGitHubHost(context.Background(), "github.com")
	for _, host := range []string{"github.com", "github.com", "github.example.com", "github.example.com"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := p.command(ctx, "gh", []string{"api", "--hostname", host, "user"}, "/repo", "")
			if err != nil {
				t.Error(err)
			}
		}()
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			workers.Wait()
			t.Fatal("credential lookups did not start independently")
		}
	}
	close(release)
	workers.Wait()
	if lookups.Load() != 2 {
		t.Fatal("repeated keychain lookups", lookups.Load())
	}
	if p.Env["GH_TOKEN"] != "" || p.Env["GH_ENTERPRISE_TOKEN"] != "" {
		t.Fatal("cached credentials leaked into shared process environment")
	}
}

func TestCredentialEnvironmentOverridesAndFallback(t *testing.T) {
	for _, variable := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		t.Run(variable, func(t *testing.T) {
			host := "github.com"
			if strings.Contains(variable, "ENTERPRISE") {
				host = "github.example.com"
			}
			p := newPlugin(map[string]string{variable: "provided-token"})
			p.Execute = func(_ context.Context, r commandRequest) (commandResult, error) {
				if r.Args[0] == "auth" {
					t.Error("environment token triggered credential lookup")
				}
				if r.Env[variable] != "provided-token" {
					t.Error("environment token overwritten")
				}
				return ok(""), nil
			}
			_, err := p.command(context.Background(), "gh", []string{"api", "--hostname", host, "user"}, "", "")
			requireNoError(t, err)
		})
	}
	p := newPlugin(map[string]string{})
	var authCalls, apiCalls int
	p.Execute = func(_ context.Context, r commandRequest) (commandResult, error) {
		if r.Args[0] == "auth" {
			authCalls++
			return failed("Credentials unavailable"), nil
		}
		apiCalls++
		return failed("Please run gh auth login"), nil
	}
	for range 2 {
		_, err := checked(p.command(context.Background(), "gh", []string{"api", "--hostname", "github.com", "user"}, "", ""))
		requireError(t, err, "gh auth login")
	}
	if authCalls != 1 || apiCalls != 2 {
		t.Fatal(authCalls, apiCalls)
	}
}

func TestCredentialCancellationAllowsRetry(t *testing.T) {
	p := newPlugin(map[string]string{})
	started := make(chan struct{})
	var lookups atomic.Int64
	p.Execute = func(ctx context.Context, r commandRequest) (commandResult, error) {
		if r.Args[0] == "auth" {
			if lookups.Add(1) == 1 {
				close(started)
				<-ctx.Done()
				return commandResult{}, ctx.Err()
			}
			return ok("retry-token"), nil
		}
		if r.Env["GH_TOKEN"] != "retry-token" {
			t.Error("retry did not get a fresh credential")
		}
		return ok(""), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.command(ctx, "gh", []string{"api", "--hostname", "github.com", "user"}, "", "")
		done <- err
	}()
	<-started
	cancel()
	if !errors.Is(<-done, context.Canceled) {
		t.Fatal("credential lookup ignored cancellation")
	}
	_, err := p.command(context.Background(), "gh", []string{"api", "--hostname", "github.com", "user"}, "", "")
	requireNoError(t, err)
	if lookups.Load() != 2 {
		t.Fatal(lookups.Load())
	}
}

func TestRefreshKeepsCredentialsOutOfCache(t *testing.T) {
	f := newFixture(t)
	delete(f.p.Env, "GH_TOKEN")
	delete(f.p.Env, "GH_ENTERPRISE_TOKEN")
	execute := f.p.Execute
	var lookups atomic.Int64
	f.p.Execute = func(ctx context.Context, r commandRequest) (commandResult, error) {
		if r.Program == "gh" && r.Args[0] == "auth" {
			lookups.Add(1)
			return ok("memory-only-test-credential"), nil
		}
		if r.Program == "gh" && r.Env["GH_TOKEN"] != "memory-only-test-credential" {
			t.Error("GitHub request repeated native credential resolution")
		}
		return execute(ctx, r)
	}
	refreshFixture(t, f, refreshOptions{})
	data, err := os.ReadFile(f.p.cacheFile("w1:p1"))
	requireNoError(t, err)
	if strings.Contains(string(data), "memory-only-test-credential") || f.p.Env["GH_TOKEN"] != "" {
		t.Fatal("credential persisted outside the credential cache")
	}
	if lookups.Load() != 1 {
		t.Fatal("refresh repeated keychain lookup", lookups.Load())
	}
}

func TestStatusReadsRunConcurrently(t *testing.T) {
	for _, cancelRequests := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "cancel"}[cancelRequests], func(t *testing.T) {
			f := newFixture(t)
			started := make(chan struct{}, 3)
			release := make(chan struct{})
			f.before = func(ctx context.Context, r commandRequest) error {
				if r.Program == "gh" && ((len(r.Args) > 1 && r.Args[0] == "api") || (len(r.Args) > 1 && r.Args[1] == "checks")) {
					started <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := f.p.refresh(ctx, "w1:p1", refreshOptions{}); done <- err }()
			for range 3 {
				select {
				case <-started:
				case <-time.After(time.Second):
					cancel()
					<-done
					t.Fatal("an independent status read was blocked behind another")
				}
			}
			if cancelRequests {
				cancel()
			} else {
				close(release)
			}
			err := <-done
			if cancelRequests {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				files, err := os.ReadDir(f.p.StateDir)
				requireNoError(t, err)
				if len(files) != 0 {
					t.Fatal("canceled requests left cache or locks", files)
				}
			} else {
				requireNoError(t, err)
				if len(f.reports) != 1 || f.reports[0]["gh_review"] != "1\uf41f" || f.reports[0]["gh_passed"] != "1\uf42e" {
					t.Fatal(f.reports)
				}
			}
		})
	}
}
