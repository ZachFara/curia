package main

import (
	"bytes"
	"encoding/json"
	"io"
)

// writeJSON is the one way a command prints JSON: encoded without HTML
// escaping ("&&" in a command stays readable), indented by indent, and with
// every unprintable rune the encoder leaves raw written as an escape
// (escapeUnprintable), so no value reaches a terminal as a control sequence.
// TestEveryJSONPrinterUsesWriteJSON fails for a command that encodes by itself.
func writeJSON(w io.Writer, v any, indent string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := io.WriteString(w, escapeUnprintable(buf.String()))
	return err
}
