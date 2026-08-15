package service

import (
	"strings"
)

// SiteEmbeddingDocument is deliberately profile-only. It has no Link body,
// summary, captured HTML, or user note field, making those values impossible
// to accidentally include in a site vector input.
type SiteEmbeddingDocument struct {
	Name        string
	Intro       string
	DisplayHost string
	Tags        []string
	Entries     []SiteEmbeddingEntry
}

type SiteEmbeddingEntry struct {
	Name    string
	Purpose string
}

// BuildSiteEmbeddingInput creates a bounded, field-labelled representation.
// Labels preserve the different roles of names/tags/purposes for the model;
// duplicate and blank values are removed to avoid vector skew from retries.
func BuildSiteEmbeddingInput(document SiteEmbeddingDocument) string {
	parts := make([]string, 0, 3+len(document.Tags)+len(document.Entries)*2)
	appendField := func(label, value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			parts = append(parts, label+": "+value)
		}
	}
	appendField("site", document.Name)
	appendField("intro", document.Intro)
	appendField("host", document.DisplayHost)
	seenTags := map[string]struct{}{}
	for _, tag := range document.Tags {
		normalized := strings.ToLower(strings.TrimSpace(tag))
		if normalized == "" {
			continue
		}
		if _, exists := seenTags[normalized]; exists {
			continue
		}
		seenTags[normalized] = struct{}{}
		appendField("tag", tag)
	}
	for _, entry := range document.Entries {
		appendField("entry", entry.Name)
		appendField("purpose", entry.Purpose)
	}
	return strings.Join(parts, "\n")
}
