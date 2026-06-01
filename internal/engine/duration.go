package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// extendedDurationToken matches "<number><d|w>" tokens (with optional fractional
// part) anywhere inside a duration string. The unit-letter loop in
// time.ParseDuration only accepts ns/us/µs/μs/ms/s/m/h, so day and week tokens
// must be rewritten before delegation.
var extendedDurationToken = regexp.MustCompile(`(\d+(?:\.\d+)?)([dw])`)

// ParseDuration is a superset of time.ParseDuration that also accepts:
//   - `d` (days, 24h)
//   - `w` (weeks, 7*24h)
//
// Fractional values are supported (`1.5d`). Mixed compositions are allowed
// (`1w2d3h`). All other behaviour matches time.ParseDuration exactly —
// leading sign, ns/us/µs/μs/ms/s/m/h units, error shape.
//
// This is the canonical duration parser used everywhere nanotdb accepts a
// user-supplied duration string: manifest fields (`grace`, `page.max_age`,
// rollup intervals, …), the webapi `window` parameter, and the nanocli
// `--start`/`--end` relative-duration shorthand.
func ParseDuration(s string) (time.Duration, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return 0, fmt.Errorf("missing duration")
	}
	// Rewrite Nd / Nw tokens into their hour-equivalent (Nh) form, then let
	// time.ParseDuration do the heavy lifting. Numbers carry sign by being
	// preceded by an optional `+`/`-` outside the token — Go's parser
	// handles that already.
	var rewriteErr error
	rewritten := extendedDurationToken.ReplaceAllStringFunc(v, func(tok string) string {
		match := extendedDurationToken.FindStringSubmatch(tok)
		amount, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			rewriteErr = err
			return tok
		}
		multiplier := 24.0
		if match[2] == "w" {
			multiplier = 7 * 24
		}
		// Emit as nanoseconds to preserve fractional precision losslessly
		// through time.ParseDuration's integer-ns code path.
		ns := int64(amount * multiplier * float64(time.Hour))
		return strconv.FormatInt(ns, 10) + "ns"
	})
	if rewriteErr != nil {
		return 0, rewriteErr
	}
	return time.ParseDuration(rewritten)
}
