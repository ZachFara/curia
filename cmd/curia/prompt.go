package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// promptTerminal asks on the controlling terminal for the confirmation and
// returns the answer; tests replace it. `setup approve`, `config rollback`
// and `mode bypass` use it.
var promptTerminal = func(prompt string) (string, error) { return promptTerminalAt("/dev/tty", prompt) }

// promptTerminalAt writes prompt to the terminal at path and reads one line
// from it, never from stdin, so piped input cannot answer. No terminal, or
// an end of input before a newline, is an error.
func promptTerminalAt(path, prompt string) (string, error) {
	tty, err := os.OpenFile(filepath.Clean(path), os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("no terminal to confirm on: %w", err)
	}
	defer func() { _ = tty.Close() }()
	if _, err = io.WriteString(tty, prompt); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(io.LimitReader(tty, 256)).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("no answer: %w", err)
	}
	return strings.TrimSpace(line), nil
}
