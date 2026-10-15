package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

type clipboardPayload struct {
	HTML  string `json:"html,omitempty"`
	Plain string `json:"plain"`
}

var slackPath = regexp.MustCompile(`^/([^/]+/[^/]+)/pull/(\d+)(?:/|$)`)
var markdownEscape = regexp.MustCompile("[\\\\\\[\\]*_`]")

func slackPRPayload(pr *PullRequest, markdown bool) (clipboardPayload, error) {
	u, err := webURL(pr.URL)
	if err != nil {
		return clipboardPayload{}, err
	}
	match := slackPath.FindStringSubmatch(u.Path)
	if match == nil {
		return clipboardPayload{}, errors.New("Invalid GitHub PR URL")
	}
	if strings.TrimSpace(pr.Title) == "" {
		return clipboardPayload{}, errors.New("Missing GitHub PR title")
	}
	link := u.Scheme + "://" + u.Host + strings.TrimSuffix(match[0], "/")
	emoji := ":pr:"
	switch {
	case pr.State == "MERGED":
		emoji = ":pr-merged:"
	case pr.State == "CLOSED":
		emoji = ":pr-closed:"
	case pr.IsDraft:
		emoji = ":pr-draft:"
	}
	title := strings.TrimSpace(clean(pr.Title)) + " (#" + match[2] + ")"
	diff := ""
	if pr.Additions != nil && pr.Deletions != nil && *pr.Additions >= 0 && *pr.Deletions >= 0 {
		diff = fmt.Sprintf(" +%d/-%d", *pr.Additions, *pr.Deletions)
	}
	if markdown {
		return clipboardPayload{Plain: emoji + " *" + match[1] + "* · [" + markdownEscape.ReplaceAllStringFunc(title, func(s string) string { return "\\" + s }) + "](" + link + ")*\u2060*" + diff}, nil
	}
	htmlEscape := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;")
	slackEscape := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return clipboardPayload{
		HTML:  emoji + " <b>" + htmlEscape.Replace(match[1]) + "</b> · <a href=\"" + htmlEscape.Replace(link) + "\">" + htmlEscape.Replace(title) + "</a>" + diff,
		Plain: emoji + " *" + slackEscape.Replace(match[1]) + "* · <" + link + "|" + slackEscape.Replace(title) + ">" + diff,
	}, nil
}

// AppleScriptObjC provides both native clipboard types without cgo or Node.js.
// The payload is read from stdin, never interpolated into executable source.
const macClipboardScript = `use framework "AppKit"
use scripting additions
set inputData to current application's NSFileHandle's fileHandleWithStandardInput()'s readDataToEndOfFile()
set payload to current application's NSJSONSerialization's JSONObjectWithData:inputData options:0 |error|:(missing value)
set clipboardItem to current application's NSPasteboardItem's alloc()'s init()
if not (clipboardItem's setString:(payload's objectForKey:"html") forType:(current application's NSPasteboardTypeHTML)) then error "Could not prepare HTML clipboard data"
if not (clipboardItem's setString:(payload's objectForKey:"plain") forType:(current application's NSPasteboardTypeString)) then error "Could not prepare plain clipboard data"
set board to current application's NSPasteboard's generalPasteboard()
board's clearContents()
if not (board's writeObjects:{clipboardItem}) then error "Could not write rich clipboard data"
`

func (p *Plugin) copyPR(ctx context.Context, pr *PullRequest, cwd string) error {
	markdown, err := p.settings(ctx, nil)
	if err != nil {
		return err
	}
	payload, err := slackPRPayload(pr, markdown)
	if err != nil {
		return err
	}
	mime, input := "text/plain", payload.Plain
	if payload.HTML != "" {
		mime, input = "text/html", payload.HTML
	}
	var program string
	var args []string
	switch {
	case p.Platform == "darwin" && payload.HTML != "":
		program, args = "/usr/bin/osascript", []string{"-l", "AppleScript", "-e", macClipboardScript}
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		input = string(data)
	case p.Platform == "darwin":
		program = "/usr/bin/pbcopy"
	case p.Env["WAYLAND_DISPLAY"] != "":
		program, args = "wl-copy", []string{"--type", mime}
	case p.Env["DISPLAY"] != "":
		program, args = "xclip", []string{"-selection", "clipboard", "-t", mime}
	default:
		return errors.New("Copy needs a desktop clipboard on the Herdr server (Wayland or X11).")
	}
	_, err = checked(p.command(ctx, program, args, cwd, input))
	if err != nil {
		return fmt.Errorf("Could not copy PR with %s: %w", program, err)
	}
	return nil
}
