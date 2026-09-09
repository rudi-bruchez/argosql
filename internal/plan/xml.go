// Package plan turns one Query Store compiled plan XML value into a
// complete, byte-preserving raw export (xml.go) and a small,
// memory-bounded summary (summary.go) - never a whole-document DOM
// (design spec, line 89: "Parse XML incrementally with a token reader,
// avoiding a whole-document DOM").
package plan

import (
	"bufio"
	"bytes"
	"io"
	"regexp"
	"strings"
)

// declMaxScanBytes bounds how far NormalizeXML ever looks for the
// terminating "?>" of a leading XML declaration, once leadingXMLDecl
// has confirmed the document actually starts with one: generous
// enough to cover any realistic declaration, even one artificially
// padded with whitespace between its pseudo-attributes (fix 1's own
// regression fixture does exactly this, deliberately, to prove the
// bound is not shorter than a real declaration can be), while still
// being a fixed, bounded scan - never proportional to the rest of a
// potentially 80 KiB+ plan (the same bounded-parsing discipline design
// spec line 89 states for Summarize, applied here to line 91's own
// declaration rule).
//
// Fix 1's A8 raised this from a previous, too-short 512: measured
// against a real repro (`<?xml version="1.0" ` followed by 512 spaces
// then `encoding="utf-16"?>`), the declaration's own encoding
// attribute started past byte 512 and was missed entirely, exporting
// a document whose declaration still named "utf-16" over bytes that
// were, in fact, UTF-8.
const declMaxScanBytes = 8192

// leadingWhitespace is the ASCII whitespace NormalizeXML tolerates
// before a document's own XML declaration - strict XML disallows any
// whitespace there at all, but this task's own fix-1 brief asks for
// it defensively ("après un éventuel espace blanc").
const leadingWhitespace = " \t\r\n"

// xmlDeclEncodingAttr matches an "encoding" pseudo-attribute anywhere
// within an ALREADY-ISOLATED declaration span (leadingXMLDecl's own
// return value), never against the raw document: it can only ever
// match real declaration content this way, never a look-alike sitting
// inside a comment or anywhere else in the document (fix 1's A8: the
// previous, unanchored form matched
// "<!-- <?xml version=\"1.0\" encoding=\"utf-16\"?> -->" and rewrote
// bytes inside that comment).
var xmlDeclEncodingAttr = regexp.MustCompile(`\bencoding\s*=\s*(['"])([^'"]*)['"]`)

// leadingXMLDecl reports the exact byte range, within prefix, of a
// well-formed-looking XML declaration ANCHORED at the very start of
// the document (after skipping leadingWhitespace): the literal
// "<?xml", up to and including the first "?>" found within prefix.
//
// found is false whenever the document does not begin with "<?xml" at
// all (after any leading whitespace), or begins with it but no "?>"
// appears within prefix - a declaration this function refuses to
// guess the end of, rather than scanning further unboundedly. Either
// way, NormalizeXML then treats the document as declaration-free and
// copies it verbatim, never altering bytes it cannot confidently
// attribute to a real, anchored declaration.
func leadingXMLDecl(prefix []byte) (start, end int, found bool) {
	i := 0
	for i < len(prefix) && strings.IndexByte(leadingWhitespace, prefix[i]) >= 0 {
		i++
	}
	if !bytes.HasPrefix(prefix[i:], []byte("<?xml")) {
		return 0, 0, false
	}
	closeIdx := bytes.Index(prefix[i:], []byte("?>"))
	if closeIdx < 0 {
		return 0, 0, false
	}
	return i, i + closeIdx + len("?>"), true
}

// NormalizeXML copies src to dst byte for byte - no line-ending
// normalization, no synthesized XML declaration or BOM (design spec,
// line 91: "Export SQL text, definitions and plan XML as UTF-8 without
// BOM and without line-ending normalization... Do not synthesize an
// XML declaration") - with exactly one exception: if the document's
// own LEADING declaration (leadingXMLDecl - anchored at the very
// start, never matched anywhere else in the document) carries an
// encoding attribute naming anything other than UTF-8, that one
// attribute's VALUE is rewritten to "utf-8" and normalized reports
// true; every other byte of the document, including the rest of that
// same declaration, is untouched. A declaration-free source, or one
// already declaring UTF-8, passes through completely unmodified and
// normalized is false (design spec, line 91: "For the ordinary
// declaration-free source, compare the artifact bytes to the UTF-8
// encoding of the exact SQL value").
//
// This exists because the driver has already decoded src into a Go
// string as UTF-8 before this package ever sees it (database/sql's own
// contract for every string this project reads - see
// internal/output/cell.go's own doc comment on the driver/Cell
// mapping): a leftover declaration naming, say, "utf-16" describes
// bytes that no longer exist by the time NormalizeXML runs, and a
// consumer trusting that stale declaration over the artifact's actual
// bytes would misdecode a file that is, in fact, valid UTF-8. It also
// matters for correctness, not only labeling: encoding/xml's own
// Decoder refuses to parse a document whose declared encoding it does
// not recognize without a registered CharsetReader, so
// internal/diagnostics' own Plan normalizes into a file BEFORE ever
// handing that same file to Summarize, never the raw driver string -
// see plan.go's own exportAndSummarize.
func NormalizeXML(src io.Reader, dst io.Writer) (normalized bool, err error) {
	br := bufio.NewReaderSize(src, declMaxScanBytes)
	prefix, peekErr := br.Peek(declMaxScanBytes)
	if peekErr != nil && peekErr != io.EOF {
		return false, peekErr
	}

	out := prefix
	if start, end, found := leadingXMLDecl(prefix); found {
		decl := prefix[start:end]
		if loc := xmlDeclEncodingAttr.FindSubmatchIndex(decl); loc != nil {
			value := string(decl[loc[4]:loc[5]])
			if !strings.EqualFold(value, "utf-8") {
				valueStart, valueEnd := start+loc[4], start+loc[5]
				rewritten := make([]byte, 0, len(prefix))
				rewritten = append(rewritten, prefix[:valueStart]...)
				rewritten = append(rewritten, "utf-8"...)
				rewritten = append(rewritten, prefix[valueEnd:]...)
				out = rewritten
				normalized = true
			}
		}
	}

	if _, err = dst.Write(out); err != nil {
		return normalized, err
	}
	if _, err = br.Discard(len(prefix)); err != nil {
		return normalized, err
	}
	if _, err = io.Copy(dst, br); err != nil {
		return normalized, err
	}
	return normalized, nil
}
