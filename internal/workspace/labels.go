package workspace

import (
	"github.com/MacroXie04/Berth/internal/docker"
)

// Ports the workspace image listens on.
const (
	TerminalPort = 7681 // ttyd
	BrowserPort  = 8080 // neko
)

// AuthMiddleware is the shared ForwardAuth middleware, defined in
// traefik/dynamic.yml (file provider) so it exists even while the backend is
// down.
const AuthMiddleware = "berth-auth@file"

// WSHeadersMiddleware is the response-header middleware for everything under
// /ws/, defined next to the auth middleware in traefik/dynamic.yml: it forbids
// framing by any other origin and removes Service-Worker-Allowed, so a workspace
// cannot widen a service worker's scope to the whole site.
const WSHeadersMiddleware = "berth-ws-headers@file"

// PathPrefix is the URL prefix a workspace section is served under.
func PathPrefix(username, section string) string { return "/ws/" + username + "/" + section }

// Labels returns the labels for a user's container: Berth's ownership labels
// and the Traefik routing for the two sections.
//
//	/ws/<u>/terminal -> ttyd, prefix stripped (ttyd serves from "/")
//	/ws/<u>/browser  -> neko, NOT stripped (neko is told its own prefix)
//
// Both routers pass through ForwardAuth first, then the response-header
// middleware, then (terminal only) StripPrefix. The order in `middlewares` is
// load-bearing: Traefik gives the auth check the request URI as it is when the
// middleware runs, so putting StripPrefix first would show the check a bare "/"
// and every request would be refused. The header middleware only edits responses,
// so its place relative to auth decides one thing: a response ForwardAuth itself
// writes (a 401 or 403 with its small JSON body) never reaches it.
//
// username must already be validated (auth.ValidUsername); it is interpolated
// into router and middleware names.
func Labels(username, network, entrypoint string) map[string]string {
	term, browser, strip := username+"-term", username+"-browser", username+"-term-strip"
	termPath, browserPath := PathPrefix(username, "terminal"), PathPrefix(username, "browser")

	return map[string]string{
		docker.LabelManaged: "true",
		docker.LabelUser:    username,

		"traefik.enable":         "true",
		"traefik.docker.network": network,

		"traefik.http.routers." + term + ".entrypoints":               entrypoint,
		"traefik.http.routers." + term + ".rule":                      "PathPrefix(`" + termPath + "`)",
		"traefik.http.routers." + term + ".middlewares":               AuthMiddleware + "," + WSHeadersMiddleware + "," + strip,
		"traefik.http.routers." + term + ".service":                   term,
		"traefik.http.middlewares." + strip + ".stripprefix.prefixes": termPath,
		"traefik.http.services." + term + ".loadbalancer.server.port": itoa(TerminalPort),

		"traefik.http.routers." + browser + ".entrypoints":               entrypoint,
		"traefik.http.routers." + browser + ".rule":                      "PathPrefix(`" + browserPath + "`)",
		"traefik.http.routers." + browser + ".middlewares":               AuthMiddleware + "," + WSHeadersMiddleware,
		"traefik.http.routers." + browser + ".service":                   browser,
		"traefik.http.services." + browser + ".loadbalancer.server.port": itoa(BrowserPort),
	}
}
