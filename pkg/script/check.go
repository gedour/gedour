package script

import (
	"errors"
	"fmt"

	"github.com/dop251/goja"
)

// SyntaxError is a script that does not parse, with its 1-based position when known.
type SyntaxError struct {
	Message string
	Line    int
	Column  int
}

func (e *SyntaxError) Error() string {
	if e.Line == 0 {
		return e.Message
	}
	return fmt.Sprintf("%s (line %d, column %d)", e.Message, e.Line, e.Column)
}

// Check parses src without running it; a nil error means it compiles. Parsing is linear in the
// (64 KiB at most) source, so it stays in-process.
func Check(src string) error {
	if len(src) > MaxSource {
		return &SyntaxError{Message: fmt.Sprintf("at most %d KiB", MaxSource>>10)}
	}
	_, err := goja.Compile("script.js", src, false)
	if err == nil {
		return nil
	}
	se := &SyntaxError{Message: err.Error()}
	var ce *goja.CompilerSyntaxError
	if errors.As(err, &ce) {
		se.Message = ce.Message
		if ce.File != nil {
			p := ce.File.Position(ce.Offset)
			se.Line, se.Column = p.Line, p.Column
		}
	}
	return se
}
