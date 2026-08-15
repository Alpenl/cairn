// Package textutil offers small rune-aware string helpers shared between
// fetch and analyzer code paths. Both paths used to ship near-identical
// truncation/counting helpers; this package consolidates them so that
// changes to truncation semantics happen in one place.
package textutil

import (
	"strings"
	"unicode/utf8"
)

// Count returns the number of runes in s. It is a thin wrapper around
// utf8.RuneCountInString kept here so callers can reach for a single
// helper module rather than importing unicode/utf8 each time.
func Count(s string) int {
	return utf8.RuneCountInString(s)
}

// Truncate returns the longest prefix of s containing at most limit runes.
// A non-positive limit yields the empty string. When s already fits within
// limit it is returned unchanged so callers don't pay an alloc on the fast
// path.
func Truncate(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if Count(s) <= limit {
		return s
	}
	count := 0
	for idx := range s {
		if count == limit {
			return s[:idx]
		}
		count++
	}
	return s
}

// LimitTextByRunes trims surrounding whitespace and then applies Truncate.
// Empty/whitespace-only input becomes the empty string.
func LimitTextByRunes(s string, limit int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return Truncate(s, limit)
}
