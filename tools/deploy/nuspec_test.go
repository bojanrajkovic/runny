// Package deploy holds the release workflow's packaging templates. It has no
// Go code — only this test, which guards the one template that is parsed by a
// machine rather than read by a human.
package deploy

import (
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

// The nuspec is XML, and `dotnet pack` only ever runs inside the release
// workflow — so a malformed template is invisible until a release is already
// cut, at which point the tag exists, the GitHub release exists, and nothing
// has been published. That is exactly how this test came to be: an XML comment
// in the template contained "--", which XML forbids, and the failure surfaced
// only after the tag was pushed.
//
// Parsing the raw template (not a rendered copy) is deliberate: the ${VAR}
// placeholders envsubst fills are XML text, so the template is well-formed XML
// in its own right, and checking it here needs no rendering step to drift.
func TestNuspecTemplateIsWellFormedXML(t *testing.T) {
	const path = "runny.nuspec.tmpl"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var v any
	if err := xml.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s is not well-formed XML, so `dotnet pack` will fail mid-release: %v", path, err)
	}
}

// The "--" rule is the specific trap, and xml.Unmarshal is lenient about some
// comment content, so assert it directly with a message naming the fix.
func TestNuspecTemplateCommentsHaveNoDoubleHyphen(t *testing.T) {
	const path = "runny.nuspec.tmpl"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	s := string(b)
	for rest := s; ; {
		open := strings.Index(rest, "<!--")
		if open < 0 {
			return
		}
		body := rest[open+len("<!--"):]
		end := strings.Index(body, "-->")
		if end < 0 {
			t.Fatalf("%s has an unterminated XML comment", path)
		}
		if i := strings.Index(body[:end], "--"); i >= 0 {
			line := 1 + strings.Count(s[:len(s)-len(rest)+open+len("<!--")+i], "\n")
			t.Errorf("%s line %d: an XML comment contains \"--\", which XML forbids and `dotnet pack` rejects. "+
				"Reword it — flags like --version have to be described rather than written literally.", path, line)
		}
		rest = body[end+len("-->"):]
	}
}
