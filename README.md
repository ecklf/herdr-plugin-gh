# herdr-plugin-gh

GitHub PRs, CI status, and unresolved review counts in [Herdr](https://herdr.dev)'s
Agents sidebar. Open or create PRs, view checks and comments, rerun failed jobs,
update PR branches, and copy PRs for Slack from a popup.

## Requirements

- Herdr 0.8.2+ on macOS or Linux; 0.9.0+ for styled sidebar tokens.
- Go 1.24+ (to build), Git, and a current [GitHub CLI](https://cli.github.com/) on the Herdr server's `PATH`.
- A [Nerd Font](https://www.nerdfonts.com/) for icons.
- Linux: `xdg-open` for check links; `wl-copy` (Wayland) or `xclip` (X11) for copying.

## Setup

```sh
gh auth login
herdr plugin install ecklf/herdr-plugin-gh
```

Merge the rows and shortcuts from [config.example.toml](config.example.toml) into
`~/.config/herdr/config.toml` (or `HERDR_CONFIG_PATH`), keeping existing settings.
Installation builds the Go binary and registers actions; sidebar rows and shortcuts require this configuration.
With a Herdr session running:

```sh
herdr server reload-config
herdr plugin action invoke gh.refresh
```

Automatic refreshes use a 30-second cache; status expires after five minutes.
Enable the one-minute polling entry in the example config to keep idle Agents updated.

## Usage

Focus an Agent's pane inside a Git checkout, then use these example bindings:

| Shortcut | Action | Behavior |
| --- | --- | --- |
| Prefix, Alt+G | `gh.menu` | Open the popup |
| Prefix, Alt+O | `gh.open` | Open the PR or GitHub's creation form |
| Prefix, Alt+V | `gh.open-checks` | Open the first failing check or PR Checks tab |
| Prefix, Alt+S | `gh.copy-slack` | Copy the PR for Slack |
| Prefix, Alt+A | `gh.refresh` | Refresh all Agents, bypassing the cache |

The default prefix is **Ctrl+B**; release it before pressing the shortcut.
In the popup, use **1–5**, arrows/Enter, or clicks to select actions; **r** refreshes,
**c** opens unresolved comments, **f** reruns failed GitHub Actions jobs, and **q/Escape**
closes it. Reruns and remote branch updates require **y** confirmation.
PR creation requires a pushed branch with an upstream; submit the PR in the browser.

On the copy row, **Space** toggles saved Markdown Mode. Standard mode copies rich
HTML; paste normally in Slack to preserve formatting. Clipboard and browser actions
run on the Herdr server; remote client forwarding is unavailable.

For errors, check the popup or `herdr plugin log list --plugin gh`.

## Update or uninstall

Re-run the install command and refresh to update. To remove:

```sh
herdr plugin uninstall gh
```

Remove the plugin's rows, shortcuts, and polling entry from your config, then reload.
For a locally linked checkout, use `herdr plugin unlink gh` instead.
When migrating from Node.js, update any polling command to the binary path in the example config.

## Development

Build before linking a checkout; rebuild after source changes:

```sh
go build -trimpath -o bin/herdr-plugin-gh .
herdr plugin link "$PWD"
go test -race ./...
go vet ./...
HERDR_GH_INTEGRATION=1 go test -run TestHerdrIntegration # isolated server
```

## License

[MIT](LICENSE).
