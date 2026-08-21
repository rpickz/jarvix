package tools

import (
	"encoding/json"
	"fmt"
)

// ExcalidrawRenderer is format "excalidraw": a scene JSON document the model
// writes, saved verbatim as a .excalidraw file and opened in the configured
// handler (typically a browser pointed at excalidraw.com, or a desktop
// wrapper). A passthrough with a validator: the scene is checked
// structurally before anything is written, because Excalidraw rejects a
// malformed scene with an opaque "couldn't load" — the model's mistake must
// surface here, where the specific error buys a retry, not in the user's
// browser.
type ExcalidrawRenderer struct{ passthrough }

// Format implements Renderer.
func (*ExcalidrawRenderer) Format() string { return "excalidraw" }

// SourceExt implements Renderer.
func (*ExcalidrawRenderer) SourceExt() string { return ".excalidraw" }

// OutputExt implements Renderer. Same as SourceExt: the saved scene is the
// artifact.
func (*ExcalidrawRenderer) OutputExt() string { return ".excalidraw" }

// ValidateSource implements SourceValidator by checking the scene's
// structure: valid JSON, a top-level object with `"type": "excalidraw"`, a
// positive numeric `version`, and an `elements` array whose entries are
// objects with a non-empty string `type` and numeric `x`/`y`. That is the
// shape every Excalidraw release requires to load a file; deeper per-element
// schemas churn between releases, so pinning them here would reject scenes
// Excalidraw itself accepts.
func (*ExcalidrawRenderer) ValidateSource(source string) error {
	var scene map[string]json.RawMessage
	if err := json.Unmarshal([]byte(source), &scene); err != nil {
		if json.Valid([]byte(source)) {
			return fmt.Errorf("scene must be a single JSON object, not an array or scalar")
		}
		return fmt.Errorf("scene is not valid JSON: %v", err)
	}

	var sceneType string
	if raw, ok := scene["type"]; !ok {
		return fmt.Errorf(`scene is missing the "type" field; it must be "excalidraw"`)
	} else if err := json.Unmarshal(raw, &sceneType); err != nil || sceneType != "excalidraw" {
		return fmt.Errorf(`scene "type" must be the string "excalidraw", got %s`, raw)
	}

	var version float64
	if raw, ok := scene["version"]; !ok {
		return fmt.Errorf(`scene is missing the "version" field; use 2`)
	} else if err := json.Unmarshal(raw, &version); err != nil || version < 1 {
		return fmt.Errorf(`scene "version" must be a positive number, got %s`, raw)
	}

	rawElements, ok := scene["elements"]
	if !ok {
		return fmt.Errorf(`scene is missing the "elements" array`)
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(rawElements, &elements); err != nil {
		return fmt.Errorf(`scene "elements" must be a JSON array of element objects`)
	}
	for i, raw := range elements {
		var element struct {
			Type string   `json:"type"`
			X    *float64 `json:"x"`
			Y    *float64 `json:"y"`
		}
		if err := json.Unmarshal(raw, &element); err != nil {
			return fmt.Errorf("element %d is not a JSON object", i)
		}
		if element.Type == "" {
			return fmt.Errorf(`element %d has no "type" (e.g. "rectangle", "arrow", "text")`, i)
		}
		if element.X == nil || element.Y == nil {
			return fmt.Errorf(`element %d (%s) is missing numeric "x" and "y" coordinates`,
				i, element.Type)
		}
	}
	return nil
}
