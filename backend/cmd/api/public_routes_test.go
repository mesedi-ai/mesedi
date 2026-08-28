// Every route reachable WITHOUT authentication must be listed here.
//
// WHY THIS TEST EXISTS
// foundation-audit check B14 ("new HTTP handler contains an auth
// check") fired correctly on GET /ready, which is deliberately
// unauthenticated: an external uptime monitor holds no credentials.
// The tool offers a skip list for that, but skipping B14 would disable
// auth checking for every handler added from then on, in an untracked
// file under .git/, invisible to anyone reading the repository.
//
// That is the same failure shape as everything else found on
// 2026-08-27: a requirement that lives only in whoever remembers it.
// Eight destructive handlers had already drifted out of audit coverage
// exactly that way.
//
// So B14 is skipped and replaced by this, which is stricter than B14
// was. B14 looked for an auth-shaped string near a new handler. This
// enumerates the complete set of routes that bypass authentication and
// fails the build when that set changes. Adding a public endpoint is
// now a deliberate, reviewable act in the public repo rather than a
// line nobody notices.
//
// The routing tree has three handler groups (see cmd/api/main.go):
//
//	privateHandler  wrapped in api.RequireAuth   bearer token required
//	adminHandler    wrapped in api.AdminAuth     admin token required
//	everything else                              NO authentication
//
// This test cares only about the third group.
//
// If you are here because the test failed: do not delete the entry or
// widen the pattern. Add the route with an honest reason, or move it
// behind privateHandler.

package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// authenticatedHandlers are the two handler variables that carry an
// auth middleware. Any route bound to something else is public.
var authenticatedHandlers = map[string]bool{
	"privateHandler": true,
	"adminHandler":   true,
}

// publicRoutes is the complete unauthenticated surface of the API.
// Every entry needs a reason, because "add it to the list" is exactly
// how this test would be quietly defeated.
//
// OPTIONS routes are handled by a blanket rule below rather than
// enumerated: a CORS preflight carries no body, performs no action and
// returns no tenant data, so requiring auth on it would simply break
// browsers. That exemption is about the METHOD, not about any
// particular path.
var publicRoutes = map[string]string{
	"GET /health": "Liveness probe. Polled by Fly's machine health check every 30s " +
		"(see fly.toml). Returns service name, version and time. No database access, " +
		"no tenant data. Deliberately cannot fail; see cmd/api/ready.go.",

	"GET /ready": "Readiness probe. Polled by external uptime monitoring, which holds " +
		"no credentials, so it cannot be authenticated. Returns fixed check strings, a " +
		"fixed reason code and migration counts only. Driver errors never reach the body " +
		"and a test asserts that. Result is cached 5s so it cannot amplify load onto our " +
		"own database.",

	"GET /ui/": "Local-development dashboard served from files embedded in the binary. " +
		"Static assets only. NOT the production dashboard, which is a separate " +
		"deployment; see internal/dashboard/dashboard.go.",

	"POST /signup": "Account creation. Unauthenticated by definition: there is no account " +
		"to authenticate as yet. Protected by signup rate limiting and email verification.",

	"POST /signin": "Server-to-server endpoint called by the dashboard server, which holds " +
		"no customer key. Guarded by the MESEDI_SIGNIN_SECRET shared secret rather than a " +
		"bearer token.",

	"POST /magic-link/start": "Requests a sign-in link. Unauthenticated by definition; " +
		"the caller is trying to become authenticated. Rate limited.",

	"GET /magic-link/verify": "Consumes a sign-in link. The single-use token in the URL " +
		"IS the credential, so bearer auth cannot apply.",

	"POST /auth/logout": "Clears the session. Must work even when the session is already " +
		"invalid, which is precisely when auth would reject it.",

	"POST /auth/2fa-verify": "Second factor, submitted mid-login. The user is by definition " +
		"not yet fully authenticated at this point.",

	"POST /api/email-verify/confirm": "Consumes an email-verification token. The token in " +
		"the request IS the credential.",

	"POST /api/email-verify/resend": "Resends verification mail before the account is " +
		"verified. Rate limited.",

	"POST /billing/webhook": "Inbound Stripe webhook. Stripe cannot present our bearer " +
		"token. Authenticity is established by Stripe signature verification instead, " +
		"which foundation-audit check E3 enforces separately.",

	"GET /invites/{token}": "Reads a pending organization invite. The invite token IS the " +
		"credential, and the recipient has no account yet.",

	"POST /invites/{token}/accept": "Accepts an organization invite. Same reasoning: the " +
		"token is the credential.",
}

var routeRe = regexp.MustCompile(`mux\.Handle\("([A-Z]+) ([^"]+)",\s*(\w+)\)`)

func TestEveryUnauthenticatedRouteIsExplicitlyListed(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	matches := routeRe.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		// A regex that silently matches nothing would make this test
		// pass forever while checking nothing, which is the same class
		// of bug it exists to prevent.
		t.Fatal("matched zero routes in main.go; the route regex is broken and this " +
			"test is not actually checking anything")
	}

	var undocumented []string
	publicSeen := map[string]bool{}
	checked := 0

	for _, m := range matches {
		method, path, handler := m[1], m[2], m[3]
		if authenticatedHandlers[handler] {
			continue
		}
		checked++
		if method == "OPTIONS" {
			continue // CORS preflight; see the note on publicRoutes.
		}

		key := method + " " + path
		publicSeen[key] = true
		if reason, ok := publicRoutes[key]; !ok {
			undocumented = append(undocumented, key+"  → bound to "+handler)
		} else if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is listed with an empty reason", key)
		}
	}

	if checked == 0 {
		t.Fatal("found no unauthenticated routes at all; /health alone should match, " +
			"so the handler-name detection is broken")
	}

	if len(undocumented) > 0 {
		t.Errorf(
			"%d route(s) are reachable WITHOUT authentication and are not listed in "+
				"publicRoutes:\n  %s\n\n"+
				"Either move the route behind privateHandler, or add it to publicRoutes "+
				"with an honest reason explaining why it is safe to expose.\n\n"+
				"This is not a formality. Every entry in that map is an endpoint any "+
				"stranger on the internet can call.",
			len(undocumented), strings.Join(undocumented, "\n  "),
		)
	}

	// The list must not rot in the other direction either. A stale
	// entry for a route that no longer exists makes the surface look
	// larger than it is and trains the reader to skim.
	for key := range publicRoutes {
		if !publicSeen[key] {
			t.Errorf("publicRoutes lists %q but main.go no longer registers it; "+
				"remove the stale entry", key)
		}
	}
}

// TestReadyIsPublicAndHealthIsPublic pins the specific pair this whole
// file was written around, so a later refactor that accidentally moves
// either behind auth fails loudly rather than silently blinding the
// uptime monitors again.
func TestProbeEndpointsStayUnauthenticated(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"GET /health", "GET /ready"} {
		if _, ok := publicRoutes[key]; !ok {
			t.Errorf("%s is no longer public. An uptime monitor holds no credentials, "+
				"so putting a probe behind auth makes it permanently red.", key)
		}
	}
}
