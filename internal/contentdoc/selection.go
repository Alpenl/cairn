package contentdoc

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	goldmarkast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	goldmarkextensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer"
	goldmarktext "github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"webtag/internal/model"
)

// ErrSelectionMismatch means that a client selection is not an exact range
// in the canonical rendered block projection.
var ErrSelectionMismatch = errors.New("selection does not match rendered block")

// RenderedBlockProjection returns the text-node sequence used by Reader's
// block-local selection offsets. Plain blocks render their source verbatim.
// Markdown blocks include text nodes introduced by the Markdown-to-HAST
// conversion, such as the newlines around lists and fenced code blocks.
func RenderedBlockProjection(format model.ContentFormat, source string) (string, error) {
	switch format {
	case model.ContentFormatPlain:
		return source, nil
	case model.ContentFormatMarkdown:
		return renderedMarkdownProjection(source)
	default:
		return "", fmt.Errorf("rendered block projection: unsupported content format %q", format)
	}
}

// ValidateRenderedSelection derives the rendered projection from a canonical
// source and verifies the selected text in JavaScript's UTF-16 coordinate
// system. Callers should pass the source read under their authoritative lock;
// accepting a precomputed projection here would make stale validation easy.
func ValidateRenderedSelection(
	format model.ContentFormat,
	canonicalSource string,
	startOffset, endOffset int,
	selectedText string,
) error {
	projection, err := RenderedBlockProjection(format, canonicalSource)
	if err != nil {
		return err
	}
	return validateUTF16Selection(projection, startOffset, endOffset, selectedText)
}

func validateUTF16Selection(projection string, startOffset, endOffset int, selectedText string) error {
	if !utf8.ValidString(projection) || !utf8.ValidString(selectedText) {
		return fmt.Errorf("%w: invalid UTF-8", ErrSelectionMismatch)
	}
	units := utf16.Encode([]rune(projection))
	if startOffset < 0 || endOffset <= startOffset || endOffset > len(units) {
		return fmt.Errorf("%w: offsets outside projection", ErrSelectionMismatch)
	}
	if !utf16Boundary(units, startOffset) || !utf16Boundary(units, endOffset) {
		return fmt.Errorf("%w: offset splits surrogate pair", ErrSelectionMismatch)
	}
	if string(utf16.Decode(units[startOffset:endOffset])) != selectedText {
		return fmt.Errorf("%w: selected text differs", ErrSelectionMismatch)
	}
	return nil
}

func renderedMarkdownProjection(source string) (string, error) {
	var rendered bytes.Buffer
	markdown := goldmark.New(
		goldmark.WithExtensions(extension.GFM, extension.Footnote),
		goldmark.WithRendererOptions(renderer.WithNodeRenderers(
			util.Prioritized(markdownRawTextRenderer{}, 500),
			util.Prioritized(markdownFootnoteTextRenderer{}, 400),
		)),
	)
	strictSource, err := strictGFMTableSource(markdown, source)
	if err != nil {
		return "", fmt.Errorf("normalize markdown table projection: %w", err)
	}
	if err := markdown.Convert([]byte(strictSource), &rendered); err != nil {
		return "", fmt.Errorf("render markdown block projection: %w", err)
	}

	context := &html.Node{Type: html.ElementNode, DataAtom: atom.Div, Data: "div"}
	nodes, err := html.ParseFragment(strings.NewReader(rendered.String()), context)
	if err != nil {
		return "", fmt.Errorf("parse rendered markdown block projection: %w", err)
	}
	var projection strings.Builder
	for _, node := range nodes {
		appendTextNodes(&projection, node)
	}
	// Goldmark serializes every terminal block with one formatting newline.
	// React's HAST root only inserts newlines between root children, so remove
	// that serializer-only suffix while preserving a code block's own newline.
	return strings.TrimSuffix(projection.String(), "\n"), nil
}

// Goldmark pads a short header row to the delimiter width, while remark-gfm
// leaves that malformed table as paragraph text. Find only tables Goldmark
// actually parsed, then escape their delimiter's first pipe and reparse. The
// Markdown escape is not visible in rendered text, and AST inspection avoids
// rewriting delimiter-looking text inside code fences or raw HTML.
func strictGFMTableSource(markdown goldmark.Markdown, source string) (string, error) {
	sourceBytes := []byte(source)
	document := markdown.Parser().Parse(goldmarktext.NewReader(sourceBytes))
	var escapeOffsets []int
	err := goldmarkast.Walk(document, func(node goldmarkast.Node, entering bool) (goldmarkast.WalkStatus, error) {
		if !entering || node.Kind() != goldmarkextensionast.KindTable {
			return goldmarkast.WalkContinue, nil
		}
		table := node.(*goldmarkextensionast.Table)
		header := table.FirstChild()
		if header == nil || header.Kind() != goldmarkextensionast.KindTableHeader {
			return goldmarkast.WalkStop, errors.New("parsed table has no header")
		}
		explicitHeaderCells := 0
		for cell := header.FirstChild(); cell != nil; cell = cell.NextSibling() {
			if cell.Lines().Len() > 0 {
				explicitHeaderCells++
			}
		}
		if explicitHeaderCells == len(table.Alignments) {
			return goldmarkast.WalkContinue, nil
		}

		delimiterStart := nextLineStart(sourceBytes, header.Pos())
		if delimiterStart < 0 {
			return goldmarkast.WalkStop, errors.New("parsed table has no delimiter line")
		}
		delimiterEnd := bytes.IndexByte(sourceBytes[delimiterStart:], '\n')
		if delimiterEnd < 0 {
			delimiterEnd = len(sourceBytes)
		} else {
			delimiterEnd += delimiterStart
		}
		pipeOffset := firstUnescapedPipe(sourceBytes[delimiterStart:delimiterEnd])
		if pipeOffset < 0 {
			return goldmarkast.WalkStop, errors.New("malformed table delimiter has no pipe to escape")
		}
		escapeOffsets = append(escapeOffsets, delimiterStart+pipeOffset)
		return goldmarkast.WalkContinue, nil
	})
	if err != nil || len(escapeOffsets) == 0 {
		return source, err
	}

	sort.Ints(escapeOffsets)
	var strict strings.Builder
	strict.Grow(len(source) + len(escapeOffsets))
	previous := 0
	for _, offset := range escapeOffsets {
		strict.WriteString(source[previous:offset])
		strict.WriteByte('\\')
		previous = offset
	}
	strict.WriteString(source[previous:])
	return strict.String(), nil
}

func nextLineStart(source []byte, offset int) int {
	if offset < 0 || offset >= len(source) {
		return -1
	}
	newline := bytes.IndexByte(source[offset:], '\n')
	if newline < 0 || offset+newline+1 >= len(source) {
		return -1
	}
	return offset + newline + 1
}

func firstUnescapedPipe(line []byte) int {
	for i, value := range line {
		if value != '|' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && line[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return i
		}
	}
	return -1
}

// react-markdown turns raw HAST nodes back into text nodes unless skipHtml is
// enabled. Goldmark's safe renderer emits invisible comments instead, so these
// two node overrides escape the original markup into visible HTML text before
// the common text-node extraction runs.
type markdownRawTextRenderer struct{}

func (markdownRawTextRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(goldmarkast.KindRawHTML, renderInlineRawMarkdownText)
	reg.Register(goldmarkast.KindHTMLBlock, renderBlockRawMarkdownText)
}

// react-markdown exposes the generated footnote label and uses ordinary
// spaces plus ↩/↩N backlink text. Goldmark's default renderer omits the label
// and uses NBSP plus a variation selector, so override only those visible
// nodes while leaving its footnote parsing, numbering, and list items intact.
type markdownFootnoteTextRenderer struct{}

func (markdownFootnoteTextRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(goldmarkextensionast.KindFootnoteBacklink, renderFootnoteBacklinkText)
	reg.Register(goldmarkextensionast.KindFootnoteList, renderFootnoteListText)
}

func renderFootnoteBacklinkText(
	w util.BufWriter,
	_ []byte,
	node goldmarkast.Node,
	entering bool,
) (goldmarkast.WalkStatus, error) {
	if !entering {
		return goldmarkast.WalkContinue, nil
	}
	backlink := node.(*goldmarkextensionast.FootnoteBacklink)
	_, _ = w.WriteString(" <a>↩")
	if backlink.RefIndex > 0 {
		_, _ = w.WriteString("<sup>")
		_, _ = w.WriteString(strconv.Itoa(backlink.RefIndex + 1))
		_, _ = w.WriteString("</sup>")
	}
	_, _ = w.WriteString("</a>")
	return goldmarkast.WalkContinue, nil
}

func renderFootnoteListText(
	w util.BufWriter,
	_ []byte,
	_ goldmarkast.Node,
	entering bool,
) (goldmarkast.WalkStatus, error) {
	if entering {
		_, _ = w.WriteString("<section><h2>Footnotes</h2>\n<ol>\n")
	} else {
		_, _ = w.WriteString("</ol>\n</section>\n")
	}
	return goldmarkast.WalkContinue, nil
}

func renderInlineRawMarkdownText(
	w util.BufWriter,
	source []byte,
	node goldmarkast.Node,
	entering bool,
) (goldmarkast.WalkStatus, error) {
	if !entering {
		return goldmarkast.WalkSkipChildren, nil
	}
	raw := node.(*goldmarkast.RawHTML)
	_, _ = w.Write(util.EscapeHTML(raw.Segments.Value(source)))
	return goldmarkast.WalkSkipChildren, nil
}

func renderBlockRawMarkdownText(
	w util.BufWriter,
	source []byte,
	node goldmarkast.Node,
	entering bool,
) (goldmarkast.WalkStatus, error) {
	if !entering {
		return goldmarkast.WalkSkipChildren, nil
	}
	raw := node.(*goldmarkast.HTMLBlock)
	value := append([]byte(nil), raw.Lines().Value(source)...)
	if raw.HasClosure() {
		value = append(value, raw.ClosureLine.Value(source)...)
	}
	value = trimOneLineEnding(value)
	_, _ = w.Write(util.EscapeHTML(value))
	_ = w.WriteByte('\n')
	return goldmarkast.WalkSkipChildren, nil
}

func trimOneLineEnding(value []byte) []byte {
	if bytes.HasSuffix(value, []byte("\r\n")) {
		return value[:len(value)-2]
	}
	if bytes.HasSuffix(value, []byte("\n")) || bytes.HasSuffix(value, []byte("\r")) {
		return value[:len(value)-1]
	}
	return value
}

func appendTextNodes(out *strings.Builder, node *html.Node) {
	if node.Type == html.TextNode {
		if isTableRendererWhitespace(node) {
			return
		}
		out.WriteString(node.Data)
		return
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		appendTextNodes(out, child)
	}
}

func isTableRendererWhitespace(node *html.Node) bool {
	if node.Parent == nil {
		return false
	}
	switch node.Parent.DataAtom {
	case atom.Table, atom.Thead, atom.Tbody, atom.Tfoot, atom.Tr:
	default:
		return false
	}
	if node.Data == "" {
		return false
	}
	for _, r := range node.Data {
		switch r {
		case ' ', '\t', '\n', '\r':
		default:
			return false
		}
	}
	return true
}

func utf16Boundary(units []uint16, offset int) bool {
	if offset <= 0 || offset >= len(units) {
		return true
	}
	return units[offset-1] < 0xd800 || units[offset-1] > 0xdbff ||
		units[offset] < 0xdc00 || units[offset] > 0xdfff
}
