package plan

import (
	"bytes"
	"strings"
	"testing"
)

// TestNormalizeXML is task 12's own directing test (brief, verbatim):
// a declaration-free source passes through completely unmodified; a
// non-UTF-8 encoding declaration is rewritten to name utf-8.
func TestNormalizeXML(t *testing.T) {
	for _, input := range []string{`<ShowPlanXML/>`, `<?xml version="1.0" encoding="utf-16"?><ShowPlanXML/>`} {
		var out bytes.Buffer
		changed, err := NormalizeXML(strings.NewReader(input), &out)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(input, "utf-16") && (!changed || !strings.Contains(out.String(), "utf-8")) {
			t.Fatal(out.String())
		}
		if !strings.Contains(input, "utf-16") && out.String() != input {
			t.Fatal("source changed")
		}
	}
}

// bigCRLFMultibyteDoc builds a payload comfortably over
// declarationPeekBytes and over io.Copy's own default internal buffer
// size, with real CRLF line endings AND multi-byte UTF-8 text
// throughout the SAME document - this task's dispatch requires exactly
// this combination, so a mutation that normalizes CRLF to LF, or one
// that prepends a BOM, has real content to corrupt across more than
// one copy buffer's worth of bytes (mirroring
// tests/integration/query_test.go's own b4BigCRLFText, at the unit
// level, for NormalizeXML directly rather than for the whole
// artifacts pipeline).
func bigCRLFMultibyteDoc() string {
	var b strings.Builder
	b.WriteString("<ShowPlanXML>\r\n")
	line := "  <RelOp PhysicalOp=\"Clustered Index Scan\" NodeId=\"0\" Comment=\"accents éàüñ et 漢字ひらがな 한글\"/>\r\n"
	for b.Len() < 48*1024 {
		b.WriteString(line)
	}
	b.WriteString("</ShowPlanXML>")
	return b.String()
}

// TestNormalizeXMLByteIdentityWithCRLFAndMultibyte is this task's own
// mandatory cassure-5 target (dispatch: "cassure 5... fais échouer sur
// CHACUNE des deux [CRLF normalization, BOM]"). A declaration-free
// source with real CRLF and multi-byte UTF-8 text throughout must come
// out of NormalizeXML byte for byte identical to what went in: no
// line-ending normalization (design spec, line 91), no synthesized
// BOM. See this task's report for the two breakages run against this
// test.
func TestNormalizeXMLByteIdentityWithCRLFAndMultibyte(t *testing.T) {
	input := bigCRLFMultibyteDoc()
	if !strings.Contains(input, "\r\n") {
		t.Fatal("fixture lost its CRLF: not testing what it claims to")
	}
	if len(input) < 32*1024 {
		t.Fatalf("fixture too small to exercise more than one copy buffer: %d bytes", len(input))
	}

	var out bytes.Buffer
	changed, err := NormalizeXML(strings.NewReader(input), &out)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("no encoding declaration present: normalized should be false")
	}
	if out.String() != input {
		t.Fatalf("output not byte-identical: got %d bytes, want %d bytes", out.Len(), len(input))
	}
	if bytes.HasPrefix(out.Bytes(), []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("output carries a synthesized UTF-8 BOM")
	}
}

// TestNormalizeXMLSingleQuotedEncoding proves the prologue scan accepts
// XML's other legal quoting style for the encoding pseudo-attribute,
// not only double quotes.
func TestNormalizeXMLSingleQuotedEncoding(t *testing.T) {
	input := `<?xml version='1.0' encoding='iso-8859-1'?><ShowPlanXML/>`
	var out bytes.Buffer
	changed, err := NormalizeXML(strings.NewReader(input), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !strings.Contains(out.String(), "utf-8") {
		t.Fatalf("single-quoted encoding not normalized: %q", out.String())
	}
}

// TestNormalizeXMLDeclarationAlreadyUTF8 proves an explicit, already-
// correct UTF-8 declaration is left untouched byte for byte and
// reported as not normalized - only a MISMATCHED declaration is ever
// rewritten.
func TestNormalizeXMLDeclarationAlreadyUTF8(t *testing.T) {
	input := `<?xml version="1.0" encoding="UTF-8"?><ShowPlanXML/>`
	var out bytes.Buffer
	changed, err := NormalizeXML(strings.NewReader(input), &out)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("already-UTF-8 declaration should not be reported as normalized")
	}
	if out.String() != input {
		t.Fatalf("output not byte-identical: got %q, want %q", out.String(), input)
	}
}
