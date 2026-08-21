package tools

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// The golden scene must land byte-for-byte: Excalidraw reads the file
// directly, so formatting, element order, and unrecognised extra fields
// (appState, files) all pass through untouched.
func TestExcalidrawGoldenSceneSavedVerbatim(t *testing.T) {
	golden, err := os.ReadFile("testdata/excalidraw_scene.excalidraw")
	if err != nil {
		t.Fatal(err)
	}
	if err := (&ExcalidrawRenderer{}).ValidateSource(string(golden)); err != nil {
		t.Fatalf("golden scene must validate: %v", err)
	}
	got := goldenArtifact(t, &ExcalidrawRenderer{}, "pipeline sketch", string(golden))
	if !bytes.Equal(got, golden) {
		t.Errorf("scene altered on save:\ngot:  %q\nwant: %q", got, golden)
	}
}

func TestExcalidrawValidatesMinimalScenes(t *testing.T) {
	r := &ExcalidrawRenderer{}
	for name, source := range map[string]string{
		"empty canvas": `{"type":"excalidraw","version":2,"elements":[]}`,
		"one element":  `{"type":"excalidraw","version":2,"elements":[{"type":"ellipse","x":0,"y":0}]}`,
		"extra fields": `{"type":"excalidraw","version":2,"elements":[],"appState":{},"files":{}}`,
	} {
		if err := r.ValidateSource(source); err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
	}
}

// Structural faults must fail with the specific problem named, so the
// model's one retry round has something to work with — and none of these
// may ever reach disk (the tool-level test covers that half).
func TestExcalidrawValidationErrorsAreSpecific(t *testing.T) {
	r := &ExcalidrawRenderer{}
	for name, tc := range map[string]struct {
		source string
		want   string
	}{
		"not JSON":           {`{"type": `, "not valid JSON"},
		"top-level array":    {`[]`, "single JSON object"},
		"missing type":       {`{"version":2,"elements":[]}`, `missing the "type"`},
		"wrong type":         {`{"type":"drawing","version":2,"elements":[]}`, `"type" must be the string "excalidraw"`},
		"missing version":    {`{"type":"excalidraw","elements":[]}`, `missing the "version"`},
		"string version":     {`{"type":"excalidraw","version":"2","elements":[]}`, "positive number"},
		"zero version":       {`{"type":"excalidraw","version":0,"elements":[]}`, "positive number"},
		"missing elements":   {`{"type":"excalidraw","version":2}`, `missing the "elements"`},
		"elements not array": {`{"type":"excalidraw","version":2,"elements":{}}`, "JSON array"},
		"element not object": {`{"type":"excalidraw","version":2,"elements":[5]}`, "element 0 is not a JSON object"},
		"element no type":    {`{"type":"excalidraw","version":2,"elements":[{"x":1,"y":2}]}`, `element 0 has no "type"`},
		"element no coords":  {`{"type":"excalidraw","version":2,"elements":[{"type":"rectangle","x":1}]}`, `missing numeric "x" and "y"`},
	} {
		err := r.ValidateSource(tc.source)
		if err == nil {
			t.Errorf("%s: must be rejected", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name the fault (%q)", name, err, tc.want)
		}
	}
}

// encoding/json decodes JSON null into a nil slice without complaint, so a
// scene with "elements": null used to validate and be saved — then fail to
// load in Excalidraw with its opaque "couldn't load" error. An empty array
// still means "empty canvas" and must keep passing
// (raised in review of #19).
func TestExcalidrawRejectsNullElementsButAllowsEmpty(t *testing.T) {
	r := &ExcalidrawRenderer{}
	if err := r.ValidateSource(`{"type":"excalidraw","version":2,"elements":null}`); err == nil {
		t.Error(`"elements": null must be rejected`)
	} else if !strings.Contains(err.Error(), "null") {
		t.Errorf("error must name the problem: %v", err)
	}
	if err := r.ValidateSource(`{"type":"excalidraw","version":2,"elements":[]}`); err != nil {
		t.Errorf("an empty canvas is valid: %v", err)
	}
}
