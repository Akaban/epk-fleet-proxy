package auth

import "testing"

func TestCredSlugAndEmail(t *testing.T) {
	cases := []struct {
		in        string
		wantSlug  string
		wantEmail string
	}{
		{"claude-marie@pyramind.ai.json", "marie@pyramind.ai", "marie@pyramind.ai"},
		{"codex-bryce@godfather.dev-pro.json", "bryce@godfather.dev-pro", "bryce@godfather.dev"},
		{"marie@pyramind.ai", "marie@pyramind.ai", "marie@pyramind.ai"},
		{"/abs/path/claude-yohan@pyramind.ai.json", "yohan@pyramind.ai", "yohan@pyramind.ai"},
		{"", "", ""},
		{"no-email-here", "no-email-here", ""},
	}
	for _, c := range cases {
		if got := credSlug(c.in); got != c.wantSlug {
			t.Errorf("credSlug(%q) = %q, want %q", c.in, got, c.wantSlug)
		}
		if got := credEmail(c.in); got != c.wantEmail {
			t.Errorf("credEmail(%q) = %q, want %q", c.in, got, c.wantEmail)
		}
	}
}

func TestFirstEmailPrefersFirstParseable(t *testing.T) {
	if got := firstEmail("no-email", "claude-marie@pyramind.ai.json"); got != "marie@pyramind.ai" {
		t.Fatalf("firstEmail fallback = %q, want marie@pyramind.ai", got)
	}
	if got := firstEmail("", ""); got != "" {
		t.Fatalf("firstEmail(empty) = %q, want empty", got)
	}
}

func TestProviderNotFoundStatusIs4xx(t *testing.T) {
	// provider_not_found is a client/config error: it must floor to 4xx, never
	// default to 5xx (the founder/commander-7 order, 2026-07-20).
	if got := (&Error{Code: "provider_not_found"}).StatusCode(); got != 400 {
		t.Fatalf("provider_not_found StatusCode() = %d, want 400", got)
	}
	// An explicit status is preserved (only the unset-0 case is floored).
	if got := (&Error{Code: "provider_not_found", HTTPStatus: 404}).StatusCode(); got != 404 {
		t.Fatalf("explicit HTTPStatus overridden = %d, want 404", got)
	}
}
