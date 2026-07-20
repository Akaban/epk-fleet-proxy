package logging

// credid.go — credential-identity helpers for the minimal per-request proxy.event.
//
// These MIRROR sdk/cliproxy/auth/identity.go (credSlug/credEmail). They are
// duplicated here deliberately: sdk/cliproxy/auth imports internal/logging, so
// this package cannot import auth without an import cycle. The logic is trivial
// and stable (strip the auth-file prefix to a human slug; pull the login email).
// None of these values is a secret — the account/email is the on-disk auth-file
// identity, already carried on llm.call / error / gateway events. Keep in sync.

import (
	"regexp"
	"strings"
)

var credEmailPattern = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// credSlug strips an auth-id / auth-file id down to its human account slug:
// "claude-marie@pyramind.ai.json" -> "marie@pyramind.ai". A bare slug passes
// through unchanged.
func credSlug(authID string) string {
	s := strings.TrimSpace(authID)
	if s == "" {
		return ""
	}
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ".json")
	for _, p := range []string{"claude-", "codex-", "gemini-", "openai-", "qwen-", "kimi-", "antigravity-"} {
		if strings.HasPrefix(s, p) {
			s = s[len(p):]
			break
		}
	}
	return s
}

// credEmail extracts the login email embedded in an auth-id or account slug,
// or "" when none is present.
func credEmail(s string) string {
	return credEmailPattern.FindString(credSlug(s))
}
