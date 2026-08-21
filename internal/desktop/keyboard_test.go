package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Nothing in this file synthesises a keystroke. The Wtype driver is exercised
// against a recording stub written to a temp directory, so what is asserted is
// the argv that *would* have been handed to wtype — which is the only part of
// the driver worth asserting, and the only part that could hurt anybody.

// recorder writes a shell stub that appends its argv to a file, one argument
// per line, and returns the stub's path and the log's path. Argument-per-line
// is deliberate: it proves an argument containing whitespace stayed one
// argument, which is the whole guarantee.
func recorder(t *testing.T, exitCode int, stderr string) (binary, log string) {
	t.Helper()
	dir := t.TempDir()
	binary = filepath.Join(dir, "wtype-stub")
	log = filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> " + log + "; done\n"
	if stderr != "" {
		script += "printf '%s\\n' '" + stderr + "' >&2\n"
	}
	script += "exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return binary, log
}

func argv(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// TestLiteralFilter is the control-character rule at its lowest layer. Whatever
// a caller failed to validate, nothing that is not a printable character can
// reach argv.
func TestLiteralFilter(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		removed int
	}{
		{"plain text is untouched", "hello world", "hello world", 0},
		{"punctuation and symbols survive", `it's #1 — "quoted" (50%)`, `it's #1 — "quoted" (50%)`, 0},
		{"accents and emoji survive", "café 🎉 日本語", "café 🎉 日本語", 0},
		{"newline is removed", "rm -rf /\n", "rm -rf /", 1},
		{"carriage return is removed", "sudo reboot\r", "sudo reboot", 1},
		{"tab is removed", "a\tb", "ab", 1},
		{"escape is removed", "\x1b[31mred", "[31mred", 1},
		{"nul is removed", "a\x00b", "ab", 1},
		{"delete is removed", "a\x7fb", "ab", 1},
		{"zero-width space is removed", "pay\u200bme", "payme", 1},
		{"right-to-left override is removed", "safe\u202egnp.exe", "safegnp.exe", 1},
		{"line separator is removed", "one\u2028two", "onetwo", 1},
		{"paragraph separator is removed", "one\u2029two", "onetwo", 1},
		{"several are all counted", "a\nb\tc\rd", "abcd", 3},
		{"ordinary space is kept", "  spaced  out  ", "  spaced  out  ", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, removed := Literal(tc.in)
			if got != tc.want {
				t.Errorf("Literal(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if removed != tc.removed {
				t.Errorf("Literal(%q) removed %d, want %d", tc.in, removed, tc.removed)
			}
		})
	}
}

// TestKeysymVocabularyIsClosed: the only keys that resolve are the ones this
// package lists. Everything a chord or a modifier would need is absent, and
// absent is the answer.
func TestKeysymVocabularyIsClosed(t *testing.T) {
	for _, name := range []string{"enter", "Enter", " return ", "TAB", "escape", "esc",
		"backspace", "delete", "up", "down", "left", "right", "home", "end"} {
		if _, ok := Keysym(name); !ok {
			t.Errorf("Keysym(%q) should resolve", name)
		}
	}
	for _, name := range []string{"", "ctrl", "alt", "super", "shift", "f4", "ctrl+c",
		"Return; rm -rf /", "space", "a", "print", "sysrq", "insert"} {
		if sym, ok := Keysym(name); ok {
			t.Errorf("Keysym(%q) resolved to %q; the vocabulary must stay closed", name, sym)
		}
	}
}

// TestWtypeTypeArgv: the payload is one whole argument after `--`, so wtype can
// only read it as text. A payload that looks like an option is characters.
func TestWtypeTypeArgv(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"ordinary text", "hello world", []string{"--", "hello world"}},
		{"text that looks like an option", "-k Return", []string{"--", "-k Return"}},
		{"text that looks like a shell command", "; rm -rf ~", []string{"--", "; rm -rf ~"}},
		{"text that looks like stdin", "-", []string{"--", "-"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binary, log := recorder(t, 0, "")
			kb := &Wtype{Binary: binary, Timeout: 2 * time.Second}
			if err := kb.Type(context.Background(), tc.text); err != nil {
				t.Fatalf("Type: %v", err)
			}
			got := argv(t, log)
			if len(got) != len(tc.want) {
				t.Fatalf("argv = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("argv = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// TestWtypeRefusesControlCharactersByConstruction: the driver filters, so even
// a caller that skipped its own validation cannot deliver a newline — and it
// refuses rather than typing a silently shortened version.
func TestWtypeRefusesControlCharacters(t *testing.T) {
	binary, log := recorder(t, 0, "")
	kb := &Wtype{Binary: binary, Timeout: 2 * time.Second}
	err := kb.Type(context.Background(), "echo hi\n")
	if err == nil {
		t.Fatal("Type should refuse a payload containing a newline")
	}
	if got := argv(t, log); got != nil {
		t.Fatalf("nothing should have been executed, got argv %q", got)
	}
}

// TestWtypePressArgv: what reaches argv is a keysym from this package's table,
// never the caller's string.
func TestWtypePressArgv(t *testing.T) {
	binary, log := recorder(t, 0, "")
	kb := &Wtype{Binary: binary, Timeout: 2 * time.Second}
	if err := kb.Press(context.Background(), "ENTER"); err != nil {
		t.Fatalf("Press: %v", err)
	}
	got := argv(t, log)
	if len(got) != 2 || got[0] != "-k" || got[1] != "Return" {
		t.Fatalf("argv = %q, want [-k Return]", got)
	}
}

func TestWtypePressRefusesUnknownKey(t *testing.T) {
	binary, log := recorder(t, 0, "")
	kb := &Wtype{Binary: binary, Timeout: 2 * time.Second}
	if err := kb.Press(context.Background(), "ctrl+alt+delete"); err == nil {
		t.Fatal("Press should refuse a key outside the vocabulary")
	}
	if got := argv(t, log); got != nil {
		t.Fatalf("nothing should have been executed, got argv %q", got)
	}
}

// TestWtypeDescribeProbesWithoutTyping: doctor must be able to ask "would this
// work?" without pressing anything into whatever the user has open.
func TestWtypeDescribeProbesWithoutTyping(t *testing.T) {
	binary, log := recorder(t, 0, "")
	kb := &Wtype{Binary: binary, Timeout: 2 * time.Second}
	described, err := kb.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !strings.Contains(described, "wtype") {
		t.Errorf("Describe() = %q, want it to name the injector", described)
	}
	got := argv(t, log)
	if len(got) != 2 || got[0] != "--" || got[1] != "" {
		t.Fatalf("probe argv = %q, want [-- \"\"] — the probe must type nothing", got)
	}
}

// TestWtypeUnavailable: a missing binary or a refusing compositor is
// ErrNoKeyboard, which callers turn into one spoken sentence.
func TestWtypeUnavailable(t *testing.T) {
	t.Run("no binary", func(t *testing.T) {
		kb := &Wtype{Binary: filepath.Join(t.TempDir(), "absent"), Timeout: time.Second}
		if _, err := kb.Describe(context.Background()); !errors.Is(err, ErrNoKeyboard) {
			t.Fatalf("err = %v, want ErrNoKeyboard", err)
		}
	})
	t.Run("compositor refuses the protocol", func(t *testing.T) {
		binary, _ := recorder(t, 1, "compositor does not support the virtual keyboard protocol")
		kb := &Wtype{Binary: binary, Timeout: 2 * time.Second}
		_, err := kb.Describe(context.Background())
		if !errors.Is(err, ErrNoKeyboard) {
			t.Fatalf("err = %v, want ErrNoKeyboard", err)
		}
		if !strings.Contains(err.Error(), "virtual keyboard protocol") {
			t.Errorf("err = %v, want the compositor's own diagnostic kept for the operator", err)
		}
	})
}

// TestFakeKeyboardTypesNothing states the house rule as an assertion: the fake
// records, and no test in this tree may reach a real keyboard.
func TestFakeKeyboardRecords(t *testing.T) {
	kb := &FakeKeyboard{}
	if err := kb.Type(context.Background(), "hello"); err != nil {
		t.Fatalf("Type: %v", err)
	}
	if err := kb.Press(context.Background(), "enter"); err != nil {
		t.Fatalf("Press: %v", err)
	}
	if got := kb.Typed(); len(got) != 1 || got[0] != "hello" {
		t.Errorf("Typed() = %q, want [hello]", got)
	}
	if got := kb.Pressed(); len(got) != 1 || got[0] != "Return" {
		t.Errorf("Pressed() = %q, want [Return]", got)
	}
	if err := kb.Press(context.Background(), "f1"); err == nil {
		t.Error("the fake must refuse the keys the real injector refuses")
	}
}
