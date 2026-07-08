package bot

import "testing"

func TestHighlightKeywordsHTMLHighlightsAllKeywords(t *testing.T) {
	got := highlightKeywordsHTML("Куплю рекламу, продам канал", []string{"куплю", "продам"})
	want := "<b>КУПЛЮ</b> рекламу, <b>ПРОДАМ</b> канал"
	if got != want {
		t.Fatalf("highlight = %q, want %q", got, want)
	}
}

func TestHighlightKeywordsHTMLCaseInsensitive(t *testing.T) {
	got := highlightKeywordsHTML("КУПЛЮ рекламу", []string{"куплю"})
	want := "<b>КУПЛЮ</b> рекламу"
	if got != want {
		t.Fatalf("highlight = %q, want %q", got, want)
	}
}

func TestHighlightKeywordsHTMLNormalizesYo(t *testing.T) {
	got := highlightKeywordsHTML("Берём рекламу", []string{"берем"})
	want := "<b>БЕРЁМ</b> рекламу"
	if got != want {
		t.Fatalf("highlight = %q, want %q", got, want)
	}
}

func TestHighlightKeywordsHTMLExpandsToFullWord(t *testing.T) {
	got := highlightKeywordsHTML("услуги по организации клининга", []string{"клининг"})
	want := "услуги по организации <b>КЛИНИНГА</b>"
	if got != want {
		t.Fatalf("highlight = %q, want %q", got, want)
	}
}

func TestHighlightKeywordsHTMLEscapesText(t *testing.T) {
	got := highlightKeywordsHTML("куплю <рекламу> & трафик", []string{"куплю", "трафик"})
	want := "<b>КУПЛЮ</b> &lt;рекламу&gt; &amp; <b>ТРАФИК</b>"
	if got != want {
		t.Fatalf("highlight = %q, want %q", got, want)
	}
}

func TestHighlightKeywordsHTMLPrefersLongerOverlap(t *testing.T) {
	got := highlightKeywordsHTML("куплю рекламу", []string{"куплю", "куплю рекламу"})
	want := "<b>КУПЛЮ РЕКЛАМУ</b>"
	if got != want {
		t.Fatalf("highlight = %q, want %q", got, want)
	}
}

func TestHighlightKeywordsHTMLReturnsEscapedTextWithoutMatches(t *testing.T) {
	got := highlightKeywordsHTML("продам <канал>", []string{"куплю"})
	want := "продам &lt;канал&gt;"
	if got != want {
		t.Fatalf("highlight = %q, want %q", got, want)
	}
}
