package ast

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// htmlExtractor parses HTML via golang.org/x/net/html — a pure-Go,
// canonical HTML5 parser maintained by the Go team. Emits one Section
// symbol per heading (h1–h6) with a hierarchical dotted-path qualified
// name, matching the Markdown extractor's pattern (#100).
//
// Confidence is 1.0 (real parser, not regex). Routes to the docs
// corpus via ClassifyCorpus + the v9 trigger WHERE clauses (HTML and
// Markdown both classify as docs).
//
// Coverage decisions:
//
//   - h1–h6 → Section symbols with hierarchical QN, byte range covers
//     the heading element's start tag through just before the next
//     heading at same-or-shallower level. Mirrors Markdown.
//   - <title> → Section symbol with QN = "title". Useful for SPA-style
//     single-page docs that have one canonical title and no h1.
//   - <script src="…">, <link href="…">, <a href="…"> with local
//     paths → IMPORTS edges. External URLs (http / https / //) and
//     anchor-only fragments (`#foo`) are skipped — they don't point at
//     a sibling source file in the index.
//   - Element id="…" attributes are NOT extracted as Setting symbols
//     (the design-question 1 in #100). Modern frameworks generate
//     IDs aggressively and the noise dilutes the symbol space.
//
// Templated HTML (Jinja2-pre-render, Go-template, ERB, etc.) often
// isn't valid HTML before rendering. golang.org/x/net/html is
// permissive — it tolerates malformed input gracefully and returns a
// partial tree. We extract whatever the parser yields rather than
// gating on validation.
//
// Closes #100.
type htmlExtractor struct{}

func (h *htmlExtractor) Languages() []string { return []string{"HTML"} }
func (h *htmlExtractor) Extensions() map[string]string {
	return map[string]string{
		".html": "HTML",
		".htm":  "HTML",
	}
}
func (h *htmlExtractor) Confidence() float64 { return 1.0 }

func (h *htmlExtractor) Extract(source []byte, _, relPath string, _ ExtractOptions) (result *FileResult) {
	result = &FileResult{Module: htmlModuleName(relPath)}
	if len(source) == 0 {
		return result
	}

	// Defensive recover: x/net/html is robust but a malformed file
	// shouldn't take down the indexer goroutine. Partial result beats
	// a crash.
	defer func() {
		if r := recover(); r != nil {
			if result == nil {
				result = &FileResult{Module: htmlModuleName(relPath)}
			}
		}
	}()

	doc, err := html.Parse(bytes.NewReader(source))
	if err != nil {
		// html.Parse rarely returns an error (it tolerates broken
		// input), but on the off chance it does we emit nothing.
		return result
	}

	// Collect headings (h1–h6), <title>, and IMPORTS-eligible refs in
	// document order. We walk once and accumulate everything; the
	// post-pass turns headings into Section symbols with hierarchy.
	type headingInfo struct {
		level int
		title string
		// startByte/endByte cover the whole element by inclusive line.
		// We can't get exact byte offsets from x/net/html (it doesn't
		// preserve them on the node), so we approximate by re-finding
		// the heading text in the source. Good enough — Section
		// retrieval lands on the right neighborhood.
		startByte int
	}
	var headings []headingInfo
	var titleText string
	var titleStartByte int
	var imports []string
	seenImports := make(map[string]bool)

	addImport := func(href string) {
		ref := canonicalImportPath(href)
		if ref == "" || seenImports[ref] {
			return
		}
		seenImports[ref] = true
		imports = append(imports, ref)
	}

	// searchFrom advances past each successfully-located heading so
	// duplicate (level, title) pairs in the document don't all resolve
	// to the same first-occurrence offset. Pre-fix, two h2 "Phase 2:
	// Configuration" sections both got startByte=N (the first), and the
	// section-emit loop produced end_byte=start_byte=N for the first
	// (since the second heading's startByte == its own startByte) —
	// recordExtractionHeuristics caught these as byte_range_negative
	// failures on real HTML docs (atrium/website, hermes/docs).
	searchFrom := 0
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.DataAtom {
			case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
				level := int(n.Data[1] - '0') // "h3" → 3
				title := strings.TrimSpace(htmlInnerText(n))
				if title != "" {
					off := bytesFindHeadingAfter(source, level, title, searchFrom)
					if off >= searchFrom {
						searchFrom = off + 1
					}
					headings = append(headings, headingInfo{
						level: level, title: title, startByte: off,
					})
				}
			case atom.Title:
				if titleText == "" {
					titleText = strings.TrimSpace(htmlInnerText(n))
					titleStartByte = bytesFindElement(source, "title")
				}
			case atom.Script:
				if src := htmlAttr(n, "src"); src != "" {
					addImport(src)
				}
			case atom.Link:
				if href := htmlAttr(n, "href"); href != "" {
					addImport(href)
				}
			case atom.A:
				if href := htmlAttr(n, "href"); href != "" {
					addImport(href)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	// Title symbol (when present) goes first so it appears at the top
	// of the file's symbol list.
	if titleText != "" {
		startLine := byteOffsetToLine(source, titleStartByte)
		endByte := titleStartByte + len(titleText) // approximate
		if endByte > len(source) {
			endByte = len(source)
		}
		result.Symbols = append(result.Symbols, ExtractedSymbol{
			Name:          titleText,
			QualifiedName: "title",
			Kind:          "Section",
			StartByte:     titleStartByte,
			EndByte:       endByte,
			StartLine:     startLine,
			EndLine:       startLine,
			Signature:     "<title>" + titleText + "</title>",
			IsExported:    true,
		})
	}

	// Heading hierarchy via stack — same algorithm as Markdown.
	type stackFrame struct {
		level int
		slug  string
	}
	var stack []stackFrame

	for i, h := range headings {
		startByte := h.startByte
		endByte := len(source)
		for j := i + 1; j < len(headings); j++ {
			if headings[j].level <= h.level {
				endByte = headings[j].startByte
				break
			}
		}
		// Trim trailing newlines to keep adjacent sections from
		// aliasing on the same byte.
		for endByte > startByte && (source[endByte-1] == '\n' || source[endByte-1] == '\r') {
			endByte--
		}

		for len(stack) > 0 && stack[len(stack)-1].level >= h.level {
			stack = stack[:len(stack)-1]
		}

		slug := markdownSlug(h.title) // reuse the markdown slug — same QN-safety contract
		path := slug
		if len(stack) > 0 {
			parts := make([]string, 0, len(stack)+1)
			for _, f := range stack {
				parts = append(parts, f.slug)
			}
			parts = append(parts, slug)
			path = strings.Join(parts, ".")
		}

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
			Signature:     "<h" + string(rune('0'+h.level)) + ">" + h.title + "</h" + string(rune('0'+h.level)) + ">",
			IsExported:    true,
		})
	}

	// Emit IMPORTS edges. The indexer's edge-resolution pass will look
	// up the target file's symbols by path; we just emit a name-shaped
	// reference here per the convention used by jinja / yaml extractors.
	// FromQN is left blank — the file itself is the importer; the
	// indexer attaches IMPORTS edges to the file scope when FromQN
	// is empty (matches the jinja extractor's pattern).
	for _, ref := range imports {
		result.Edges = append(result.Edges, ExtractedEdge{
			Kind:       "IMPORTS",
			ToName:     ref,
			Confidence: 1.0,
		})
	}

	return result
}

// htmlInnerText returns the concatenated text content of an element,
// trimming leading/trailing whitespace per child.
func htmlInnerText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	// Collapse runs of whitespace to single spaces. Browsers render
	// "<h1>Hello\n    World</h1>" as "Hello World"; we want our QN
	// slug pipeline to see the same thing.
	out := b.String()
	var sb strings.Builder
	sb.Grow(len(out))
	prevSpace := false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if !prevSpace && sb.Len() > 0 {
				sb.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		sb.WriteByte(c)
		prevSpace = false
	}
	return strings.TrimSpace(sb.String())
}

// htmlAttr returns the value of a named attribute, or "".
func htmlAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// canonicalImportPath filters an href / src for IMPORTS-edge eligibility.
// External URLs (`http://`, `https://`, `//cdn.example/...`), anchor
// fragments (`#`), and data: / mailto: schemes are skipped — they
// don't reference a sibling file in the index. Relative paths and
// project-rooted paths are returned as-is for the indexer's
// edge-resolution pass to resolve.
func canonicalImportPath(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "#") {
		return "" // anchor-only
	}
	if strings.HasPrefix(href, "//") {
		return "" // protocol-relative, treated as external
	}
	if i := strings.Index(href, "://"); i > 0 {
		// has a scheme — http, https, ftp, data, etc.
		return ""
	}
	if strings.HasPrefix(href, "mailto:") || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "tel:") {
		return ""
	}
	// Strip a fragment — `foo.html#section` → `foo.html`.
	if i := strings.IndexByte(href, '#'); i >= 0 {
		href = href[:i]
	}
	// Strip a query string — `foo.html?v=1` → `foo.html`.
	if i := strings.IndexByte(href, '?'); i >= 0 {
		href = href[:i]
	}
	if href == "" {
		return ""
	}
	return href
}

// bytesFindHeading searches `source` for a heading-tag opening that
// likely contains `title`. Best-effort byte-offset recovery since
// x/net/html doesn't preserve them on nodes.
//
// We look for "<h{level}" (case-insensitive) followed within ~1KB by
// the title text. Returns 0 on no match (will land at the file start,
// which is acceptable — Section retrieval still works, just less
// precise byte ranges for malformed corner cases).
func bytesFindHeading(source []byte, level int, title string) int {
	return bytesFindHeadingAfter(source, level, title, 0)
}

// bytesFindHeadingAfter starts the search at offset `start`. Used by the
// HTML walker to advance past previously-located headings so duplicate
// (level, title) pairs in the document don't all resolve to the same
// first-occurrence offset.
func bytesFindHeadingAfter(source []byte, level int, title string, start int) int {
	if start < 0 {
		start = 0
	}
	if start >= len(source) {
		return 0
	}
	tagPrefix := []byte("<h" + string(rune('0'+level)))
	titleBytes := []byte(title)
	off := start
	for {
		idx := bytesIndexFold(source[off:], tagPrefix)
		if idx < 0 {
			return 0
		}
		tagStart := off + idx
		// Look for the title within a reasonable window of the tag
		// open. Cap at 4KB so we don't accidentally match a heading
		// far below.
		windowEnd := tagStart + 4096
		if windowEnd > len(source) {
			windowEnd = len(source)
		}
		if bytesIndexFold(source[tagStart:windowEnd], titleBytes) >= 0 {
			return tagStart
		}
		off = tagStart + len(tagPrefix)
		if off >= len(source) {
			return 0
		}
	}
}

// bytesFindElement searches for the opening tag `<{name}` (case-insensitive).
// Returns the byte offset of the `<`, or 0 if not found.
func bytesFindElement(source []byte, name string) int {
	needle := []byte("<" + name)
	idx := bytesIndexFold(source, needle)
	if idx < 0 {
		return 0
	}
	return idx
}

// bytesIndexFold is a case-insensitive bytes.Index. Returns -1 on no
// match. Implemented manually so we don't pull in regexp or strings
// for ASCII-only HTML tag matching.
func bytesIndexFold(haystack, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	if len(needle) > len(haystack) {
		return -1
	}
	last := len(haystack) - len(needle)
	for i := 0; i <= last; i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			c1 := haystack[i+j]
			c2 := needle[j]
			if c1 >= 'A' && c1 <= 'Z' {
				c1 += 32
			}
			if c2 >= 'A' && c2 <= 'Z' {
				c2 += 32
			}
			if c1 != c2 {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// htmlModuleName mirrors markdownModuleName: turns "site/index.html"
// into "index" — the file basename stripped of extension. Currently
// unused for QN construction (heading hierarchy is enough), but the
// FileResult.Module field stays populated for parity with the other
// extractors.
func htmlModuleName(relPath string) string {
	if relPath == "" {
		return ""
	}
	base := relPath
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '/' || base[i] == '\\' {
			base = base[i+1:]
			break
		}
	}
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	return base
}

func init() {
	Register(&htmlExtractor{})
}
