package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeRichClipboard(t *testing.T) {
	pr := testPR()
	pr.Title = `[envs] retries & "quotes" é 🚀`
	payload, err := slackPRPayload(pr, false)
	requireNoError(t, err)
	data, err := json.Marshal(payload)
	requireNoError(t, err)
	// Use a private pasteboard so verification never touches the user's clipboard.
	script := strings.Replace(macClipboardScript, "generalPasteboard()", "pasteboardWithUniqueName()", 1) + `
set resultData to current application's NSMutableDictionary's dictionary()
resultData's setObject:(board's stringForType:(current application's NSPasteboardTypeHTML)) forKey:"html"
resultData's setObject:(board's stringForType:(current application's NSPasteboardTypeString)) forKey:"plain"
set encoded to current application's NSJSONSerialization's dataWithJSONObject:resultData options:0 |error|:(missing value)
return (current application's NSString's alloc()'s initWithData:encoded encoding:(current application's NSUTF8StringEncoding)) as text
`
	text, err := checked(runCommand(context.Background(), commandRequest{Program: "/usr/bin/osascript", Args: []string{"-l", "AppleScript", "-e", script}, Input: string(data)}))
	requireNoError(t, err)
	var actual clipboardPayload
	requireNoError(t, json.Unmarshal([]byte(text), &actual))
	if actual != payload {
		t.Fatalf("native clipboard mismatch: %#v != %#v", actual, payload)
	}
}
