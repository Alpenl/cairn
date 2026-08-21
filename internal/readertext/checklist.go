package readertext

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Block is a source-addressable Markdown checklist item. Occurrence is
// counted among items with the same derived block reference, so repeated task
// text can be updated without relying on a line number.
type Block struct {
	BlockRef    string
	Text        string
	Done        bool
	Occurrence  int
	LineStart   int
	LineEnd     int
	MarkerStart int
}

type UpdateStatus string

const (
	Updated   UpdateStatus = "updated"
	NotFound  UpdateStatus = "not-found"
	Ambiguous UpdateStatus = "ambiguous"
)

type UpdateResult struct {
	Status UpdateStatus
	Source string
	Block  Block
}

var taskPattern = regexp.MustCompile(`^(\s*)([-*+]|[0-9]+[.)])\s+\[([ xX])\]\s+(.*?)\s*$`)
var fencePattern = regexp.MustCompile("^\\s{0,3}(`{3,}|~{3,})")

type sourceLine struct {
	value string
	start int
	end   int
}

func linesOf(source string) []sourceLine {
	lines := make([]sourceLine, 0, strings.Count(source, "\n")+1)
	start := 0
	for start < len(source) {
		newline := strings.IndexByte(source[start:], '\n')
		end := len(source)
		if newline >= 0 {
			end = start + newline + 1
		}
		raw := source[start:end]
		value := strings.TrimSuffix(raw, "\n")
		value = strings.TrimSuffix(value, "\r")
		lines = append(lines, sourceLine{value: value, start: start, end: end})
		start = end
	}
	if len(lines) == 0 {
		lines = append(lines, sourceLine{})
	}
	return lines
}

func nearestHeading(lines []sourceLine, index int) string {
	for cursor := index - 1; cursor >= 0; cursor-- {
		value := strings.TrimSpace(lines[cursor].value)
		if strings.HasPrefix(value, "#") {
			level := 0
			for level < len(value) && value[level] == '#' {
				level++
			}
			if level > 0 && level <= 6 && len(value) > level && value[level] == ' ' {
				return value
			}
		}
	}
	return ""
}

// BlockReference deliberately hashes UTF-16 JSON code units. This mirrors
// JavaScript's JSON.stringify and keeps server projections addressable by the
// Reader client without exposing source text in a persistent marker.
func blockReference(lines []sourceLine, index int, text string) string {
	heading := nearestHeading(lines, index)
	return blockReferenceForParts(strings.TrimSpace(text), heading)
}

func blockReferenceForParts(parts ...string) string {
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(parts)
	encoded := bytes.TrimSuffix(payload.Bytes(), []byte{'\n'})
	var hash uint32 = 2166136261
	for _, unit := range utf16.Encode([]rune(string(encoded))) {
		hash ^= uint32(unit)
		hash *= 16777619
	}
	return "task:" + strconv.FormatUint(uint64(hash), 16)
}

func List(source string) []Block {
	return parseBlocks(source)
}

func parseBlocks(source string) []Block {
	lines := linesOf(source)
	blocks := make([]Block, 0)
	occurrences := make(map[string]int)
	var fence *struct {
		marker byte
		length int
	}
	for index, line := range lines {
		if match := fencePattern.FindStringSubmatch(line.value); match != nil {
			marker := match[1][0]
			length := len(match[1])
			if fence == nil {
				fence = &struct {
					marker byte
					length int
				}{marker: marker, length: length}
			} else if fence.marker == marker && length >= fence.length {
				fence = nil
			}
			continue
		}
		if fence != nil {
			continue
		}
		match := taskPattern.FindStringSubmatch(line.value)
		if match == nil {
			continue
		}
		text := match[4]
		ref := blockReference(lines, index, text)
		occurrences[ref]++
		markerStart := len(match[1]) + len(match[2]) + 2
		blocks = append(blocks, Block{
			BlockRef:    ref,
			Text:        text,
			Done:        strings.EqualFold(match[3], "x"),
			Occurrence:  occurrences[ref],
			LineStart:   line.start,
			LineEnd:     line.end,
			MarkerStart: line.start + markerStart,
		})
	}
	return blocks
}

func Update(source, blockRef string, occurrence int, done bool) UpdateResult {
	if occurrence <= 0 {
		occurrence = 1
	}
	matches := matchingBlocks(parseBlocks(source), blockRef)
	if len(matches) == 0 {
		return UpdateResult{Status: NotFound}
	}
	selected, status := selectByOccurrence(matches, occurrence)
	if selected == nil {
		return UpdateResult{Status: status}
	}
	block := *selected
	next, ok := writeMarker(source, block.MarkerStart, done)
	if !ok {
		return UpdateResult{Status: NotFound}
	}
	block.Done = done
	return UpdateResult{Status: Updated, Source: next, Block: block}
}

func matchingBlocks(blocks []Block, blockRef string) []Block {
	matches := make([]Block, 0, 1)
	for _, block := range blocks {
		if block.BlockRef == blockRef {
			matches = append(matches, block)
		}
	}
	return matches
}

// selectByOccurrence picks the single match carrying the requested occurrence.
// A second hit is Ambiguous rather than a silent first-wins update, because two
// items sharing a reference and an occurrence are not distinguishable. The
// status is the caller's verdict only when no block is returned.
func selectByOccurrence(matches []Block, occurrence int) (selected *Block, status UpdateStatus) {
	for index := range matches {
		candidate := matches[index]
		if candidate.Occurrence != occurrence {
			continue
		}
		if selected != nil {
			return nil, Ambiguous
		}
		selected = &candidate
	}
	if selected == nil {
		return nil, NotFound
	}
	return selected, Updated
}

// writeMarker rewrites the single checkbox byte at markerStart. It re-reads
// that byte from the current source first: a stale offset must fail the update
// instead of corrupting unrelated text.
func writeMarker(source string, markerStart int, done bool) (string, bool) {
	if markerStart < 0 || markerStart >= len(source) {
		return "", false
	}
	marker := source[markerStart]
	if marker != ' ' && marker != 'x' && marker != 'X' {
		return "", false
	}
	next := source[:markerStart]
	if done {
		next += "x"
	} else {
		next += " "
	}
	next += source[markerStart+1:]
	return next, true
}
