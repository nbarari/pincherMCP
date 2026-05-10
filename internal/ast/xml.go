package ast

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// xmlExtractor parses XML files via stdlib `encoding/xml` and emits one
// Setting symbol per element plus one Setting per attribute. Routes to
// the config corpus.
//
// Symbol model (Option A, generic walker per #101 direction):
//
//	<config>                        →  config              Setting
//	  <database>                    →  config.database     Setting
//	    <host>localhost</host>      →  config.database.host       Setting
//	    <port>5432</port>           →  config.database.port       Setting
//	  </database>
//	  <resource id="db1" type="…">  →  config.resource     Setting
//	    <connection url="…"/>       →  config.resource.connection         Setting
//	                                   config.resource.connection@url     Setting
//	                                   config.resource@id                 Setting
//	                                   config.resource@type               Setting
//	  </resource>
//	</config>
//
// **Multi-instance same-name siblings** disambiguate via positional suffix
// (mirrors #88's HCL fix). `<usb>` × 4 sibling-of-same-name → `usb.0`,
// `usb.1`, `usb.2`, `usb.3`. A lone `<usb>` keeps `usb` (no suffix); the
// suffix only appears when 2+ siblings share a local name.
//
// **Namespaced elements/attributes** strip the namespace prefix from QN
// (`<android:intent-filter>` → `intent-filter`). The original element
// text is preserved in the symbol's Signature so `symbol get` still
// surfaces the literal source.
//
// **File extension scope**: `.xml`, `.xsd`, `.xsl`, `.xslt`, `.config`
// (the .NET app/web config). Explicitly NOT `.html` (#100 owns that)
// and NOT `.svg` (the structural attribute space — `d=`, `viewBox=`,
// transform matrices — is noise from a code-search standpoint).
//
// Confidence is 1.0 (parser-backed). Routes to the `config` corpus via
// ClassifyCorpus + the v9/v14 trigger WHERE clauses.
//
// Closes #101.
type xmlExtractor struct{}

func newXMLExtractor() *xmlExtractor { return &xmlExtractor{} }

func (x *xmlExtractor) Languages() []string { return []string{"XML"} }
func (x *xmlExtractor) Extensions() map[string]string {
	return map[string]string{
		".xml":    "XML",
		".xsd":    "XML",
		".xsl":    "XML",
		".xslt":   "XML",
		".config": "XML",
	}
}
func (x *xmlExtractor) Confidence() float64 { return 1.0 }

func (x *xmlExtractor) Extract(source []byte, _, relPath string, _ ExtractOptions) (result *FileResult) {
	result = &FileResult{Module: xmlModuleName(relPath)}
	if len(source) == 0 {
		return result
	}

	defer func() {
		if r := recover(); r != nil {
			if result == nil {
				result = &FileResult{Module: xmlModuleName(relPath)}
			}
		}
	}()

	type xmlNode struct {
		name      string // local name (namespace stripped)
		attrs     []xml.Attr
		startByte int
		endByte   int
		startLine int
		endLine   int
		children  []*xmlNode
	}

	dec := xml.NewDecoder(bytes.NewReader(source))
	dec.Strict = false             // tolerate unquoted attrs / mismatched casing
	dec.Entity = xml.HTMLEntity    // allow &nbsp; etc. without erroring
	dec.AutoClose = xml.HTMLAutoClose

	var root *xmlNode
	stack := []*xmlNode{}
	var prevOffset int64

	// findElementStart locates the `<` for the just-returned StartElement.
	// dec.InputOffset() is post-token; pre-offset is `prevOffset`. The
	// `<` lives in the gap, after any leading whitespace.
	findElementStart := func(from, to int) int {
		if from < 0 {
			from = 0
		}
		if to > len(source) {
			to = len(source)
		}
		if from >= to {
			return from
		}
		idx := bytes.IndexByte(source[from:to], '<')
		if idx < 0 {
			return from
		}
		return from + idx
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF || tok == nil {
			break
		}
		if err != nil {
			// Permissive: keep what we've got, abandon the rest. Whatever
			// the parser produced before erroring is still correct.
			break
		}
		afterOffset := dec.InputOffset()

		switch t := tok.(type) {
		case xml.StartElement:
			startByte := findElementStart(int(prevOffset), int(afterOffset))
			n := &xmlNode{
				name:      t.Name.Local,
				attrs:     append([]xml.Attr(nil), t.Attr...),
				startByte: startByte,
				startLine: byteOffsetToLine(source, startByte),
			}
			if root == nil {
				root = n
			}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) == 0 {
				break
			}
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			n.endByte = int(afterOffset)
			endLineByte := n.endByte - 1
			if endLineByte < 0 {
				endLineByte = 0
			}
			n.endLine = byteOffsetToLine(source, endLineByte)
			if n.endLine < n.startLine {
				n.endLine = n.startLine
			}
		}
		prevOffset = afterOffset
	}

	if root == nil {
		return result
	}

	// Close any unclosed elements (permissive parsing): extend through EOF.
	for _, n := range stack {
		if n.endByte == 0 {
			n.endByte = len(source)
		}
		endLineByte := n.endByte - 1
		if endLineByte < 0 {
			endLineByte = 0
		}
		n.endLine = byteOffsetToLine(source, endLineByte)
		if n.endLine < n.startLine {
			n.endLine = n.startLine
		}
	}

	emitNode := func(n *xmlNode, qn string) {
		result.Symbols = append(result.Symbols, ExtractedSymbol{
			Name:          n.name,
			QualifiedName: qn,
			Kind:          "Setting",
			StartByte:     n.startByte,
			EndByte:       n.endByte,
			StartLine:     n.startLine,
			EndLine:       n.endLine,
			Signature:     xmlStartTagSnippet(source, n.startByte),
			IsExported:    true,
		})
		for _, a := range n.attrs {
			attrName := a.Name.Local
			if attrName == "" {
				continue
			}
			attrQN := qn + "@" + attrName
			attrStart, attrEnd := xmlFindAttrRange(source, n.startByte, attrName)
			if attrStart == 0 || attrEnd <= attrStart {
				attrStart, attrEnd = n.startByte, n.startByte
			}
			result.Symbols = append(result.Symbols, ExtractedSymbol{
				Name:          attrName,
				QualifiedName: attrQN,
				Kind:          "Setting",
				StartByte:     attrStart,
				EndByte:       attrEnd,
				StartLine:     byteOffsetToLine(source, attrStart),
				EndLine:       byteOffsetToLine(source, attrEnd-1),
				Signature:     fmt.Sprintf("%s=%q", attrName, a.Value),
				IsExported:    true,
			})
		}
	}

	var emitChildren func(parent *xmlNode, parentPath string)
	emitChildren = func(parent *xmlNode, parentPath string) {
		// Per (parent, name) sibling counts drive the .N suffix decision.
		counts := map[string]int{}
		for _, c := range parent.children {
			counts[c.name]++
		}
		seen := map[string]int{}
		for _, c := range parent.children {
			if c.name == "" {
				continue
			}
			seg := c.name
			if counts[c.name] > 1 {
				seg = fmt.Sprintf("%s.%d", c.name, seen[c.name])
				seen[c.name]++
			}
			qn := parentPath + "." + seg
			emitNode(c, qn)
			emitChildren(c, qn)
		}
	}

	emitNode(root, root.name)
	emitChildren(root, root.name)

	return result
}

// xmlStartTagSnippet returns the literal source from the element's `<`
// through the next `>`. Used as the Setting symbol's Signature so the
// original (potentially namespaced) tag text survives the namespace-
// stripping done in QN. Returns a fallback if `>` isn't found.
func xmlStartTagSnippet(source []byte, startByte int) string {
	if startByte < 0 || startByte >= len(source) {
		return ""
	}
	tail := source[startByte:]
	if len(tail) > 256 {
		tail = tail[:256]
	}
	end := bytes.IndexByte(tail, '>')
	if end < 0 {
		return strings.TrimSpace(string(tail))
	}
	return string(tail[:end+1])
}

// xmlFindAttrRange locates an attribute by local name within the start
// tag that begins at elementStart. Returns the byte range of
// `attr="value"` (or `attr='value'` or unquoted `attr=value`). Returns
// (0,0) when the attribute isn't found in the source — defensive guard
// for templated XML where the parser saw an attr that the source doesn't
// literally contain at this offset.
func xmlFindAttrRange(source []byte, elementStart int, attrName string) (int, int) {
	if elementStart < 0 || elementStart >= len(source) || attrName == "" {
		return 0, 0
	}
	tagEnd := bytes.IndexByte(source[elementStart:], '>')
	if tagEnd < 0 {
		return 0, 0
	}
	tagEnd += elementStart
	region := source[elementStart : tagEnd+1]

	// Search for `<sep>attrName=` where <sep> is whitespace/`<`. A bare
	// IndexOf can mis-match prefixes (id= in maxid=). Iterate.
	needle := []byte(attrName + "=")
	pos := 0
	for pos < len(region) {
		idx := bytes.Index(region[pos:], needle)
		if idx < 0 {
			return 0, 0
		}
		abs := pos + idx
		// Char before must be whitespace or `<` (start of tag).
		if abs == 0 || isXMLAttrSep(region[abs-1]) {
			attrStart := elementStart + abs
			valStart := attrStart + len(attrName) + 1
			if valStart >= len(source) {
				return attrStart, valStart
			}
			quote := source[valStart]
			switch quote {
			case '"', '\'':
				closeIdx := bytes.IndexByte(source[valStart+1:tagEnd+1], quote)
				if closeIdx < 0 {
					return attrStart, valStart + 1
				}
				return attrStart, valStart + 1 + closeIdx + 1
			default:
				// Unquoted value (Strict=false tolerance). Scan to whitespace or `>`.
				e := valStart
				for e <= tagEnd && !isXMLAttrSep(source[e]) && source[e] != '>' && source[e] != '/' {
					e++
				}
				return attrStart, e
			}
		}
		pos = abs + 1
	}
	return 0, 0
}

func isXMLAttrSep(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '<'
}

func xmlModuleName(relPath string) string {
	base := filepath.Base(relPath)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}

func init() {
	Register(newXMLExtractor())
}
