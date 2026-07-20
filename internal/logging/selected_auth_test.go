package logging

import (
	"context"
	"testing"
)

func TestSelectedAuthHolderRoundTrip(t *testing.T) {
	ctx := WithSelectedAuthHolder(context.Background())
	if !HasSelectedAuthHolder(ctx) {
		t.Fatal("holder not installed")
	}
	if got := GetSelectedAuth(ctx); got != "" {
		t.Fatalf("fresh holder = %q, want empty", got)
	}
	SetSelectedAuth(ctx, "  claude-marie@pyramind.ai.json  ")
	if got := GetSelectedAuth(ctx); got != "claude-marie@pyramind.ai.json" {
		t.Fatalf("GetSelectedAuth = %q, want trimmed auth id", got)
	}
}

func TestSelectedAuthNoHolderIsNoop(t *testing.T) {
	ctx := context.Background()
	if HasSelectedAuthHolder(ctx) {
		t.Fatal("bare context should have no holder")
	}
	SetSelectedAuth(ctx, "claude-x@y.z.json") // must not panic
	if got := GetSelectedAuth(ctx); got != "" {
		t.Fatalf("GetSelectedAuth without holder = %q, want empty", got)
	}
}

// The middleware installs the holder on the request context; GetContextWithCancel
// bridges it into the separate execution context. A write during execution (dst)
// must be visible to the middleware reading the request context (src): same
// pointer.
func TestSelectedAuthHolderIntoSharesPointer(t *testing.T) {
	src := WithSelectedAuthHolder(context.Background())
	dst := SelectedAuthHolderInto(src, context.Background())
	if !HasSelectedAuthHolder(dst) {
		t.Fatal("holder not bridged into dst")
	}
	SetSelectedAuth(dst, "codex-sol@epk.dev.json")
	if got := GetSelectedAuth(src); got != "codex-sol@epk.dev.json" {
		t.Fatalf("src did not observe dst write: %q", got)
	}
}

func TestSelectedAuthHolderIntoNoSourceHolder(t *testing.T) {
	dst := SelectedAuthHolderInto(context.Background(), context.Background())
	if HasSelectedAuthHolder(dst) {
		t.Fatal("dst should have no holder when src has none")
	}
}

func TestCredSlugAndEmail(t *testing.T) {
	cases := []struct{ in, slug, email string }{
		{"claude-marie@pyramind.ai.json", "marie@pyramind.ai", "marie@pyramind.ai"},
		{"codex-bryce@pyramind.ai.json", "bryce@pyramind.ai", "bryce@pyramind.ai"},
		{"/home/x/.epkv2/fable-proxy/auths/claude-a@b.co.json", "a@b.co", "a@b.co"},
		{"aa-primary", "aa-primary", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := credSlug(c.in); got != c.slug {
			t.Errorf("credSlug(%q) = %q, want %q", c.in, got, c.slug)
		}
		if got := credEmail(c.in); got != c.email {
			t.Errorf("credEmail(%q) = %q, want %q", c.in, got, c.email)
		}
	}
}
