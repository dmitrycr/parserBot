package parser

import (
	"strings"
	"unicode"
)

type Match struct {
	Keyword string
}

type Matcher struct {
	keywords []string
}

func NewMatcher(keywords []string) Matcher {
	normalized := make([]string, 0, len(keywords))
	for _, keyword := range keywords {
		keyword = normalize(keyword)
		if keyword == "" {
			continue
		}
		normalized = append(normalized, keyword)
	}

	return Matcher{keywords: normalized}
}

func (m Matcher) Empty() bool {
	return len(m.keywords) == 0
}

func (m Matcher) Match(text string) (Match, bool) {
	text = normalize(text)
	if text == "" {
		return Match{}, false
	}

	for _, keyword := range m.keywords {
		if strings.Contains(text, keyword) {
			return Match{Keyword: keyword}, true
		}
	}

	return Match{}, false
}

func normalize(value string) string {
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, "ё", "е")

	var builder strings.Builder
	builder.Grow(len(value))

	previousSpace := false
	for _, r := range value {
		if unicode.IsSpace(r) {
			if !previousSpace {
				builder.WriteRune(' ')
				previousSpace = true
			}
			continue
		}

		builder.WriteRune(r)
		previousSpace = false
	}

	return strings.TrimSpace(builder.String())
}
