package engine

import (
	"strings"
	"testing"
)

func TestValidateDatabaseName_Accepts(t *testing.T) {
	cases := []string{
		"prod",
		"sensors_2026",
		"prod-east",
		"app.v2",
		"a",
		"a1",
		"_internal", // leading underscore allowed (used by stats DB pattern)
		"internal",
		strings.Repeat("a", 64), // exactly the cap
	}
	for _, name := range cases {
		if err := ValidateDatabaseName(name); err != nil {
			t.Errorf("ValidateDatabaseName(%q) unexpected error: %v", name, err)
		}
	}
}

func TestValidateDatabaseName_Rejects(t *testing.T) {
	cases := []struct {
		name string
		why  string
	}{
		{"", "empty"},
		{".", "single dot"},
		{"..", "double dot (traversal)"},
		{".hidden", "leading dot"},
		{"-flag", "leading dash"},
		{"prod/east", "slash"},
		{"prod\\east", "backslash"},
		{"prod east", "space"},
		{"prod\x00", "null byte"},
		{"prod\n", "newline"},
		{"prod\t", "tab"},
		{"../etc", "traversal"},
		{"./prod", "relative"},
		{"prod*", "glob"},
		{"prod;rm", "semicolon"},
		{"prod$", "shell metachar"},
		{"prod@host", "at-sign"},
		{"unicode-ñ", "non-ASCII"},
		{strings.Repeat("a", 65), "too long"},
	}
	for _, tc := range cases {
		if err := ValidateDatabaseName(tc.name); err == nil {
			t.Errorf("ValidateDatabaseName(%q) expected error (%s) but got nil", tc.name, tc.why)
		}
	}
}
