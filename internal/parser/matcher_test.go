package parser

import "testing"

func TestMatcherMatch(t *testing.T) {
	matcher := NewMatcher([]string{"куплю"})

	match, ok := matcher.Match("Куплю рекламу")
	if !ok {
		t.Fatal("expected match")
	}
	if match.Keyword != "куплю" {
		t.Fatalf("keyword = %q, want %q", match.Keyword, "куплю")
	}
}

func TestMatcherNoMatch(t *testing.T) {
	matcher := NewMatcher([]string{"куплю"})

	if _, ok := matcher.Match("продам рекламу"); ok {
		t.Fatal("expected no match")
	}
}

func TestMatcherExcludeKeyword(t *testing.T) {
	matcher := NewMatcherWithExcludes([]string{"куплю"}, []string{"не куплю"})

	if _, ok := matcher.Match("не куплю рекламу"); ok {
		t.Fatal("expected excluded message to not match")
	}
}

func TestMatcherExcludeKeywordCaseInsensitive(t *testing.T) {
	matcher := NewMatcherWithExcludes([]string{"КУПЛЮ"}, []string{"НЕ КУПЛЮ"})

	if _, ok := matcher.Match("Не куплю рекламу"); ok {
		t.Fatal("expected excluded message to not match")
	}
}

func TestMatcherExcludeKeywordNormalizesYo(t *testing.T) {
	matcher := NewMatcherWithExcludes([]string{"беру"}, []string{"не берём"})

	if _, ok := matcher.Match("не берем рекламу"); ok {
		t.Fatal("expected excluded message to not match")
	}
}
