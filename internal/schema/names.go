package schema

import (
	"regexp"
	"strings"
	"unicode"
)

var (
	reExported = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	reLower    = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
	reAttr     = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	rePackage  = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
)

var initialisms = map[string]string{
	"id": "ID", "url": "URL", "uri": "URI", "http": "HTTP", "html": "HTML", "api": "API",
	"uid": "UID", "uuid": "UUID", "ulid": "ULID", "json": "JSON", "ttl": "TTL", "sku": "SKU",
	"ip": "IP", "sql": "SQL", "css": "CSS", "xml": "XML", "sms": "SMS", "https": "HTTPS",
}

// GoName converts a schema name (camelCase, snake_case or PascalCase) to an exported Go name,
// upper-casing common initialisms and their plurals: "libraryId" → "LibraryID", "labelIds" →
// "LabelIDs", "on_loan" → "OnLoan".
func GoName(s string) string {
	var b strings.Builder
	for _, w := range words(s) {
		low := strings.ToLower(w)
		if up, ok := initialisms[low]; ok {
			b.WriteString(up)
			continue
		}
		if up, ok := initialisms[strings.TrimSuffix(low, "s")]; ok {
			b.WriteString(up + "s")
			continue
		}
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		b.WriteString(string(r))
	}
	return b.String()
}

// words splits camelCase, PascalCase, snake_case and kebab-case into words.
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = nil
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			// Separators and symbols ("in-progress", "c++") can't appear in Go names.
			flush()
		case unicode.IsUpper(r):
			// Start a new word at a lower→upper boundary, or at the last capital of an acronym
			// followed by a lower-case letter ("HTMLBody" → "HTML", "Body").
			if len(cur) > 0 && (unicode.IsLower(cur[len(cur)-1]) || unicode.IsDigit(cur[len(cur)-1]) ||
				(i+1 < len(rs) && unicode.IsLower(rs[i+1]))) {
				flush()
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// Plural returns a simple English plural of a Go name, used for store field names.
func Plural(s string) string {
	switch {
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsRune("aeiou", rune(s[len(s)-2])):
		return s[:len(s)-1] + "ies"
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	}
	return s + "s"
}

// unexported returns the lower-case form of a Go name that generated unexported helpers use,
// matching the generator: "Loan" → "loan", "APIKey" → "apiKey", "THING" → "thing".
func unexported(s string) string {
	r := []rune(s)
	n := 0
	for n < len(r) && unicode.IsUpper(r[n]) {
		n++
	}
	if n > 1 && n < len(r) && !pluralInitialism(r, n) {
		n--
	}
	for i := 0; i < n; i++ {
		r[i] = unicode.ToLower(r[i])
	}
	return string(r)
}

// pluralInitialism reports whether the n capitals starting r are an initialism in the plural
// ("IDs", "URLsSeen"), whose last capital doesn't start the next word.
func pluralInitialism(r []rune, n int) bool {
	return r[n] == 's' && (n+1 == len(r) || unicode.IsUpper(r[n+1]))
}
