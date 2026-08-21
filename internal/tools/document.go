package tools

// DocumentRenderer is format "document": Markdown the model drafts, saved
// verbatim as a .md file and opened in the user's editor. It is a
// passthrough — there is nothing to render, and nothing to validate either:
// Markdown has no failure mode short of not being text, and rewriting it
// (even "normalising") would betray the point of a draft the user keeps
// editing. Front matter, spacing, everything lands byte-for-byte as the
// model wrote it.
type DocumentRenderer struct{ passthrough }

// Format implements Renderer.
func (*DocumentRenderer) Format() string { return "document" }

// SourceExt implements Renderer.
func (*DocumentRenderer) SourceExt() string { return ".md" }

// OutputExt implements Renderer. Same as SourceExt: the saved Markdown is
// the artifact.
func (*DocumentRenderer) OutputExt() string { return ".md" }
