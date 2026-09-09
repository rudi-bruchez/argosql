// Package plan turns one Query Store compiled plan XML value into a
// complete, byte-preserving raw export (xml.go) and a small,
// memory-bounded summary (summary.go) - never a whole-document DOM
// (design spec, line 89: "Parse XML incrementally with a token reader,
// avoiding a whole-document DOM").
package plan

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

// declarationPeekBytes bounds how much of src's prologue NormalizeXML
// ever inspects looking for an XML declaration's own encoding
// attribute. A declaration, when present, must be the very first thing
// in the document, and is always a short, single line - this task's
// own brief instruction ("inspecter uniquement le prologue") and the
// design spec's own bounded-parsing discipline (line 89) both rule out
// scanning any further into what can be an 80 KiB+ plan looking for
// one.
const declarationPeekBytes = 512

// xmlDeclEncoding matches an XML declaration's own "encoding"
// pseudo-attribute anywhere within a peeked prologue window,
// single- or double-quoted (both are legal XML). Capture group 2 is
// the raw encoding value's own byte range within the match - the only
// span NormalizeXML ever rewrites.
var xmlDeclEncoding = regexp.MustCompile(`(?i)<\?xml[^>]*\bencoding\s*=\s*(['"])([^'"]*)['"]`)

// NormalizeXML copies src to dst byte for byte - no line-ending
// normalization, no synthesized XML declaration or BOM (design spec,
// line 91: "Export SQL text, definitions and plan XML as UTF-8 without
// BOM and without line-ending normalization... Do not synthesize an
// XML declaration") - with exactly one exception: if src's own
// prologue (its first declarationPeekBytes bytes) carries an XML
// declaration whose encoding attribute names anything other than
// UTF-8, that one attribute's VALUE is rewritten to "utf-8" and
// normalized reports true; every other byte of the document, including
// the rest of that same declaration, is untouched. A declaration-free
// source, or one already declaring UTF-8, passes through completely
// unmodified and normalized is false (design spec, line 91: "For the
// ordinary declaration-free source, compare the artifact bytes to the
// UTF-8 encoding of the exact SQL value").
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
	br := bufio.NewReaderSize(src, declarationPeekBytes)
	prologue, peekErr := br.Peek(declarationPeekBytes)
	if peekErr != nil && peekErr != io.EOF {
		return false, peekErr
	}

	out := prologue
	if loc := xmlDeclEncoding.FindSubmatchIndex(prologue); loc != nil {
		value := string(prologue[loc[4]:loc[5]])
		if !strings.EqualFold(value, "utf-8") {
			rewritten := make([]byte, 0, len(prologue))
			rewritten = append(rewritten, prologue[:loc[4]]...)
			rewritten = append(rewritten, "utf-8"...)
			rewritten = append(rewritten, prologue[loc[5]:]...)
			out = rewritten
			normalized = true
		}
	}

	if _, err = dst.Write(out); err != nil {
		return normalized, err
	}
	if _, err = br.Discard(len(prologue)); err != nil {
		return normalized, err
	}
	if _, err = io.Copy(dst, br); err != nil {
		return normalized, err
	}
	return normalized, nil
}
