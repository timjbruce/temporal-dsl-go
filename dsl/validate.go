package dsl

import (
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"
)

type FieldError struct {
	Field
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

type Validation struct {
	Missing []RequiredField `json:"missing"`
	Invalid []FieldError    `json:"invalid"`
	Unused  []string        `json:"unused"`
}

func (v Validation) OK() bool { return len(v.Missing) == 0 && len(v.Invalid) == 0 }

func Validate(p Plan, data map[string]string) Validation {
	v := Validation{Missing: []RequiredField{}, Invalid: []FieldError{}, Unused: []string{}}

	used := map[string]bool{}
	for _, rf := range p.Required {
		// A field offering alternatives is satisfied by whichever one the caller
		// actually supplied, so all of them count as used.
		supplied, ok := "", false
		for _, f := range rf.Choices() {
			used[f.Name] = true
			if raw := strings.TrimSpace(data[f.Name]); raw != "" && !ok {
				supplied, ok = f.Name, true
			}
		}

		if !ok {
			v.Missing = append(v.Missing, rf)
			continue
		}

		field := rf.Field
		for _, f := range rf.Choices() {
			if f.Name == supplied {
				field = f
			}
		}
		if reason := checkType(field, data[supplied]); reason != "" {
			v.Invalid = append(v.Invalid, FieldError{Field: field, Value: data[supplied], Reason: reason})
		}
	}

	for key := range data {
		if !used[key] {
			v.Unused = append(v.Unused, key)
		}
	}
	sortStrings(v.Unused)

	return v
}

// CheckValue reports why a value is unusable for a registered field, or "" if it
// is fine. An unregistered name is not this package's business, and an empty
// value is absence rather than malformation — both are the caller's to judge.
func CheckValue(name, raw string) string {
	f, ok := Fields[name]
	if !ok || strings.TrimSpace(raw) == "" {
		return ""
	}
	return checkType(f, raw)
}

func checkType(f Field, raw string) string {
	raw = strings.TrimSpace(raw)
	switch f.Type {
	case TypeInteger:
		if _, err := strconv.Atoi(raw); err != nil {
			return fmt.Sprintf("expected a whole number, got %q", raw)
		}
	case TypeNumber:
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			return fmt.Sprintf("expected a number, got %q", raw)
		}
	case TypeEmail:
		if _, err := mail.ParseAddress(raw); err != nil {
			return fmt.Sprintf("expected an email address, got %q", raw)
		}
	case TypeDate:
		if _, err := time.Parse("2006-01-02", raw); err != nil {
			return fmt.Sprintf("expected a date as YYYY-MM-DD, got %q", raw)
		}
	}
	return ""
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
