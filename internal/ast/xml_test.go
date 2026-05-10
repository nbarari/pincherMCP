package ast

import (
	"strings"
	"testing"
)

// TestXML_ConfigShape covers the canonical config-style XML (Spring-bean
// or app.config-shaped): nested elements with text-leaf values. Every
// element + every attribute becomes a Setting; QNs are dotted paths
// rooted at the document element name.
func TestXML_ConfigShape(t *testing.T) {
	src := []byte(`<config>
  <database>
    <host>localhost</host>
    <port>5432</port>
  </database>
  <resource id="db1" type="postgres">
    <connection url="postgres://localhost"/>
  </resource>
</config>`)
	got := (&xmlExtractor{}).Extract(src, "XML", "app.xml", ExtractOptions{})

	wantQNs := map[string]bool{
		"config":                            false,
		"config.database":                   false,
		"config.database.host":              false,
		"config.database.port":              false,
		"config.resource":                   false,
		"config.resource@id":                false,
		"config.resource@type":              false,
		"config.resource.connection":        false,
		"config.resource.connection@url":    false,
	}
	for _, s := range got.Symbols {
		if s.Kind != "Setting" {
			t.Errorf("kind=%q, want Setting for %q", s.Kind, s.QualifiedName)
		}
		if _, ok := wantQNs[s.QualifiedName]; ok {
			wantQNs[s.QualifiedName] = true
		}
	}
	for qn, found := range wantQNs {
		if !found {
			t.Errorf("expected QN %q not produced; got: %v", qn, xmlQNList(got.Symbols))
		}
	}
}

func xmlQNList(syms []ExtractedSymbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.QualifiedName)
	}
	return out
}

// TestXML_MultiInstanceSiblings covers the #88 lesson: multiple
// same-named siblings must disambiguate via positional suffix or the
// QN-collision sanity heuristic fires.
func TestXML_MultiInstanceSiblings(t *testing.T) {
	src := []byte(`<vm>
  <usb model="A"/>
  <usb model="B"/>
  <usb model="C"/>
  <usb model="D"/>
  <other id="x"/>
</vm>`)
	got := (&xmlExtractor{}).Extract(src, "XML", "vm.xml", ExtractOptions{})

	want := []string{"vm.usb.0", "vm.usb.1", "vm.usb.2", "vm.usb.3", "vm.other"}
	have := map[string]bool{}
	for _, s := range got.Symbols {
		have[s.QualifiedName] = true
	}
	for _, qn := range want {
		if !have[qn] {
			t.Errorf("expected %q in symbols; got %v", qn, xmlQNList(got.Symbols))
		}
	}
	// Reject the un-suffixed bare `vm.usb` — collision would mean we
	// silently dropped 3 of the 4.
	if have["vm.usb"] {
		t.Errorf("did not expect bare %q (siblings should be indexed); got %v",
			"vm.usb", xmlQNList(got.Symbols))
	}
	// `vm.other` is alone among siblings — no index suffix.
	if have["vm.other.0"] {
		t.Errorf("did not expect indexed %q (lone sibling); got %v",
			"vm.other.0", xmlQNList(got.Symbols))
	}
}

// TestXML_NamespaceStripped covers the namespace handling rule: QNs use
// local names only (`<android:intent-filter>` → `intent-filter`); the
// original prefix survives in Signature.
func TestXML_NamespaceStripped(t *testing.T) {
	src := []byte(`<manifest xmlns:android="http://schemas.android.com/apk/res/android">
  <android:intent-filter>
    <android:action android:name="MAIN"/>
  </android:intent-filter>
</manifest>`)
	got := (&xmlExtractor{}).Extract(src, "XML", "AndroidManifest.xml", ExtractOptions{})

	have := map[string]string{} // qn -> signature
	for _, s := range got.Symbols {
		have[s.QualifiedName] = s.Signature
	}
	if _, ok := have["manifest.intent-filter"]; !ok {
		t.Errorf("expected QN %q (namespace stripped); got %v",
			"manifest.intent-filter", xmlQNList(got.Symbols))
	}
	if _, ok := have["manifest.intent-filter.action"]; !ok {
		t.Errorf("expected QN %q; got %v",
			"manifest.intent-filter.action", xmlQNList(got.Symbols))
	}
	// Signature should still carry the original `android:` prefix.
	sig := have["manifest.intent-filter"]
	if !strings.Contains(sig, "android:intent-filter") {
		t.Errorf("expected Signature to preserve namespace prefix, got %q", sig)
	}
}

// TestXML_AttributeByteRanges covers that attribute byte ranges land
// within the start tag (not on the parent element's full extent).
func TestXML_AttributeByteRanges(t *testing.T) {
	src := []byte(`<root key="value" other='foo'/>`)
	got := (&xmlExtractor{}).Extract(src, "XML", "x.xml", ExtractOptions{})

	for _, s := range got.Symbols {
		if s.QualifiedName == "root@key" {
			snippet := string(src[s.StartByte:s.EndByte])
			if snippet != `key="value"` {
				t.Errorf("root@key byte range = %q, want `key=\"value\"`", snippet)
			}
		}
		if s.QualifiedName == "root@other" {
			snippet := string(src[s.StartByte:s.EndByte])
			if snippet != `other='foo'` {
				t.Errorf("root@other byte range = %q, want `other='foo'`", snippet)
			}
		}
	}
}

// TestXML_MalformedTolerated covers permissive parsing: a templated /
// truncated XML file should produce as many symbols as the parser was
// able to read, not zero. Mirrors HTML's permissive contract.
func TestXML_MalformedTolerated(t *testing.T) {
	// Unclosed `<config>` — encoding/xml errors on EOF inside an open
	// element. We accept whatever it returned before the error.
	src := []byte(`<config>
  <database>
    <host>localhost</host>
    <port>5432`)
	got := (&xmlExtractor{}).Extract(src, "XML", "broken.xml", ExtractOptions{})
	// Should not panic, should return a result, may have partial symbols
	// (at minimum `config.database.host` which closed cleanly).
	if got == nil {
		t.Fatal("malformed XML caused nil result")
	}
	have := map[string]bool{}
	for _, s := range got.Symbols {
		have[s.QualifiedName] = true
	}
	if !have["config.database.host"] {
		t.Errorf("expected partial QN %q from malformed input; got %v",
			"config.database.host", xmlQNList(got.Symbols))
	}
}

// TestXML_EmptyInput covers the zero-byte case — must return an empty
// result without panicking.
func TestXML_EmptyInput(t *testing.T) {
	got := (&xmlExtractor{}).Extract(nil, "XML", "empty.xml", ExtractOptions{})
	if got == nil {
		t.Fatal("nil input returned nil result")
	}
	if len(got.Symbols) != 0 {
		t.Errorf("nil input produced %d symbols, want 0", len(got.Symbols))
	}
	// Empty bytes also.
	got = (&xmlExtractor{}).Extract([]byte{}, "XML", "empty.xml", ExtractOptions{})
	if len(got.Symbols) != 0 {
		t.Errorf("empty input produced %d symbols, want 0", len(got.Symbols))
	}
}

// TestXML_ExtensionsRegistered covers that .xml/.xsd/.xsl/.xslt/.config
// all detect as XML, but .html and .svg do not (per the direction
// comment's explicit exclusions).
func TestXML_ExtensionsRegistered(t *testing.T) {
	cases := map[string]string{
		"a.xml":    "XML",
		"a.xsd":    "XML",
		"a.xsl":    "XML",
		"a.xslt":   "XML",
		"web.config": "XML",
		"a.html":   "HTML", // owned by #100, not us
		"a.svg":    "",     // explicitly excluded
	}
	for name, want := range cases {
		got := DetectLanguage(name)
		if got != want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestXML_Confidence covers the parser-backed claim: confidence should
// be 1.0, matching TOML/HTML/Markdown.
func TestXML_Confidence(t *testing.T) {
	if c := (&xmlExtractor{}).Confidence(); c != 1.0 {
		t.Errorf("XML extractor confidence = %v, want 1.0", c)
	}
}

// TestXML_TextLeavesEmittedAsSettings covers that pure-text leaf
// elements (no children, just CharData) still emit as Settings — the
// element-as-Setting model treats every element uniformly.
func TestXML_TextLeavesEmittedAsSettings(t *testing.T) {
	src := []byte(`<root>
  <name>pincher</name>
  <version>0.6.0</version>
</root>`)
	got := (&xmlExtractor{}).Extract(src, "XML", "x.xml", ExtractOptions{})
	have := map[string]string{}
	for _, s := range got.Symbols {
		have[s.QualifiedName] = string(src[s.StartByte:s.EndByte])
	}
	if !strings.Contains(have["root.name"], "<name>pincher</name>") {
		t.Errorf("root.name byte range should cover the full element; got %q",
			have["root.name"])
	}
	if !strings.Contains(have["root.version"], "<version>0.6.0</version>") {
		t.Errorf("root.version byte range should cover the full element; got %q",
			have["root.version"])
	}
}
