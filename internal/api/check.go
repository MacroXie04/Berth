package api

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/MacroXie04/Berth/internal/auth"
)

// handleCheck is Traefik's ForwardAuth endpoint. Traefik calls it (always with
// GET) before forwarding any request to a workspace router, passing the
// original request's cookie and, in X-Forwarded-Uri, its URI. Traefik sets that
// header itself unless forwardAuth.trustForwardHeader is enabled, which must
// stay off: otherwise a client could claim any URI.
//
//	200  logged in as the owner of /ws/<username>/...
//	401  no valid session
//	403  valid session for a different user, or a path we cannot attribute
//
// Everything unexpected fails closed.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	user, err := s.workspaceUser(r)
	switch {
	case errors.Is(err, errNoSession):
		writeError(w, http.StatusUnauthorized, "not logged in")
		return
	case err != nil:
		s.Log.Error("session lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	owner, ok := workspaceOwner(r.Header.Get("X-Forwarded-Uri"))
	if !ok || owner != user.Username {
		writeError(w, http.StatusForbidden, "forbidden")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// maxForwardedURILen bounds the work done on an attacker-chosen header.
const maxForwardedURILen = 4096

// workspaceOwner returns the username a request URI addresses. The only
// accepted shape is /ws/<username>/(terminal|browser)[/anything-canonical].
//
// An encoded slash in the path is refused before decoding. The rest is
// percent-decoded, because that is the form Traefik routes on, and must then be
// canonical: no empty, "." or ".." segments, no backslashes or control
// characters (which also covers encoded backslashes and NULs). This closes path-confusion bypasses (for example
// /ws/alice/../bob/terminal, or a %2e%2e / %2f encoding of it) in which the
// router and this check could otherwise disagree about whose workspace is
// addressed. Anything that is not exactly the expected shape returns false.
func workspaceOwner(rawURI string) (string, bool) {
	if rawURI == "" || len(rawURI) > maxForwardedURILen || rawURI[0] != '/' {
		return "", false
	}
	// Refuse an encoded slash in the path outright: decoding it would silently
	// change the segmentation below, and the result should not depend on Traefik's
	// encodedcharacters entrypoint flags. (An encoded backslash or NUL needs no
	// special case: it decodes to a character the loop below rejects.)
	rawPath, _, _ := strings.Cut(rawURI, "?")
	if strings.Contains(strings.ToLower(rawPath), "%2f") {
		return "", false
	}
	u, err := url.ParseRequestURI(rawURI)
	if err != nil {
		return "", false
	}
	p := u.Path
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", false
		}
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	last := len(segs) - 1
	for i, seg := range segs {
		if seg == "." || seg == ".." || (seg == "" && i != last) {
			return "", false
		}
	}
	if len(segs) < 3 || segs[0] != "ws" || !auth.ValidUsername(segs[1]) {
		return "", false
	}
	if segs[2] != "terminal" && segs[2] != "browser" {
		return "", false
	}
	return segs[1], true
}
