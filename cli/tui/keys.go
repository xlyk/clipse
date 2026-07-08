package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap is the dashboard's key bindings, declared once (rather than matched
// against raw rune strings scattered through Update) so the same set drives
// both key handling and the bubbles help view. It satisfies help.KeyMap via
// ShortHelp/FullHelp.
type keyMap struct {
	Up         key.Binding
	Down       key.Binding
	Enter      key.Binding
	Back       key.Binding
	Kanban     key.Binding
	Help       key.Binding
	ScrollUp   key.Binding
	ScrollDown key.Binding
	Quit       key.Binding
}

// defaultKeyMap returns the standard bindings: vim-style j/k plus arrows for
// navigation, enter/esc to open and leave the detail view, tab (or v) to flip
// to the kanban board, ? for the help overlay, pgup/pgdn (and ctrl+u/ctrl+d)
// to scroll, and q/ctrl+c to quit.
func defaultKeyMap() keyMap {
	return keyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		Enter: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "details"),
		),
		Back: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "back"),
		),
		Kanban: key.NewBinding(
			key.WithKeys("tab", "v"),
			key.WithHelp("tab", "board"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		ScrollUp: key.NewBinding(
			key.WithKeys("pgup", "ctrl+u"),
			key.WithHelp("pgup", "scroll up"),
		),
		ScrollDown: key.NewBinding(
			key.WithKeys("pgdown", "ctrl+d"),
			key.WithHelp("pgdn", "scroll down"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
	}
}

// ShortHelp is the single-line hint set (help.KeyMap).
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Enter, k.Kanban, k.Help, k.Quit}
}

// FullHelp is the expanded, column-grouped hint set shown when the help
// overlay is toggled open (help.KeyMap).
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Enter, k.Back},
		{k.Kanban, k.Help},
		{k.ScrollUp, k.ScrollDown, k.Quit},
	}
}
