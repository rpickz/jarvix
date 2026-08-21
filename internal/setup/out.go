package setup

import (
	"fmt"
	"io"
)

// fprintf and fprintln write wizard output, deliberately dropping the writer
// error: output goes to the user's terminal, where a failed write has no
// useful recovery.
func fprintf(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

func fprintln(w io.Writer, args ...any) { _, _ = fmt.Fprintln(w, args...) }
