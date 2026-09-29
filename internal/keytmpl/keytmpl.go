// Package keytmpl parses key templates such as "LIB#{libraryId}#TOOL#{toolId}".
//
// A template is literal text with {field} placeholders. Two placeholders must be separated by
// literal text, so a rendered key can always be read back by a human and so range bounds on a
// single field are well defined.
package keytmpl

import (
	"fmt"
	"strings"

	"github.com/nicklanng/dynago"
)

// Segment is one piece of a template: literal text, or a field reference with an optional
// transform ("{name|lower}").
type Segment struct {
	Literal   string
	Field     string
	Transform string
}

// Transforms are the functions a placeholder can apply to its value before it goes into a key.
var Transforms = map[string]func(string) string{
	// lower makes a key sort and match case-insensitively: "Zoe" and "adam" sort as "adam", "zoe".
	// It must match dynago.Lower, which generated code calls.
	"lower": dynago.Lower,
}

// IsField reports whether the segment is a field placeholder.
func (s Segment) IsField() bool { return s.Field != "" }

// Template is a parsed key template.
type Template struct {
	Raw      string
	Segments []Segment
}

// Parse parses a key template.
func Parse(raw string) (Template, error) {
	t := Template{Raw: raw}
	if raw == "" {
		return t, fmt.Errorf("empty key template")
	}
	var lit strings.Builder
	prevField := false
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; c {
		case '{':
			end := strings.IndexByte(raw[i:], '}')
			if end < 0 {
				return t, fmt.Errorf("key template %q: unclosed '{'", raw)
			}
			name, transform, _ := strings.Cut(raw[i+1:i+end], "|")
			if name == "" {
				return t, fmt.Errorf("key template %q: empty placeholder", raw)
			}
			if strings.ContainsAny(name, "{ \t") {
				return t, fmt.Errorf("key template %q: invalid placeholder %q", raw, name)
			}
			if _, ok := Transforms[transform]; transform != "" && !ok {
				return t, fmt.Errorf("key template %q: unknown transform %q (known: lower)", raw, transform)
			}
			if lit.Len() > 0 {
				t.Segments = append(t.Segments, Segment{Literal: lit.String()})
				lit.Reset()
			} else if prevField {
				return t, fmt.Errorf("key template %q: placeholders must be separated by literal text", raw)
			}
			t.Segments = append(t.Segments, Segment{Field: name, Transform: transform})
			prevField = true
			i += end
		case '}':
			return t, fmt.Errorf("key template %q: unexpected '}'", raw)
		default:
			lit.WriteByte(c)
			prevField = false
		}
	}
	if lit.Len() > 0 {
		t.Segments = append(t.Segments, Segment{Literal: lit.String()})
	}
	return t, nil
}

// MustParse parses a template and panics on error. For tests.
func MustParse(raw string) Template {
	t, err := Parse(raw)
	if err != nil {
		panic(err)
	}
	return t
}

// Fields returns the field names referenced by the template, in order, without duplicates.
func (t Template) Fields() []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range t.Segments {
		if s.IsField() && !seen[s.Field] {
			seen[s.Field] = true
			out = append(out, s.Field)
		}
	}
	return out
}

// LiteralPrefix returns the literal text before the first placeholder (the whole template if it
// has no placeholders).
func (t Template) LiteralPrefix() string {
	if len(t.Segments) == 0 || t.Segments[0].IsField() {
		return ""
	}
	return t.Segments[0].Literal
}

// IsConstant reports whether the template has no placeholders.
func (t Template) IsConstant() bool {
	for _, s := range t.Segments {
		if s.IsField() {
			return false
		}
	}
	return true
}

// FirstField returns the first placeholder, or "" if there is none.
func (t Template) FirstField() string {
	for _, s := range t.Segments {
		if s.IsField() {
			return s.Field
		}
	}
	return ""
}

// Render substitutes values for placeholders. Missing values render as "{field}", which is what
// documentation wants for a key pattern with no example.
func (t Template) Render(values map[string]string) string {
	var b strings.Builder
	for _, s := range t.Segments {
		if !s.IsField() {
			b.WriteString(s.Literal)
			continue
		}
		v, ok := values[s.Field]
		switch {
		case !ok:
			b.WriteString("{" + s.Field + "}")
		case s.Transform != "":
			b.WriteString(Transforms[s.Transform](v))
		default:
			b.WriteString(v)
		}
	}
	return b.String()
}

// MayOverlap reports whether two templates could render to the same string. It compares the
// literal prefixes: if neither is a prefix of the other, no two renders can be equal. Otherwise
// the answer is conservatively yes.
func MayOverlap(a, b Template) bool {
	pa, pb := a.LiteralPrefix(), b.LiteralPrefix()
	if a.IsConstant() && b.IsConstant() {
		return pa == pb
	}
	if a.IsConstant() {
		return strings.HasPrefix(pa, pb)
	}
	if b.IsConstant() {
		return strings.HasPrefix(pb, pa)
	}
	return strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)
}

// PrefixCollides reports whether a begins_with query on prefix a could also match keys rendered
// from template b (and vice versa).
func PrefixCollides(a, b Template) bool {
	pa, pb := a.LiteralPrefix(), b.LiteralPrefix()
	if a.IsConstant() && b.IsConstant() {
		return pa == pb
	}
	return strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)
}
