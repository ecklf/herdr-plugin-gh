package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
)

func cli(ctx context.Context, plugin *Plugin, args []string, stdout, stderr io.Writer) int {
	action := ""
	if len(args) > 0 {
		action = args[0]
	}
	quiet := slices.Contains(args, "--silent") || action == "refresh-auto" || plugin.Env["HERDR_PLUGIN_EVENT"] != ""
	err := func() error {
		switch action {
		case "", "--help", "-h":
			_, err := fmt.Fprintln(stdout, "Usage: herdr-plugin-gh <menu|open|refresh|open-checks|copy-slack> [--cached] [--silent]")
			return err
		case "refresh":
			if err := plugin.refreshAll(ctx, slices.Contains(args, "--cached")); err != nil {
				return err
			}
			if !quiet {
				_, err := fmt.Fprintln(stdout, "Refreshed GitHub status for all Agents.")
				return err
			}
			return nil
		case "menu", "popup", "refresh-auto", "open", "open-checks", "copy-slack":
			pane, err := plugin.target(ctx)
			if err != nil {
				return err
			}
			switch action {
			case "menu":
				return plugin.herdr(ctx, []string{"plugin", "pane", "open", "--plugin", pluginID, "--entrypoint", "menu", "--env", "HERDR_GH_PANE_ID=" + pane.ID, "--focus"}, nil)
			case "popup":
				return menu(ctx, plugin, pane.ID, os.Stdin, os.Stdout)
			case "refresh-auto":
				state, err := plugin.refresh(ctx, pane.ID, refreshOptions{Cached: true, AgentOnly: true})
				if err != nil {
					return err
				}
				if state != nil && state.Unavailable == "" && state.problem() != "" {
					return fmt.Errorf("%s", state.problem())
				}
				return nil
			default:
				state, err := plugin.act(ctx, pane.ID, action, nil)
				if err != nil {
					return err
				}
				if !quiet {
					if action == "copy-slack" {
						_, err = fmt.Fprintln(stdout, "Copied PR for Slack.")
					} else {
						tokens := tokensFor(*state)
						var values []string
						for _, key := range tokenOrder {
							if tokens[key] != "" {
								values = append(values, tokens[key])
							}
						}
						_, err = fmt.Fprintln(stdout, strings.Join(values, " | "))
					}
				}
				return err
			}
		default:
			return fmt.Errorf("Unknown action: %s", action)
		}
	}()
	if err == nil {
		return 0
	}
	fmt.Fprintln(stderr, clean(err.Error()))
	if !quiet && plugin.Env["HERDR_PLUGIN_ACTION_ID"] != "" && ctx.Err() == nil {
		body := []rune(clean(err.Error()))
		_ = plugin.herdr(ctx, []string{"notification", "show", "GitHub action failed", "--body", string(body[:min(len(body), 240)])}, nil)
	}
	return 1
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	code := cli(ctx, newPlugin(nil), os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
