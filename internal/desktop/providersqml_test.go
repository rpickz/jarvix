package desktop

import (
	"os"
	"strings"
	"testing"
)

// The Providers section (issue #163) puts a credential field on screen, and
// the one thing that must never happen there is the window learning a stored
// key. QML is the one place in this project that cannot be tested, so what
// can be checked is checked by scanning the source — the technique
// composer_test.go established for exactly this reason.
//
// Two invariants, both about the direction credentials travel:
//
//   - The window renders a credential's PRESENCE, from the daemon's `secrets`
//     report, and never a value or a mask of one. A mask whose length is the
//     value's length leaks the length, so no masking helper may appear here
//     either.
//   - The write goes out as an instruction (keep / set / clear), never as a
//     field of the entry the form round-trips — the entry map is echoed back,
//     validated, and quoted in problems, and a secret must not be in
//     something with that many destinations.

func TestProvidersFormNeverRendersACredential(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixWindow.qml"))
	if err != nil {
		t.Fatalf("reading JarvixWindow.qml: %v", err)
	}
	qml := string(source)

	if !strings.Contains(qml, `method: "config.list_entries"`) {
		t.Error("the Providers tab no longer lists through config.list_entries; " +
			"a second listing path would be a second place for a credential to appear (#163)")
	}
	if !strings.Contains(qml, `method: "config.test_entry"`) {
		t.Error("the Providers tab no longer offers the Test action, which is what " +
			"proves an endpoint answers before it is trusted (#163)")
	}

	// The write-only channel: the three instructions, and no other route.
	for _, want := range []string{
		`{ api_key: { action: "set", value: providerSecretValue } }`,
		`{ api_key: { action: "clear" } }`,
		`{ api_key: { action: "keep" } }`,
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("the credential instruction %s is gone; the form must send an "+
				"instruction about the stored key, never the key as a field (#163)", want)
		}
	}

	// providerDraftEntry is the serialiser the entry travels in. A credential
	// key appearing in it would be exactly the leak this design removes — and
	// the daemon refuses such an entry, so it would also be a broken form.
	body, ok := functionBody(qml, "function providerDraftEntry()")
	if !ok {
		t.Fatal("providerDraftEntry is gone from JarvixWindow.qml")
	}
	if strings.Contains(body, "api_key") && !strings.Contains(body, "api_key_env") {
		t.Error("providerDraftEntry touches api_key; the credential never travels " +
			"inside the entry (#163)")
	}
	for _, forbidden := range []string{"entry.api_key =", "entry.api_key="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("providerDraftEntry writes %s into the entry; the credential has "+
				"its own write-only channel (#163)", forbidden)
		}
	}

	// No masking anywhere near the credential: a mask is a shape of the value,
	// and the window is never given a value to shape.
	for _, forbidden := range []string{"maskKey", "maskedKey", `repeat("*"`, `repeat("•"`} {
		if strings.Contains(qml, forbidden) {
			t.Errorf("JarvixWindow.qml contains %q; a mask carries the value's length "+
				"and the window is never sent a value to mask (#163)", forbidden)
		}
	}
}

// functionBody extracts the text of one QML function by brace matching from
// its declaration, so an assertion can be about ONE function rather than the
// whole 5,000-line file.
func functionBody(qml, decl string) (string, bool) {
	start := strings.Index(qml, decl)
	if start < 0 {
		return "", false
	}
	depth := 0
	for i := start; i < len(qml); i++ {
		switch qml[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return qml[start : i+1], true
			}
		}
	}
	return "", false
}
