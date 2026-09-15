package handlers

import (
	"fmt"
	"net/http"
)

// ============================================
// PROBE RELEASE RESPONDER (iOS/Android/Windows captive release)
// ============================================
//
// While a client is captive, the per-portal dnsmasq answers every name
// with the gateway IP. iOS in particular CACHES that hijacked resolution
// for captive.apple.com. After payment, aircoins-captive-rules auth
// sends fresh DNS to a real resolver and RETURNs the client's traffic —
// but the iPhone keeps re-probing the CACHED GATEWAY IP, which lands on
// lighttpd and gets 302-redirected back to the portal. iOS reads that
// as "still captive" and never closes the Captive Network Assistant,
// even though the session is valid. Android escapes because it
// re-resolves DNS and probes the real internet.
//
// The fix lives at layer 3 + layer 7:
//
//  1. cmd_auth (aircoins-captive-rules) inserts per-MAC nat rules that
//     REDIRECT an authorized client's probe GETs destined for a LOCAL
//     address (the cached gateway IP) to the API port below.
//  2. ProbeRelease answers those redirected requests with the EXACT
//     success response each OS expects, so the captive state clears.
//
// These paths are registered directly on the mux (no /api prefix) and
// are only reachable through the REDIRECT rules above — a client cannot
// reach them without already being authorized by MAC. The API listens on
// all interfaces, so the REDIRECT (which rewrites the destination to the
// gateway IP, not 127.0.0.1) lands here.

// ProbeRelease answers OS connectivity probes with the exact content the
// probing OS expects when the network has no captive portal. Mirrors the
// probe path list in lighttpd.conf (which 302s the same paths to the
// portal for UNAUTHORIZED clients to trigger the popup).
func ProbeRelease(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	// Android / Samsung — expect an empty HTTP 204
	case "/generate_204", "/gen_204", "/generate204", "/mobile/status.php",
		"/kindle-wifi/wifistub.html":
		w.WriteHeader(http.StatusNoContent)

	// Windows NCSI — expect exact plaintext markers
	case "/connecttest.txt":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "Microsoft Connect Test")
	case "/ncsi.txt":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "Microsoft NCSI")

	// Firefox / NetworkManager / Ubuntu
	case "/success.txt":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "success")
	case "/nm-check.txt", "/check_network_status.txt":
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "NetworkManager is online")

	// Apple (hotspot-detect.html, /library/test/success.html,
	// /canonical.html) and anything else — expect the "Success" page.
	default:
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<HTML><HEAD><TITLE>Success</TITLE></HEAD><BODY>Success</BODY></HTML>")
	}
}

// ProbeReleasePaths lists every probe path the release responder serves.
// aircoins-captive-rules uses the same list for its per-MAC REDIRECT
// rules; keep both in sync (and with lighttpd.conf's redirect list).
var ProbeReleasePaths = []string{
	"/hotspot-detect.html",
	"/library/test/success.html",
	"/canonical.html",
	"/generate_204",
	"/gen_204",
	"/generate204",
	"/mobile/status.php",
	"/connecttest.txt",
	"/ncsi.txt",
	"/success.txt",
	"/nm-check.txt",
	"/check_network_status.txt",
	"/kindle-wifi/wifistub.html",
}
