package main

import (
	"encoding/json"
	"io"
)

type outputWriter struct {
	w      io.Writer
	asJSON bool
}

func (o outputWriter) emit(report any, human func(io.Writer)) error {
	if o.asJSON {
		enc := json.NewEncoder(o.w)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	human(o.w)
	return nil
}
