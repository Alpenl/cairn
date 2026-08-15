// Package notetitle derives the stable, user-visible title for Reader notes.
package notetitle

import (
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	goldmarktext "github.com/yuin/goldmark/text"
)

const Untitled = "未命名笔记"

// Derive applies the note title contract to GFM source. A first-level heading
// wins even when prose comes before it; otherwise the first textual block is
// used. Titles are normalized for stable search and display.
func Derive(markdown string) string {
	source := []byte(markdown)
	document := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser().Parse(goldmarktext.NewReader(source))

	var title string
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || title != "" {
			return ast.WalkContinue, nil
		}
		if heading, ok := node.(*ast.Heading); ok && heading.Level == 1 {
			title = normalize(inlineText(heading, source))
		}
		return ast.WalkContinue, nil
	})
	if title != "" {
		return title
	}

	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || title != "" || !isTextBlock(node) {
			return ast.WalkContinue, nil
		}
		title = normalize(inlineText(node, source))
		return ast.WalkContinue, nil
	})
	if title == "" {
		return Untitled
	}
	return title
}

func isTextBlock(node ast.Node) bool {
	switch node.(type) {
	case *ast.Heading, *ast.Paragraph, *ast.TextBlock, *ast.FencedCodeBlock, *ast.CodeBlock:
		return true
	default:
		return false
	}
}

func inlineText(node ast.Node, source []byte) string {
	var builder strings.Builder
	var visit func(ast.Node)
	visit = func(current ast.Node) {
		if current.FirstChild() == nil {
			writeLeafText(&builder, current, source)
			return
		}
		for child := current.FirstChild(); child != nil; child = child.NextSibling() {
			visit(child)
		}
	}
	visit(node)
	return builder.String()
}

func writeLeafText(builder *strings.Builder, node ast.Node, source []byte) {
	switch current := node.(type) {
	case *ast.Text:
		builder.Write(current.Value(source))
	case *ast.String:
		builder.Write(current.Value)
	case *ast.AutoLink:
		builder.Write(current.Label(source))
	case *ast.RawHTML:
		builder.Write(current.Segments.Value(source))
	default:
		if current.Type() == ast.TypeBlock {
			builder.Write(current.Lines().Value(source))
		}
	}
}

func normalize(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) > 60 {
		return string(runes[:60])
	}
	return value
}
