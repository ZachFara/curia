package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// paneFormat is the tmux format string that parsePanes expects. start_time
// is the server's start in unix seconds. The window name may hold tabs; the
// socket path comes last, so a tab inside it stays in one part and is then
// refused.
const paneFormat = "#{pane_id}\t#{session_name}\t#{window_id}\t#{start_time}\t#{pane_pid}\t#{window_name}\t#{socket_path}"

type execClient struct {
	socket string
	// runner runs one tmux command and returns its stdout. NewExec sets it
	// to execTmux; tests set a recorder.
	runner func(ctx context.Context, args ...string) (string, error)
	// openTTY opens a pane's tty for Bell. NewExec sets it to openTTY;
	// tests set a recorder.
	openTTY func(name string) (*os.File, error)
}

// NewExec returns a Client that runs the tmux binary. An empty socket uses
// the default server; otherwise every command gets -L socket.
func NewExec(socket string) Client {
	c := &execClient{socket: socket}
	c.runner = c.execTmux
	c.openTTY = openTTY
	return c
}

// run runs one tmux command through the client's runner.
func (c *execClient) run(ctx context.Context, args ...string) (string, error) {
	return c.runner(ctx, args...)
}

// execTmux executes tmux with args as separate argv entries (never a shell).
func (c *execClient) execTmux(ctx context.Context, args ...string) (string, error) {
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

// focusFormat is what PaneFocused asks about a pane. The pane id tells a real
// pane from an unknown target, which tmux expands to empty fields.
const focusFormat = "#{pane_id}\t#{pane_active}\t#{window_active}\t#{session_attached}"

func (c *execClient) PaneFocused(ctx context.Context, pane string) (bool, error) {
	out, err := c.run(ctx, "display-message", "-p", "-t", pane, focusFormat)
	if err != nil {
		return false, err
	}
	f := strings.Split(strings.TrimSuffix(out, "\n"), "\t")
	if len(f) != 4 || f[0] == "" {
		return false, fmt.Errorf("tmux pane focus: unexpected answer %q for %q", out, pane)
	}
	return f[1] == "1" && f[2] == "1" && f[3] != "0", nil
}

func (c *execClient) SwitchTo(ctx context.Context, pane string) error {
	_, err := c.run(ctx, "switch-client", "-t", pane)
	return err
}

// noWindow holds what tmux prints for a window target that does not exist:
// tmux 3.8 says "no such window", older releases "can't find window".
var noWindow = []string{"no such window", "can't find window"}

func (c *execClient) SetWindowOption(ctx context.Context, window, name, value string) error {
	_, err := c.run(ctx, "set-option", "-w", "-t", window, "--", name, value)
	if err != nil && slices.ContainsFunc(noWindow, func(s string) bool { return strings.Contains(err.Error(), s) }) {
		return fmt.Errorf("%w: %w", ErrNoWindow, err)
	}
	return err
}

func (c *execClient) DisplayMessage(ctx context.Context, text string) error {
	// "--" stops text starting with "-" from being read as a flag.
	_, err := c.run(ctx, "display-message", "--", escapeMessage(text))
	return err
}

func (c *execClient) KillPane(ctx context.Context, pane string) error {
	_, err := c.run(ctx, "kill-pane", "-t", pane)
	return err
}

// paneIDPattern matches a pane id, the only target SendKeys and Bell accept:
// a session, window or other target could resolve to a pane nobody chose.
var paneIDPattern = regexp.MustCompile(`^%[0-9]+$`)

// maxKeysRunes caps SendKeys text. Above about 1,000 characters Claude
// Code's prompt treats input as a paste and send-keys -l splits it.
const maxKeysRunes = 800

// checkKeys refuses what SendKeys must never type, whoever calls it.
func checkKeys(pane, text string) error {
	if !paneIDPattern.MatchString(pane) {
		return fmt.Errorf("tmux send-keys: refusing target %q: not a pane id", pane)
	}
	if !utf8.ValidString(text) {
		return errors.New("tmux send-keys: refusing text that is not valid UTF-8")
	}
	// One character is refused because at a permission prompt a single
	// digit selects an option at once.
	switch n := utf8.RuneCountInString(text); {
	case n < 2:
		return fmt.Errorf("tmux send-keys: refusing text of %d characters", n)
	case n > maxKeysRunes:
		return fmt.Errorf("tmux send-keys: refusing text of %d characters (limit %d)", n, maxKeysRunes)
	}
	for _, r := range text {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return fmt.Errorf("tmux send-keys: refusing text with control character %U", r)
		}
	}
	return nil
}

// keysStepTimeout bounds each of SendKeys's copy-mode and text steps, and
// enterTimeout its Enter step, each on its own (Ruling 81).
const (
	keysStepTimeout = time.Second
	enterTimeout    = 2 * time.Second
)

// SendKeys leaves any mode the pane is in, types text literally and presses
// Enter, as three tmux invocations: text and Enter in one invocation make the
// Enter a newline inside the prompt for long text.
//
// Each invocation has its own timeout. The copy-mode and text steps also end
// with ctx. Once the text step has succeeded the text is in the prompt and
// pressing Enter is the only good outcome, so the Enter step runs on a
// context detached from ctx's cancellation, with its own timeout (Ruling 81).
//
// The error wraps ErrPartialSend when the text may have reached the pane:
// when the Enter step fails for any reason, and when the text step fails
// once its context is done (a killed tmux client may already have typed the
// text). The line may then be in the prompt unsubmitted, or even submitted; a
// caller seeing ErrPartialSend must not retry by typing again. Plain errors,
// after which nothing was typed, come from a failed copy-mode step, from ctx
// being done before the text step starts (it is then never run), and from a
// text step that tmux itself failed while its context was live (such as no
// such pane).
func (c *execClient) SendKeys(ctx context.Context, pane, text string) error {
	if err := checkKeys(pane, text); err != nil {
		return err
	}
	// tmux reads a trailing ";" on an argument as a command separator and
	// drops it; "\;" keeps it as a literal ";".
	if strings.HasSuffix(text, ";") {
		text = strings.TrimSuffix(text, ";") + `\;`
	}
	if err := c.runFor(ctx, keysStepTimeout, "copy-mode", "-q", "-t", pane); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tctx, cancel := context.WithTimeout(ctx, keysStepTimeout)
	_, err := c.run(tctx, "send-keys", "-t", pane, "-l", "--", text)
	done := tctx.Err() != nil
	cancel()
	if err != nil {
		if done {
			return fmt.Errorf("%w: %w", ErrPartialSend, err)
		}
		return err
	}
	if err := c.runFor(context.WithoutCancel(ctx), enterTimeout, "send-keys", "-t", pane, "Enter"); err != nil {
		return fmt.Errorf("%w: %w", ErrPartialSend, err)
	}
	return nil
}

// runFor runs one tmux command on a context derived from ctx with its own
// timeout, and returns only the error.
func (c *execClient) runFor(ctx context.Context, timeout time.Duration, args ...string) error {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := c.run(rctx, args...)
	return err
}

// syncFormat is what CapturePane asks about a pane before capturing it. The
// pane id tells a real pane from an unknown target, which tmux expands to
// empty fields.
const syncFormat = "#{pane_id}\t#{synchronize-panes}\t#{cursor_x}\t#{cursor_y}"

// cursorFormat is what CapturePane asks again after the capture.
const cursorFormat = "#{pane_id}\t#{cursor_x}\t#{cursor_y}"

func (c *execClient) CapturePane(ctx context.Context, pane string) (Capture, error) {
	if !paneIDPattern.MatchString(pane) {
		return Capture{}, fmt.Errorf("tmux capture-pane: refusing target %q: not a pane id", pane)
	}
	out, err := c.run(ctx, "display-message", "-p", "-t", pane, syncFormat)
	if err != nil {
		return Capture{}, err
	}
	f := strings.Split(strings.TrimSuffix(out, "\n"), "\t")
	if len(f) != 4 || f[0] != pane {
		return Capture{}, fmt.Errorf("tmux capture-pane: unexpected answer %q for %q", out, pane)
	}
	styled, err := c.run(ctx, "capture-pane", "-e", "-p", "-t", pane)
	if err != nil {
		return Capture{}, err
	}
	capt := Capture{Screen: StripEscapes(styled), Styled: styled, Synchronized: f[1] == "1"}
	after, err := c.run(ctx, "display-message", "-p", "-t", pane, cursorFormat)
	if err != nil {
		return Capture{}, err
	}
	g := strings.Split(strings.TrimSuffix(after, "\n"), "\t")
	x, xerr := strconv.Atoi(f[2])
	y, yerr := strconv.Atoi(f[3])
	if xerr == nil && yerr == nil && len(g) == 3 && g[0] == pane && g[1] == f[2] && g[2] == f[3] {
		capt.CursorX, capt.CursorY, capt.CursorKnown = x, y, true
	}
	return capt, nil
}

// StripEscapes removes the escape sequences capture-pane -e writes (CSI
// sequences such as SGR, and OSC sequences such as hyperlinks) from s. Any
// other escape character is removed alone.
func StripEscapes(s string) string {
	if !strings.ContainsRune(s, '\x1b') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '\x1b' {
			b.WriteByte(s[i])
			i++
			continue
		}
		i += escapeLen(s[i:])
	}
	return b.String()
}

// escapeLen is the length of the escape sequence at the start of s, which
// starts with ESC: a CSI sequence ends at its final byte (0x40 to 0x7e), an
// OSC one at BEL or ESC \ (or the end of the line); anything else is the
// ESC alone.
func escapeLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	switch s[1] {
	case '[':
		for j := 2; j < len(s); j++ {
			if s[j] >= 0x40 && s[j] <= 0x7e {
				return j + 1
			}
			if s[j] == '\n' {
				return j
			}
		}
		return len(s)
	case ']':
		for j := 2; j < len(s); j++ {
			switch {
			case s[j] == '\a':
				return j + 1
			case s[j] == '\x1b' && j+1 < len(s) && s[j+1] == '\\':
				return j + 2
			case s[j] == '\n':
				return j
			}
		}
		return len(s)
	}
	return 1
}

// windowTimeout bounds NewWindow, all its tmux invocations together.
const windowTimeout = 5 * time.Second

// NewWindow's limits. tmux reads an argument equal to ";" or ending in ";" as
// a command separator, so nothing NewWindow passes on may end in one.
var (
	windowNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
	windowEnvPattern  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}=[A-Za-z0-9._/:@-]{0,256}$`)
	// plainWordPattern is what a one-argument command must be: tmux runs a
	// single argument with its default-shell's -c, which then reads a plain
	// word, holding nothing a shell expands or splits, as a bare command.
	plainWordPattern = regexp.MustCompile(`^[A-Za-z0-9._/@:+][A-Za-z0-9._/@:+-]*$`)
)

const (
	maxWindowEnv  = 8
	maxWindowArgs = 16
	maxArgBytes   = 4096
)

// checkWindow refuses a WindowSpec NewWindow must never pass to tmux.
func checkWindow(w WindowSpec) error {
	if !windowNamePattern.MatchString(w.Session) {
		return fmt.Errorf("tmux new-window: refusing session name %q", w.Session)
	}
	if !windowNamePattern.MatchString(w.Name) {
		return fmt.Errorf("tmux new-window: refusing window name %q", w.Name)
	}
	if !filepath.IsAbs(w.Dir) || filepath.Clean(w.Dir) != w.Dir || !commandText(w.Dir) {
		return fmt.Errorf("tmux new-window: refusing directory %q", w.Dir)
	}
	if len(w.Env) > maxWindowEnv {
		return fmt.Errorf("tmux new-window: refusing %d environment entries (limit %d)", len(w.Env), maxWindowEnv)
	}
	for _, e := range w.Env {
		if !windowEnvPattern.MatchString(e) {
			return fmt.Errorf("tmux new-window: refusing environment entry %q", e)
		}
	}
	return checkWindowCommand(w.Command)
}

// checkWindowCommand refuses a command NewWindow must never pass to tmux.
func checkWindowCommand(command []string) error {
	if n := len(command); n < 1 || n > maxWindowArgs {
		return fmt.Errorf("tmux new-window: refusing a command of %d arguments", n)
	}
	for _, a := range command {
		if a == "" || len(a) > maxArgBytes || !commandText(a) {
			return fmt.Errorf("tmux new-window: refusing command argument %q", a)
		}
	}
	if len(command) == 1 && !plainWordPattern.MatchString(command[0]) {
		return fmt.Errorf("tmux new-window: refusing one-argument command %q: tmux would give it to a shell", command[0])
	}
	return nil
}

// commandText reports whether s may go on tmux's command line as one
// argument: valid UTF-8, no rune below 0x20, not ending in ";".
func commandText(s string) bool {
	if !utf8.ValidString(s) || strings.HasSuffix(s, ";") {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 })
}

// formatEscaper doubles "#", which tmux expands as a format in -c.
var formatEscaper = strings.NewReplacer("#", "##")

// NewWindow opens a detached window running w.Command in w.Dir. It asks
// has-session whether w.Session exists (an exact match) and runs new-window
// in it or new-session to create it; a new-session that finds the session
// created meanwhile by another process is retried once as new-window.
func (c *execClient) NewWindow(ctx context.Context, w WindowSpec) (Pane, error) {
	if err := checkWindow(w); err != nil {
		return Pane{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, windowTimeout)
	defer cancel()
	tail := []string{"-n", w.Name, "-c", formatEscaper.Replace(w.Dir)}
	for _, e := range w.Env {
		tail = append(tail, "-e", e)
	}
	tail = append(append(tail, "--"), w.Command...)
	newWindow := append([]string{"new-window", "-d", "-P", "-F", paneFormat, "-t", "=" + w.Session + ":"}, tail...)
	var out string
	var err error
	if _, herr := c.run(ctx, "has-session", "-t", "="+w.Session); herr == nil {
		out, err = c.run(ctx, newWindow...)
	} else {
		out, err = c.run(ctx, append([]string{"new-session", "-d", "-P", "-F", paneFormat, "-s", w.Session}, tail...)...)
		if err != nil && strings.Contains(err.Error(), "duplicate session") {
			out, err = c.run(ctx, newWindow...)
		}
	}
	if err != nil {
		return Pane{}, err
	}
	panes, err := parsePanes(out)
	if err != nil {
		return Pane{}, err
	}
	if len(panes) != 1 {
		return Pane{}, fmt.Errorf("tmux new-window: got %d panes for window %q", len(panes), w.Name)
	}
	return panes[0], nil
}

// maxTailLines caps CaptureTail; it is tmux's default history-limit.
const maxTailLines = 2000

// CaptureTail captures the pane from lines rows up its history to the bottom
// of the screen, with wrapped lines joined, drops the blank rows at the
// bottom of the screen and returns the last lines rows, each ending in a
// newline.
func (c *execClient) CaptureTail(ctx context.Context, pane string, lines int) (string, error) {
	if !paneIDPattern.MatchString(pane) {
		return "", fmt.Errorf("tmux capture-pane: refusing target %q: not a pane id", pane)
	}
	if lines < 1 || lines > maxTailLines {
		return "", fmt.Errorf("tmux capture-pane: refusing %d lines (1 to %d)", lines, maxTailLines)
	}
	out, err := c.run(ctx, "capture-pane", "-p", "-J", "-t", pane, "-S", "-"+strconv.Itoa(lines))
	if err != nil {
		return "", err
	}
	rows := strings.Split(out, "\n")
	for len(rows) > 0 && strings.TrimSpace(rows[len(rows)-1]) == "" {
		rows = rows[:len(rows)-1]
	}
	if len(rows) == 0 {
		return "", nil
	}
	return strings.Join(rows[max(0, len(rows)-lines):], "\n") + "\n", nil
}

// ttyPattern matches a pane's tty as tmux reports it: a pty, named
// /dev/ttysN on macOS and /dev/pts/N on Linux. An unknown pane yields junk
// with exit 0, so nothing else is ever opened (Ruling 55).
var ttyPattern = regexp.MustCompile(`^/dev/(ttys[0-9]+|pts/[0-9]+)$`)

// bellTimeout bounds the write of BEL when the context has no deadline, so a
// flow-stopped pane cannot block the caller.
const bellTimeout = time.Second

// openTTY opens a tty write-only without making it the controlling terminal.
func openTTY(name string) (*os.File, error) {
	return os.OpenFile(filepath.Clean(name), os.O_WRONLY|syscall.O_NOCTTY, 0)
}

// Bell writes BEL to the pane's tty, which sets the window's bell flag. It
// writes only to a character device at a pty path, and the write carries the
// context's deadline, or bellTimeout when it has none.
func (c *execClient) Bell(ctx context.Context, pane string) error {
	if !paneIDPattern.MatchString(pane) {
		return fmt.Errorf("tmux bell: refusing target %q: not a pane id", pane)
	}
	out, err := c.run(ctx, "display-message", "-p", "-t", pane, "#{pane_tty}")
	if err != nil {
		return err
	}
	tty := strings.TrimSuffix(out, "\n")
	if !ttyPattern.MatchString(tty) {
		return fmt.Errorf("tmux bell: unexpected tty %q for %q", out, pane)
	}
	f, err := c.openTTY(tty)
	if err != nil {
		return fmt.Errorf("tmux bell: %w", err)
	}
	err = ringBell(ctx, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("tmux bell: %w", err)
	}
	return nil
}

// ringBell writes BEL to f once f is known to be a character device, with a
// write deadline from ctx (or bellTimeout).
func ringBell(ctx context.Context, f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("%s is not a terminal (mode %s)", f.Name(), info.Mode())
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(bellTimeout)
	}
	if err = f.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	_, err = f.Write([]byte{0x07})
	return err
}

// parsePanes parses paneFormat output, one pane per line. Blank lines are
// skipped; a window name may contain spaces. The pane pid is required. tmux
// escapes a tab in a window name, so a seventh part that does not start with
// "/" is taken as the rest of a name that holds a tab, never as a socket; a
// six-field line (no socket) has an empty Socket. An absolute seventh part
// that fails the socket rules gives an empty Socket.
func parsePanes(out string) ([]Pane, error) {
	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 7)
		// An unknown -t target expands to empty fields, so an empty id is malformed too.
		if len(f) < 6 || f[0] == "" {
			return nil, fmt.Errorf("tmux: malformed pane line %q", line)
		}
		start, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("tmux: bad server start time in pane line %q", line)
		}
		pid, err := strconv.Atoi(f[4])
		if err != nil {
			return nil, fmt.Errorf("tmux: bad pane pid in pane line %q", line)
		}
		name, socket := f[5], ""
		if len(f) == 7 {
			if strings.HasPrefix(f[6], "/") {
				socket = usableSocket(f[6])
			} else {
				name += "\t" + f[6]
			}
		}
		panes = append(panes, Pane{
			ID: f[0], Session: f[1], WindowID: f[2], ServerStart: time.Unix(start, 0), PID: pid, WindowName: name, Socket: socket,
		})
	}
	return panes, nil
}

// maxSocketPath bounds a socket path kept from tmux output.
const maxSocketPath = 256

// usableSocket returns p when it is an absolute path of at most maxSocketPath
// bytes, valid UTF-8, with no rune below 0x20; otherwise "".
func usableSocket(p string) string {
	if len(p) > maxSocketPath || !strings.HasPrefix(p, "/") || !utf8.ValidString(p) {
		return ""
	}
	for _, r := range p {
		if r < 0x20 {
			return ""
		}
	}
	return p
}

// messageEscaper doubles the characters display-message expands: # starts
// formats and style codes, % starts strftime conversions.
var messageEscaper = strings.NewReplacer("#", "##", "%", "%%")

// escapeMessage makes text display literally in display-message.
func escapeMessage(text string) string {
	return messageEscaper.Replace(text)
}
