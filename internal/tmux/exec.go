package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// paneFormat is the tmux format string that parsePanes expects.
const paneFormat = "#{pane_id}\t#{session_name}\t#{window_id}\t#{window_name}"

type execClient struct {
	socket string
}

// NewExec returns a Client that runs the tmux binary. An empty socket uses
// the default server; otherwise every command gets -L socket.
func NewExec(socket string) Client {
	return &execClient{socket: socket}
}

// run executes tmux with args as separate argv entries (never a shell).
func (c *execClient) run(ctx context.Context, args ...string) (string, error) {
	full := args
	if c.socket != "" {
		full = append([]string{"-L", c.socket}, args...)
	}
	cmd := exec.CommandContext(ctx, "tmux", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (c *execClient) ListPanes(ctx context.Context) ([]Pane, error) {
	out, err := c.run(ctx, "list-panes", "-a", "-F", paneFormat)
	if err != nil {
		return nil, err
	}
	return parsePanes(out)
}

func (c *execClient) PaneInfo(ctx context.Context, pane string) (Pane, error) {
	out, err := c.run(ctx, "display-message", "-p", "-t", pane, paneFormat)
	if err != nil {
		return Pane{}, err
	}
	panes, err := parsePanes(out)
	if err != nil {
		return Pane{}, err
	}
	if len(panes) != 1 {
		return Pane{}, fmt.Errorf("tmux pane info: got %d panes for %q", len(panes), pane)
	}
	return panes[0], nil
}

func (c *execClient) SwitchTo(ctx context.Context, pane string) error {
	_, err := c.run(ctx, "switch-client", "-t", pane)
	return err
}

func (c *execClient) SetWindowOption(ctx context.Context, window, name, value string) error {
	_, err := c.run(ctx, "set-option", "-w", "-t", window, "--", name, value)
	return err
}

func (c *execClient) DisplayMessage(ctx context.Context, text string) error {
	// display-message expands #{...} formats; doubling # makes text literal.
	// "--" stops text starting with "-" from being read as a flag.
	_, err := c.run(ctx, "display-message", "--", strings.ReplaceAll(text, "#", "##"))
	return err
}

func (c *execClient) KillPane(ctx context.Context, pane string) error {
	_, err := c.run(ctx, "kill-pane", "-t", pane)
	return err
}

// parsePanes parses paneFormat output, one pane per line. Blank lines are
// skipped; a window name may contain spaces or tabs.
func parsePanes(out string) ([]Pane, error) {
	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 4)
		// An unknown -t target expands to empty fields, so an empty id is malformed too.
		if len(f) != 4 || f[0] == "" {
			return nil, fmt.Errorf("tmux: malformed pane line %q", line)
		}
		panes = append(panes, Pane{ID: f[0], Session: f[1], WindowID: f[2], WindowName: f[3]})
	}
	return panes, nil
}
