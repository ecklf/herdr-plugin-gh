package main

import (
	"context"
	"net/url"
	"regexp"
	"strings"
)

type githubHostKey struct{}

type credentialLookup struct {
	ready    chan struct{}
	token    string
	canceled bool
}

func withGitHubHost(ctx context.Context, host string) context.Context {
	return context.WithValue(ctx, githubHostKey{}, host)
}

func remoteHost(remote string) string {
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err == nil && (u.Scheme == "https" || u.Scheme == "http" || u.Scheme == "ssh" || u.Scheme == "git") {
			return strings.ToLower(u.Hostname())
		}
		return ""
	}
	// Git's scp-style remotes, including SSH aliases, have no URL scheme.
	if _, hostPath, found := strings.Cut(remote, "@"); found {
		host, _, found := strings.Cut(hostPath, ":")
		if found {
			return strings.ToLower(host)
		}
	}
	return ""
}

var remoteURLKey = regexp.MustCompile(`^remote\..+\.url$`)
var githubHostname = regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)

// Only infer an implicit gh target when every remote agrees. Ambiguous remotes,
// GH_REPO overrides and SSH aliases can keep using gh's native authentication.
func githubHostHint(config string, env map[string]string) string {
	if env["GH_REPO"] != "" {
		return ""
	}
	host := ""
	for _, line := range strings.Split(config, "\n") {
		key, value, _ := strings.Cut(line, " ")
		if !remoteURLKey.MatchString(key) {
			continue
		}
		current := remoteHost(value)
		if current == "" || (host != "" && current != host) {
			return ""
		}
		host = current
	}
	if override := env["GH_HOST"]; override != "" && !strings.EqualFold(override, host) {
		return ""
	}
	return host
}

func githubCommandHost(ctx context.Context, args []string) string {
	if len(args) == 0 || args[0] == "auth" {
		return ""
	}
	for index, arg := range args {
		if arg == "--hostname" && index+1 < len(args) {
			return strings.ToLower(args[index+1])
		}
		if strings.HasPrefix(arg, "--hostname=") {
			return strings.ToLower(strings.TrimPrefix(arg, "--hostname="))
		}
		if (arg == "--repo" || arg == "-R") && index+1 < len(args) {
			repo := args[index+1]
			if host := remoteHost(repo); host != "" {
				return host
			}
			if parts := strings.Split(repo, "/"); len(parts) == 3 {
				return strings.ToLower(parts[0])
			}
		}
	}
	if len(args) > 2 && ((args[0] == "pr" && (args[1] == "view" || args[1] == "checks")) || (args[0] == "repo" && args[1] == "view")) {
		if host := remoteHost(args[2]); host != "" {
			return host
		}
	}
	host, _ := ctx.Value(githubHostKey{}).(string)
	return host
}

func tokenVariables(host string) (string, string) {
	if host == "github.com" || strings.HasSuffix(host, ".ghe.com") {
		return "GH_TOKEN", "GITHUB_TOKEN"
	}
	return "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"
}

// Keychain access can take seconds on macOS. Share one lookup per host across
// concurrent requests, retaining the credential only for this process's lifetime.
func (p *Plugin) githubCredential(ctx context.Context, host, cwd string, env map[string]string) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		p.credentialsMu.Lock()
		if existing := p.credentials[host]; existing != nil {
			p.credentialsMu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-existing.ready:
				if existing.canceled {
					continue
				}
				return existing.token, nil
			}
		}
		if p.credentials == nil {
			p.credentials = make(map[string]*credentialLookup)
		}
		entry := &credentialLookup{ready: make(chan struct{})}
		p.credentials[host] = entry
		p.credentialsMu.Unlock()
		result, err := p.Execute(ctx, commandRequest{Program: "gh", Args: []string{"auth", "token", "--hostname", host}, Cwd: cwd, Env: env})
		token := strings.TrimSpace(result.Stdout)
		if err == nil && result.Code == 0 && !strings.ContainsAny(token, "\r\n\x00") {
			entry.token = token
		}
		p.credentialsMu.Lock()
		if ctx.Err() != nil {
			entry.token, entry.canceled = "", true
			delete(p.credentials, host)
		}
		close(entry.ready)
		p.credentialsMu.Unlock()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// If token retrieval is unsupported or unavailable, preserve gh's normal
		// credential resolution and its original authentication error messages.
		return entry.token, nil
	}
}

func (p *Plugin) authenticateGitHub(ctx context.Context, args []string, cwd string, env map[string]string) error {
	host := githubCommandHost(ctx, args)
	if host == "" || !githubHostname.MatchString(host) {
		return nil
	}
	primary, fallback := tokenVariables(host)
	if env[primary] != "" || env[fallback] != "" {
		return nil
	}
	token, err := p.githubCredential(ctx, host, cwd, env)
	if err != nil {
		return err
	}
	if token != "" {
		env[primary] = token
	}
	return nil
}
