package api

import (
	"strings"
	"testing"
)

func TestWorkspaceOwnerAccepts(t *testing.T) {
	cases := map[string]string{
		"/ws/alice/terminal":                                "alice",
		"/ws/alice/terminal/":                               "alice",
		"/ws/alice/terminal/ws":                             "alice",
		"/ws/alice/terminal/ws?x=1&y=/../":                  "alice", // query is not part of the path
		"/ws/alice/terminal/ws?next=%2Fws%2Fbob%2Fterminal": "alice", // encoded slashes in the query are harmless
		"/ws/alice/browser":                                 "alice",
		"/ws/alice/browser/api/room?token=abc":              "alice",
		"/ws/a-b-c/terminal":                                "a-b-c",
		"/ws/user42/browser/static/a%20b.js":                "user42",
		"/ws/" + strings.Repeat("a", 32) + "/terminal":      strings.Repeat("a", 32),
	}
	for uri, want := range cases {
		got, ok := workspaceOwner(uri)
		if !ok || got != want {
			t.Errorf("workspaceOwner(%q) = %q, %v; want %q, true", uri, got, ok, want)
		}
	}
}

// Every entry here is an attempt to make the ForwardAuth check attribute a
// request to the wrong user, or to a non-workspace path. All must be refused.
func TestWorkspaceOwnerRejects(t *testing.T) {
	long := "/ws/alice/terminal/" + strings.Repeat("a", maxForwardedURILen)
	cases := map[string]string{
		// shape
		"empty":                "",
		"relative":             "ws/alice/terminal",
		"root":                 "/",
		"ws only":              "/ws",
		"ws slash":             "/ws/",
		"no section":           "/ws/alice",
		"empty section":        "/ws/alice/",
		"unknown section":      "/ws/alice/other",
		"section prefix match": "/ws/alice/terminalx",
		"section suffix match": "/ws/alice/xterminal",
		"uppercase prefix":     "/WS/alice/terminal",
		"wrong root":           "/api/alice/terminal",
		"absolute form":        "http://evil.example/ws/alice/terminal",
		"scheme-relative":      "//ws/alice/terminal",
		"fragment":             "/ws/alice/terminal#frag",
		"too long":             long,
		// empty / dot segments, raw and encoded
		"empty username segment":     "/ws//terminal",
		"double slash":               "/ws/alice//terminal",
		"double slash after section": "/ws/alice/terminal//x", // canonical form only, even where attribution is unaffected
		"trailing double slash":      "/ws/alice/terminal/x//",
		"double slash before ws":     "//ws/alice/terminal",
		"dot segment":                "/ws/alice/./terminal",
		"dotdot to other user":       "/ws/alice/../bob/terminal",
		"dotdot after section":       "/ws/alice/terminal/../../bob/terminal",
		"encoded dotdot":             "/ws/alice/%2e%2e/bob/terminal",
		"encoded dotdot upper":       "/ws/alice/%2E%2E/bob/terminal",
		"half-encoded dotdot":        "/ws/alice/.%2e/bob/terminal",
		"encoded slash dotdot":       "/ws/alice%2F..%2Fbob/terminal",
		"encoded slash dotdot 2":     "/ws/alice/terminal%2F..%2F..%2Fbob/terminal",
		"encoded dot":                "/ws/alice/%2e/terminal",
		// username abuse
		"uppercase user":       "/ws/Alice/terminal",
		"short user":           "/ws/al/terminal",
		"leading hyphen":       "/ws/-alice/terminal",
		"trailing hyphen":      "/ws/alice-/terminal",
		"double hyphen":        "/ws/ali--ce/terminal",
		"semicolon param":      "/ws/alice;x=1/terminal",
		"dot in user":          "/ws/al.ice/terminal",
		"underscore user":      "/ws/al_ice/terminal",
		"unicode homoglyph":    "/ws/аlice/terminal", // Cyrillic а
		"percent-encoded user": "/ws/%61lice%00/terminal",
		"space in user":        "/ws/ali%20ce/terminal",
		"over-long user":       "/ws/" + strings.Repeat("a", 33) + "/terminal",
		// control characters and backslashes
		"nul":                         "/ws/alice/terminal\x00",
		"encoded nul":                 "/ws/alice/terminal/%00",
		"newline":                     "/ws/alice/terminal\n/ws/bob/terminal",
		"encoded newline":             "/ws/alice/terminal/%0a",
		"tab":                         "/ws/alice/term\tinal",
		"backslash":                   "/ws/alice\\..\\bob/terminal",
		"backslash after section":     "/ws/alice/terminal/a\\b",
		"encoded slash in user":       "/ws/alice%2Fterminal/x", // decodes to a valid alice path; refused outright
		"encoded slash after section": "/ws/alice/terminal%2fx",
		"encoded slash upper":         "/ws/alice/terminal/%2F",
		"encoded backslash after":     "/ws/alice/terminal/a%5cb",
		"encoded backslash lower":     "/ws/alice/terminal/a%5Cb",
		"encoded backslash":           "/ws/alice%5C..%5Cbob/terminal",
		"delete char":                 "/ws/alice/terminal/\x7f",
		"invalid percent":             "/ws/alice/terminal/%zz",
	}
	for name, uri := range cases {
		if got, ok := workspaceOwner(uri); ok {
			t.Errorf("%s: workspaceOwner(%q) accepted as %q", name, uri, got)
		}
	}
}
