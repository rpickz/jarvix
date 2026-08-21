package setup

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// Prompter asks the user questions. The wizard never blocks on a broken
// stdin: implementations return the default on EOF, so a piped or
// non-interactive run degrades to "accept every default".
type Prompter interface {
	// Confirm asks a yes/no question and returns the answer.
	Confirm(question string, def bool) bool
	// Choose presents numbered options and returns the chosen index.
	Choose(question string, options []string, def int) int
	// Input asks for a free-text value, returning def when left empty.
	Input(question, def string) string
}

// TerminalPrompter is the interactive Prompter reading from a terminal.
type TerminalPrompter struct {
	in  *bufio.Reader
	out io.Writer
}

// NewTerminalPrompter builds a Prompter over the given streams.
func NewTerminalPrompter(in io.Reader, out io.Writer) *TerminalPrompter {
	return &TerminalPrompter{in: bufio.NewReader(in), out: out}
}

func (t *TerminalPrompter) readLine() (string, bool) {
	line, err := t.in.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		return "", false
	}
	return line, true
}

// Confirm implements Prompter.
func (t *TerminalPrompter) Confirm(question string, def bool) bool {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		fprintf(t.out, "%s %s ", question, hint)
		line, ok := t.readLine()
		if !ok || line == "" {
			return def
		}
		switch strings.ToLower(line) {
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fprintln(t.out, "Please answer y or n.")
	}
}

// Choose implements Prompter.
func (t *TerminalPrompter) Choose(question string, options []string, def int) int {
	if def < 0 || def >= len(options) {
		def = 0
	}
	fprintln(t.out, question)
	for i, opt := range options {
		fprintf(t.out, "  %d) %s\n", i+1, opt)
	}
	for {
		fprintf(t.out, "Choice [%d]: ", def+1)
		line, ok := t.readLine()
		if !ok || line == "" {
			return def
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
			return n - 1
		}
		fprintf(t.out, "Enter a number between 1 and %d.\n", len(options))
	}
}

// Input implements Prompter.
func (t *TerminalPrompter) Input(question, def string) string {
	if def != "" {
		fprintf(t.out, "%s [%s]: ", question, def)
	} else {
		fprintf(t.out, "%s: ", question)
	}
	line, ok := t.readLine()
	if !ok || line == "" {
		return def
	}
	return line
}
