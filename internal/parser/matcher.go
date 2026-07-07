package parser

import (
	"strings"
	"unicode"
)

type Match struct {
	Keyword string
}

type Matcher struct {
	keywords        []string
	excludeKeywords []string
}

func NewMatcher(keywords []string) Matcher {
	return NewMatcherWithExcludes(keywords, nil)
}

func NewMatcherWithExcludes(keywords []string, excludeKeywords []string) Matcher {
	normalized := make([]string, 0, len(keywords))
	for _, keyword := range keywords {
		keyword = normalize(keyword)
		if keyword == "" {
			continue
		}
		normalized = append(normalized, keyword)
	}

	normalizedExcludes := make([]string, 0, len(excludeKeywords))
	for _, keyword := range excludeKeywords {
		keyword = normalize(keyword)
		if keyword == "" {
			continue
		}
		normalizedExcludes = append(normalizedExcludes, keyword)
	}

	return Matcher{keywords: normalized, excludeKeywords: normalizedExcludes}
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
			if m.excluded(text) {
				return Match{}, false
			}
			return Match{Keyword: keyword}, true
		}
	}

	return Match{}, false
}

func (m Matcher) excluded(text string) bool {
	for _, keyword := range m.excludeKeywords {
		if strings.Contains(text, keyword) {
			return true
		}
	}

	return false
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
