package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const tablePathMaxWidth = 48

func printAlignedTable(w io.Writer, headers []string, rows [][]string, rightAligned map[int]bool) {
	if len(headers) == 0 {
		return
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i := 0; i < len(headers) && i < len(row); i++ {
			if l := len(row[i]); l > widths[i] {
				widths[i] = l
			}
		}
	}

	printRow := func(cols []string) {
		for i := 0; i < len(headers); i++ {
			val := ""
			if i < len(cols) {
				val = cols[i]
			}
			if rightAligned != nil && rightAligned[i] {
				fmt.Fprintf(w, "%*s", widths[i], val)
			} else {
				fmt.Fprintf(w, "%-*s", widths[i], val)
			}
			if i < len(headers)-1 {
				fmt.Fprint(w, "  ")
			}
		}
		fmt.Fprintln(w)
	}

	printRow(headers)
	printRow(makeHeaderUnderline(headers, widths))
	for _, row := range rows {
		printRow(row)
	}
}

func makeHeaderUnderline(headers []string, widths []int) []string {
	out := make([]string, len(headers))
	for i := range headers {
		out[i] = strings.Repeat("-", widths[i])
	}
	return out
}

func formatDurationNS(startNS, endNS int64) string {
	d := endNS - startNS
	if d < 0 {
		d = 0
	}
	return time.Duration(d).String()
}

func shortenTablePath(baseDir, fullPath string) string {
	baseDir = strings.TrimSpace(baseDir)
	if baseDir != "" {
		if rel, err := filepath.Rel(baseDir, fullPath); err == nil && rel != "" && rel != "." && !strings.HasPrefix(rel, "..") {
			fullPath = rel
		}
	}
	return ellipsizeLeft(fullPath, tablePathMaxWidth)
}

func ellipsizeLeft(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return "..." + s[len(s)-(max-3):]
}
