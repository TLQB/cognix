// APK-protocol transport for chat.qwen.ai.
//
// Research 2026-09-12 (~/qwen-waf-probe/quotakey-apk + goreplay): the Aliyun
// WAF fronts chat.qwen.ai fingerprints the TLS handshake, not just headers.
// Same request bytes: Python/OpenSSL TLS reaches the app layer (RateLimited
// on a capped token), Go-stdlib TLS gets the captcha challenge page on BOTH
// /api/v2/chats/new and completions. An early APK-header probe with stdlib
// TLS appeared to pass, but the A/B replay proved that lucky IP-state, not a
// bypass — uTLS is mandatory in every mode. QWEN_TRANSPORT=apk therefore only
// swaps the HTTP header set to the Android app's (Dalvik/Cronet UA,
// X-Platform: android, source: app, per-request x-device-id) on top of the
// uTLS transport, HTTP/1.1 (Cronet's h2 fingerprint over uTLS is unverified).
// The app authenticates with the WEB account JWT (Cookie: token=<JWT>);
// app_waf requires a separate app login and is not used here.
package zbridge

import (
	"net/http"
	"os"
	"strings"
	"time"
)

// qwenAPKUserAgent mirrors the official Android app's Cronet stack with the
// AliApp build tag. The app version has not been observed to matter for the
// WAF check; keep it plausible and current.
const qwenAPKUserAgent = "Dalvik/2.1.0 (Linux; U; Android 14; zh-CN; Pixel 8; Build/UQ1A.240205.002) AliApp(QWENCHAT/2.5.1) Cronet/119.0.6045.31"

// apkTransportEnabled reports whether the APK protocol transport is active.
func apkTransportEnabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("QWEN_TRANSPORT")), "apk")
}

// apkDeviceID is the per-install device id, sent when
// QWEN_APK_STABLE_DEVICE_ID=1 pins it. Default is per-request rotation,
// matching the Android app's Cronet stack (each RPC carries a fresh
// install-derived nonce). Probe 2026-09-12: both rotation styles reach the
// app layer; the CHAT_NOT_FOUND failures back then were sync-mode
// client-minted chat ids (see AcquireStatelessSession), not device ids.
var apkDeviceID = randomUUID()

// apkNextDeviceID returns the device id for the next request.
func apkNextDeviceID() string {
	if os.Getenv("QWEN_APK_STABLE_DEVICE_ID") == "1" {
		return apkDeviceID
	}
	return randomUUID()
}

// applyQwenTransportHeaders specialises setQwenHeaders for APK mode: the
// app sends the Dalvik UA, X-Platform/source/x-device-id, and does NOT send
// the web-only headers (Version, Referer, X-Accel-Buffering, Connection).
// Called from setQwenHeaders after the common fields are set.
func applyQwenTransportHeaders(req *http.Request, feVersion, token string) {
	if !apkTransportEnabled() {
		return // web mode: headers already correct
	}
	req.Header.Set("User-Agent", qwenAPKUserAgent)
	req.Header.Set("X-Platform", "android")
	req.Header.Set("source", "app")
	req.Header.Set("x-device-id", apkNextDeviceID())
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	// Web-only headers the app never sends.
	req.Header.Del("Version")
	req.Header.Del("X-Accel-Buffering")
	req.Header.Del("Connection")
	// Referer is a web concept; the completions call sets it explicitly, so
	// delete it there — this covers the shared-path calls (configs, models,
	// chats/new, chat delete).
	if req.Header.Get("Referer") != "" {
		req.Header.Del("Referer")
	}
}

// newQwenHTTPClientForMode builds the chat.qwen.ai client for the active
// transport mode:
//   - apk  : uTLS ClientHello with the Android app header set
//     (QWEN_TRANSPORT=apk), HTTP/1.1, no cookie warm-up.
//   - web  : uTLS Chrome-120 ClientHello over HTTP/1.1 (legacy default).
//
// Both are paced by newPacedTransport with the shared upstream interval.
func newQwenHTTPClientForMode() *http.Client {
	base := &http.Transport{
		DialTLSContext:        dialUTLS, // mandatory in every mode — see header note
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false,
	}

	// No cookie jar in either mode: the auth JWT rides the explicit Cookie
	// header (set by setQwenHeaders), and net/http ADD-COOKIES the jar's
	// entries onto it. When any earlier response (models fetch, configs
	// ping) set cookies, the merged Cookie header carries a second token
	// value and Qwen serves the request as the wrong account — reproducible
	// CHAT_NOT_FOUND on a freshly minted chat (2026-09-12). The app keeps
	// its session purely in the token cookie; a jar buys nothing.
	return &http.Client{
		Transport: newPacedTransport(base),
		Jar:       nil,
	}
}
