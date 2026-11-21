package ui

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// command is one key of the session table. Every file that owns a view
// registers its keys from its own init, and the table's key handler and
// help line are derived from the registrations: a new view adds a file and
// one registerCommand call, and shows in the help line by itself.
type command struct {
	keys  []string                     // the key strings (tea.KeyMsg.String) that run it
	help  string                       // its label in the help line ("i detail"); "" leaves it out
	order int                          // its place in the help line; ties keep their label order
	run   func(Model) (Model, tea.Cmd) // what the key does
}

var commands []command

// registerCommand adds c to the table; a key registered twice is a bug.
func registerCommand(c command) {
	for _, k := range c.keys {
		if _, dup := lookupCommand(k); dup {
			panic(fmt.Sprintf("ui: key %q is registered twice", k))
		}
	}
	commands = append(commands, c)
}

func lookupCommand(key string) (command, bool) {
	i := slices.IndexFunc(commands, func(c command) bool { return slices.Contains(c.keys, key) })
	if i < 0 {
		return command{}, false
	}
	return commands[i], true
}

// commandHelp is the help line's list of keys, in order.
func commandHelp() string {
	cs := slices.Clone(commands)
	slices.SortFunc(cs, func(a, b command) int {
		if a.order != b.order {
			return a.order - b.order
		}
		return strings.Compare(a.help, b.help)
	})
	var parts []string
	for _, c := range cs {
		if c.help != "" {
			parts = append(parts, c.help)
		}
	}
	return strings.Join(parts, "  ")
}

// withView returns m with the factory of the view name; a nil factory
// removes it. The map is replaced, never mutated.
func (m Model) withView(name string, open func(arg string) tea.Model) Model {
	m.views = maps.Clone(m.views)
	if m.views == nil {
		m.views = map[string]func(string) tea.Model{}
	}
	m.views[name] = open
	return m
}

// childClosed marks the message with which a view shown as the table's
// child closes itself: the table shows again and starts a new refresh chain.
type childClosed interface{ closedChild() }

func init() {
	registerCommand(command{keys: []string{"q", "esc"}, help: "q quit", order: 130, run: func(m Model) (Model, tea.Cmd) { return m, tea.Quit }})
	registerCommand(command{keys: []string{"j", "down"}, run: func(m Model) (Model, tea.Cmd) { return m.moveTo(m.cursor + 1), nil }})
	registerCommand(command{keys: []string{"k", "up"}, run: func(m Model) (Model, tea.Cmd) { return m.moveTo(m.cursor - 1), nil }})
	registerCommand(command{keys: []string{"enter"}, help: "enter jump", order: 10, run: Model.jump})
	registerCommand(command{keys: []string{" "}, run: func(m Model) (Model, tea.Cmd) { return m.toggleTeam(), nil }})
	registerCommand(command{keys: []string{"/"}, help: "/ filter", order: 110, run: func(m Model) (Model, tea.Cmd) { m.filtering = true; return m, nil }})
	registerCommand(command{keys: []string{"x"}, help: "x kill", order: 120, run: func(m Model) (Model, tea.Cmd) { return m.askKill(), nil }})
	registerCommand(command{keys: []string{"b"}, help: "b burn", order: 100, run: func(m Model) (Model, tea.Cmd) { return m.toggleBurn(), nil }})
	registerCommand(command{keys: []string{"P"}, help: "P pause", order: 180, run: func(m Model) (Model, tea.Cmd) { return m.askPause(), nil }})
}
