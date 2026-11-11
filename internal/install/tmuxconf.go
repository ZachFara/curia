package install

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	fenceBegin = "# >>> curia >>>"
	fenceEnd   = "# <<< curia <<<"
)

// snippetTemplate is the tmux configuration Curia installs; %[1]s is the
// binary path.
const snippetTemplate = `set -g status-interval 2
set -ag status-right ' #(%[1]s status)'
set -g window-status-format '#{?#{==:#{@curia_marker},red},#[fg=red]● #[default],}#I:#W'
set -g window-status-current-format '#{?#{==:#{@curia_marker},red},#[fg=red]● #[default],}#I:#W'
bind a display-popup -E -w 90%% -h 80%% '%[1]s ui'
bind N run-shell '%[1]s next'
set-hook -g pane-focus-in 'run-shell -b "%[1]s seen #{pane_id}"'
`

// WriteTmux writes the Curia tmux snippet to snippetPath and adds one fenced
// source-file line for it to the tmux configuration at tmuxConfPath, creating
// either file if missing. If the fence is already there the configuration is
// left as it is.
func WriteTmux(snippetPath, tmuxConfPath, bin string) error {
	snippetPath = filepath.Clean(snippetPath)
	tmuxConfPath = filepath.Clean(tmuxConfPath)
	conf, err := readConf(tmuxConfPath)
	if err != nil {
		return err
	}
	if err := writeAtomic(snippetPath, []byte(fmt.Sprintf(snippetTemplate, bin))); err != nil {
		return err
	}
	if hasFence(conf) {
		return nil
	}
	if conf != "" && !strings.HasSuffix(conf, "\n") {
		conf += "\n"
	}
	conf += fenceBegin + "\nsource-file '" + snippetPath + "'\n" + fenceEnd + "\n"
	return writeAtomic(tmuxConfPath, []byte(conf))
}

// RemoveTmux removes the fenced block, fence lines included, from the tmux
// configuration at tmuxConfPath. A missing file or one without the block is
// left alone; a begin fence with no end fence is an error and changes nothing.
func RemoveTmux(tmuxConfPath string) error {
	tmuxConfPath = filepath.Clean(tmuxConfPath)
	conf, err := readConf(tmuxConfPath)
	if err != nil {
		return err
	}
	lines := strings.SplitAfter(conf, "\n")
	var out []string
	inFence, found := false, false
	for _, line := range lines {
		switch {
		case !inFence && strings.TrimRight(line, "\r\n") == fenceBegin:
			inFence, found = true, true
		case inFence && strings.TrimRight(line, "\r\n") == fenceEnd:
			inFence = false
		case !inFence:
			out = append(out, line)
		}
	}
	if inFence {
		return fmt.Errorf("%s: %q has no matching %q; remove the block by hand", tmuxConfPath, fenceBegin, fenceEnd)
	}
	if !found {
		return nil
	}
	return writeAtomic(tmuxConfPath, []byte(strings.Join(out, "")))
}

// hasFence reports whether conf has a begin fence line, with or without a
// trailing newline.
func hasFence(conf string) bool {
	for _, line := range strings.Split(conf, "\n") {
		if strings.TrimRight(line, "\r") == fenceBegin {
			return true
		}
	}
	return false
}

// readConf returns the file's text, or "" when it does not exist.
func readConf(path string) (string, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}
