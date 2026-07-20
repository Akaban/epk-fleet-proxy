package auth

// identity.go — shared credential-identity helpers for fleet telemetry.
// The founder law (2026-07-20): every proxy.event carries the selected
// credential's auth_id/account/email so per-account attribution is possible on
// EVERY event kind, not only successful llm.calls. None of these values is a
// secret — the account/email is already the on-disk auth-file identity.

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

// firstEmail returns the first parseable email from the candidates (e.g. the
// exact auth-id, then the account slug), or "".
func firstEmail(candidates ...string) string {
	for _, c := range candidates {
		if e := credEmail(c); e != "" {
			return e
		}
	}
	return ""
}
