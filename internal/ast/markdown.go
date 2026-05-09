package ast

import (
	"bytes"
	"path/filepath"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// markdownExtractor parses Markdown / CommonMark via github.com/yuin/goldmark
// — a pure-Go CommonMark implementation. Emits one Section symbol per
// heading with a hierarchical dotted-path qualified name and a byte range
// covering the section's full body (heading line through just before the
// next heading at same-or-shallower level).
//
// Confidence is 1.0 (real CommonMark parser, not regex). Routes to the
// docs corpus via ClassifyCorpus + the v9 trigger WHERE clauses.
//
// Registered for .md, .markdown, .mdx, .mdc:
//   - .mdx so MDX files at least surface their headings even if the
//     JSX-embedded blocks aren't extracted.
//   - .mdc for Cursor rule files (`.cursor/rules/*.mdc`) — same CommonMark
//     grammar with optional YAML frontmatter; goldmark ignores frontmatter
//     gracefully so the heading hierarchy still extracts cleanly.
type markdownExtractor struct{}

func (m *markdownExtractor) Languages() []string { return []string{"Markdown"} }
func (m *markdownExtractor) Extensions() map[string]string {
	return map[string]string{
		".md":       "Markdown",
		".markdown": "Markdown",
		".mdx":      "Markdown",
		".mdc":      "Markdown",
	}
}
func (m *markdownExtractor) Confidence() float64 { return 1.0 }

func (m *markdownExtractor) Extract(source []byte, _, relPath string, _ ExtractOptions) (result *FileResult) {
	result = &FileResult{Module: markdownModuleName(relPath)}
	if len(source) == 0 {
		return result
	}

	// Defensive recover: goldmark shouldn't panic on any input, but a
	// malformed file shouldn't take down the indexer goroutine. Partial
	// result is more useful than a crash.
	defer func() {
		if r := recover(); r != nil {
			if result == nil {
				result = &FileResult{Module: markdownModuleName(relPath)}
			}
		}
	}()

	md := goldmark.New()
	doc := md.Parser().Parse(text.NewReader(source))

	// Pass 1: collect all headings in document order with level + byte offsets.
	type headingInfo struct {
		level    int
		title    string
		startTxt int // byte offset of the heading text (after `# `)
	}
	var headings []headingInfo

	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		h, ok := n.(*ast.Heading)
		if !ok {
			return ast.WalkContinue, nil
		}
		lines := h.Lines()
		if lines == nil || lines.Len() == 0 {
			return ast.WalkContinue, nil
		}
		startTxt := lines.At(0).Start

		var buf bytes.Buffer
		for c := h.FirstChild(); c != nil; c = c.NextSibling() {
			if t, ok := c.(*ast.Text); ok {
				buf.Write(t.Segment.Value(source))
			}
		}
		title := strings.TrimSpace(buf.String())
		if title == "" {
			return ast.WalkContinue, nil
		}
		headings = append(headings, headingInfo{level: h.Level, title: title, startTxt: startTxt})
		return ast.WalkContinue, nil
	})

	if len(headings) == 0 {
		return result
	}

	// Pass 2: compute section ranges + hierarchical qualified names.
	//
	// Stack semantics: when emitting heading i, pop the stack until the
	// top has level < headings[i].level, then push (level, slug). The
	// stack contents are the dotted path for this heading's QN.
	type stackFrame struct {
		level int
		slug  string
	}
	var stack []stackFrame

	for i, h := range headings {
		// Snap section bounds back to the start of the line containing
		// the heading. lines.At(0).Start points at the first char AFTER
		// the `# ` prefix; we want to include the prefix in the symbol's
		// byte range so retrieval shows the heading source.
		startByte := lineStartAt(source, h.startTxt)

		// Section end = byte just before the next heading with level <=
		// current. If there's no such heading, end of document.
		endByte := len(source)
		for j := i + 1; j < len(headings); j++ {
			if headings[j].level <= h.level {
				endByte = lineStartAt(source, headings[j].startTxt)
				break
			}
		}
		// Trim a trailing newline from the end so adjacent sections don't
		// alias on the same byte. Optional but reads cleaner.
		for endByte > startByte && (source[endByte-1] == '\n' || source[endByte-1] == '\r') {
			endByte--
		}

		// Pop stack until we're at the parent level.
		for len(stack) > 0 && stack[len(stack)-1].level >= h.level {
			stack = stack[:len(stack)-1]
		}

		slug := markdownSlug(h.title)
		path := slug
		if len(stack) > 0 {
			parts := make([]string, 0, len(stack)+1)
			for _, f := range stack {
				parts = append(parts, f.slug)
			}
			parts = append(parts, slug)
			path = strings.Join(parts, ".")
		}

		// Push current heading for descendants.
		stack = append(stack, stackFrame{level: h.level, slug: slug})

		startLine := byteOffsetToLine(source, startByte)
		endLine := byteOffsetToLine(source, endByte)

		result.Symbols = append(result.Symbols, ExtractedSymbol{
			Name:          h.title,
			QualifiedName: path,
			Kind:          "Section",
			StartByte:     startByte,
			EndByte:       endByte,
			StartLine:     startLine,
			EndLine:       endLine,
			Signature:     strings.Repeat("#", h.level) + " " + h.title,
			IsExported:    true,
		})
	}

	// #115 disambiguation happens centrally in ExtractWithModule —
	// goldmark gives us tree-aware paths, but identical heading text in
	// different sections still collides (`installation_from_source.windows`
	// in docs/source.md was the canonical case).
	return result
}

// markdownModuleName turns "docs/intro.md" into "intro" — the file basename
// stripped of extension. Used as the QN root prefix is unnecessary because
// hierarchical headings already produce unique paths within a file. The
// Module field on FileResult still gets populated for parity with other
// extractors, but it's not part of the QN.
func markdownModuleName(relPath string) string {
	base := filepath.Base(relPath)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

// markdownSlug normalises a heading title into a dotted-path component.
// Lowercase, spaces → underscores, drop characters that would conflict
// with the QN separator (`.`).
//
// Two headings that slug to the same value will have identical QNs in
// the same parent — the DB layer dedupes via upsert-on-ID, so the
// later-occurring symbol wins. The byte ranges differ, which is enough
// for retrieval to round-trip correctly.
func markdownSlug(title string) string {
	var b strings.Builder
	b.Grow(len(title))
	prev := byte(0)
	for i := 0; i < len(title); i++ {
		c := title[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 32)
			prev = c + 32
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
			prev = c
		case c == ' ', c == '\t', c == '-', c == '_':
			if prev != '_' && b.Len() > 0 {
				b.WriteByte('_')
				prev = '_'
			}
		case c == '.':
			// `.` is the QN separator; collapse to `_` so a "v1.0" heading
			// doesn't fragment into ["v1", "0"] under a parent.
			if prev != '_' && b.Len() > 0 {
				b.WriteByte('_')
				prev = '_'
			}
		}
		// Other characters (punctuation, unicode, etc.) are dropped.
		// CommonMark allows almost anything in headings; we trade
		// fidelity for QN safety — the original title is preserved in
		// the symbol's Name field for display.
	}
	if b.Len() == 0 {
		// Pathological case: all-punctuation heading. Use a sentinel so
		// the extractor still emits a symbol rather than dropping it.
		return "_"
	}
	// Strip trailing underscore.
	out := b.String()
	for len(out) > 1 && out[len(out)-1] == '_' {
		out = out[:len(out)-1]
	}
	return out
}

// lineStartAt walks backward from `off` to the first byte after the
// previous newline (or the start of the buffer). Used to snap a heading
// byte offset (which goldmark gives us mid-line) to the start of its line.
func lineStartAt(source []byte, off int) int {
	if off < 0 {
		return 0
	}
	if off > len(source) {
		off = len(source)
	}
	for off > 0 && source[off-1] != '\n' {
		off--
	}
	return off
}

// byteOffsetToLine converts a byte offset to a 1-indexed line number by
// counting newlines up to off. O(n) — fine for our use (a few headings
// per file, n < 100KB typical).
func byteOffsetToLine(source []byte, off int) int {
	if off > len(source) {
		off = len(source)
	}
	line := 1
	for i := 0; i < off; i++ {
		if source[i] == '\n' {
			line++
		}
	}
	return line
}

func init() {
	Register(&markdownExtractor{})
}
