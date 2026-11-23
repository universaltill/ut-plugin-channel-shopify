package app

import (
	"html"
	"strings"
	"unicode/utf8"
)

// Description, SKU and barcode sync (ut-docs#6843, ADR-0202 amendment
// 2026-11-23, contract section 8.2a).

const (
	// maxDescRunes bounds the description written (the import clips at 5000
	// bytes too): over it the op is refused before any request.
	maxDescRunes = 5000
	// maxCodeRunes bounds a SKU and a barcode (Shopify allows 255).
	maxCodeRunes     = 255
	kDescriptionHTML = "descriptionHtml"
)

// NormalizeText is the comparison form of a description: tags stripped (each
// tag counts as a space, so "<p>a</p><p>b</p>" equals "a\n\nb"), entities
// decoded, whitespace collapsed, trimmed. The cloud compares and hashes on the
// same function (ADR-0202 amendment).
func NormalizeText(s string) string {
	s = strings.ToValidUTF8(s, "")
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(s)
			break
		}
		j := strings.IndexByte(s[i:], '>')
		if j < 0 || !tagStart(s[i+1:]) { // not a tag ("1 < 2", an unclosed "<"): text
			b.WriteString(s[:i+1])
			s = s[i+1:]
			continue
		}
		b.WriteString(s[:i])
		b.WriteByte(' ')
		s = s[i+j+1:]
	}
	return strings.Join(strings.Fields(html.UnescapeString(b.String())), " ")
}

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// descriptionHTML turns item text into escaped <p> paragraphs (one per
// non-empty line) and nothing else: no tag from the item is ever emitted.
func descriptionHTML(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			b.WriteString("<p>" + htmlEscaper.Replace(line) + "</p>")
		}
	}
	return b.String()
}

// validCode is a SKU or barcode to write: trimmed, valid UTF-8, at most 255
// characters. Empty is "nothing to write" (a push never clears a channel value).
func validCode(s string) (string, bool) {
	t := strings.TrimSpace(s)
	return t, utf8.ValidString(t) && utf8.RuneCountInString(t) <= maxCodeRunes
}

// tagStart: an HTML tag opens with a letter, "/" or "!" right after "<".
func tagStart(rest string) bool {
	if rest == "" {
		return false
	}
	c := rest[0]
	return c == '/' || c == '!' || (c|0x20 >= 'a' && c|0x20 <= 'z')
}
