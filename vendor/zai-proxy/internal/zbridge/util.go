// Code moved from the original main.go monolith during the internal/ restructure.
// See README "Project Structure". Part of the Z.AI bridge core (package zbridge).

package zbridge

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// ============================================================================
// INITIALIZATION
// ============================================================================

func init() {
	// Initialise URL safe-character table for custom URL encoder
	for i := 0; i < 256; i++ {
		c := byte(i)
		if (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			baseSafeTable[i] = true
		}
	}
}

// ============================================================================
// LOGGING — silent unless --verbose
// ============================================================================

// logError / logInfo are silent unless --verbose. logOperational is for
// messages an operator MUST see even without --verbose (token source dead,
// breaker tripped, reserve empty) — without it a total captcha outage shows
// up only as silent "empty payload" errors, exactly like the 2026-09-05
// incident where a dead Q + empty reserve was invisible in bridge.log.
func logOperational(msg string) {
	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	logMu.Lock()
	fmt.Fprintf(os.Stderr, "[%s] OPS: %s\n", ts, msg)
	logMu.Unlock()
}

func logError(msg string) {
	if !verbose {
		return
	}
	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	logMu.Lock()
	fmt.Fprintf(os.Stderr, "[%s] ERROR: %s\n", ts, msg)
	logMu.Unlock()
}

func logInfo(msg string) {
	if !verbose {
		return
	}
	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	logMu.Lock()
	fmt.Fprintf(os.Stderr, "[%s] INFO: %s\n", ts, msg)
	logMu.Unlock()
}

// shortTokenForLog keeps captcha/device-token diagnostics identifiable
// (first 16 chars) without dumping the full multi-KB token into logs —
// full tokens made log lines so long they were hard to read and a leak
// surface when logs are shared for debugging.
func shortTokenForLog(tok string) string {
	const n = 16
	if len(tok) <= n {
		return tok
	}
	return tok[:n] + "…"
}

// ============================================================================
// BUFFER POOLS — eliminate GC pressure on hot paths
// ============================================================================

var bufPool = sync.Pool{
	New: func() interface{} { return bytes.NewBuffer(make([]byte, 0, 4096)) },
}

var zlibWriterPool = sync.Pool{
	New: func() interface{} {
		w, _ := zlib.NewWriterLevel(io.Discard, zlib.DefaultCompression)
		return w
	},
}

// ============================================================================
// HTTP CLIENTS — pooled connections, HTTP/2, keep-alive
// ============================================================================

// Optimised client for Aliyun captcha API calls
// No pacedTransport: the captcha API is on a separate Aliyun endpoint that
// does not share the chat.z.ai WAF rate-limit identity, so pacing it only
// adds latency (~300ms per captcha challenge) without any WAF benefit.
var aliyunHTTPClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ForceAttemptHTTP2:     true,
	},
	Timeout: 30 * time.Second,
}

// TLS FINGERPRINT SPOOFING — uTLS with Chrome ClientHello
// Aliyun ESA WAF does JA3 fingerprinting; Go's default TLS is blocked.
// ============================================================================

// ---------------------------------------------------------------------------
// Aliyun ALB geo-bypass - 405 on /api/v2/chat/completions from VN client IPs
// ---------------------------------------------------------------------------
// The Aliyun ESA edge geo-blocks chat completions by the real client IP, but
// the Aliyun ALB that also fronts chat.z.ai trusts X-Forwarded-For for its
// geo decision. Dialing the ALB IP directly (resolved via z.ai's CNAME) while
// keeping TLS SNI/Host = chat.z.ai, plus a foreign XFF header (added in
// sendToZAIStream), lets blocked regions reach the origin. Disable with
// ZAI_ALB_BYPASS=0 (e.g. when already egressing through a foreign proxy/WARP).

// zaiProdHost is the production Z.AI chat hostname. The ALB geo-bypass only
// rewrites name resolution for this host (never for mock/test upstreams).
const zaiProdHost = "chat.z.ai"

func albBypassEnabled() bool {
	switch strings.ToLower(os.Getenv("ZAI_ALB_BYPASS")) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// defaultXffIPPool is the built-in X-Forwarded-For rotation pool, retained
// as the fallback strategy (ZAI_XFF_STRATEGY=pool) and for operator-pinned
// deployments. Aliyun's WAF blocklists heavily-used spoofed addresses — the
// entire previous pool (34.120.45.67, 52.94.181.23, 151.101.1.140, 203.104.209.71,
// 1.1.1.1) was blocklisted whole on 2026-09-01 — so the pool is a larger
// candidate set of well-known foreign anycast IPs spread across distinct
// ranges. See defaultXffCIDRBanks for why random-bank is now the default.
var defaultXffIPPool = []string{
	// Cloudflare
	"1.0.0.1",
	"1.1.1.1",
	// Quad9
	"9.9.9.9",
	"9.9.9.10",
	"149.112.112.112",
	// OpenDNS / Cisco Umbrella
	"208.67.222.222",
	"208.67.220.220",
	"208.67.222.220",
	"208.67.220.222",
	// Verisign Public DNS
	"64.6.64.6",
	"64.6.65.6",
	// Neustar UltraDNS
	"156.154.70.1",
	"156.154.71.1",
	// Cloudflare scan / GitHub Pages anycast
	"185.199.108.153",
	"185.199.109.153",
	"185.199.110.153",
	"185.199.111.153",
	// Microsoft / Azure anycast (msftncsi)
	"13.107.42.14",
	"13.107.42.15",
	// Akamai anycast
	"23.195.132.79",
	"23.195.132.80",
	// Control D
	"76.76.2.0",
	"76.76.10.0",
	// Google Public DNS — deliberately LAST: Aliyun's WAF blocklists these
	// two first in practice (observed live 2026-09-02: every probe round
	// started by 405-ing on 8.8.8.8/8.8.4.4 and needed 2-3 rotations), so
	// they are kept as fallbacks rather than the first candidates.
	"8.8.8.8",
	"8.8.4.4",
}

var (
	xffMu       sync.Mutex
	xffIndex    int
	xffPoolOnce sync.Once
	xffPoolVal  []string

	// Bad-IP tracking: an address that produced a WAF 405 is remembered with
	// the time it was marked. zaiXffIP/rotateXffIP skip entries whose bad
	// timestamp is within xffBadTTL. When every pool entry is bad the set is
	// wiped so rotation can retry them (Aliyun's blocklist rolls over time).
	xffBadMu sync.Mutex
	xffBad   = map[string]time.Time{}
)

// ======================================================================
// Random-bank XFF strategy (DEFAULT) — research 2026-09-05
// ======================================================================
// Live probing of the Aliyun WAF (ZAI_RESEARCH_405.md §“Kết quả nghiên
// cứu”) established that the 405 block page on /api/v2/chat/completions is
// governed by exactly two independent rules:
//
//  1. TLS fingerprint — any non-Chrome JA3 (curl, Go stdlib TLS) gets 405
//     on every request, regardless of XFF, token, or rate. The bridge's
//     uTLS Chrome 120 ClientHello passes this rule.
//  2. Rate limit per client-identity — the WAF keys on the IP it believes
//     the request came from (the X-Forwarded-For value when present, else
//     the real egress IP) and allows roughly a dozen requests before that
//     identity starts 405-ing. Evidence: pinning one CF anycast address
//     survived exactly ~16 requests before a permanent 405; five pinned
//     addresses in the same /24 survived 60/60 (budget is per-/32, not per
//     subnet); a possibly-unassigned CF host address passed immediately.
//
// Consequence: a FRESH random address drawn from a large foreign bank for
// EVERY request never exhausts any per-identity budget — no rotation, no
// bad-set, no quarantine TTL, no pool sweep. The WAF does not validate that
// the XFF address is live (unassigned host addresses pass), it only rate-
// limits per identity. This is the complete 405 bypass.
//
// The pool strategy below is retained as an opt-in fallback (ZAI_XFF_
// STRATEGY=pool) for operators who prefer a fixed, auditable address set.

// defaultXffCIDRBanks are large foreign anycast banks (Cloudflare's
// published IPv4 ranges) with millions of assignable host addresses. Only
// ranges of /13 or shorter are used so a single address is never repeated
// in practice (104.16.0.0/13 + 104.24.0.0/14 + 172.64.0.0/13 alone cover
// over a million).
var defaultXffCIDRBanks = []string{
	"104.16.0.0/13",
	"104.24.0.0/14",
	"172.64.0.0/13",
	"162.158.0.0/15",
	"188.114.96.0/20",
	"141.101.64.0/18",
	"108.162.192.0/18",
}

// xffStrategy returns the active XFF selection strategy: "random" (default)
// or "pool" (legacy rotation). ZAI_XFF_IP / ZAI_XFF_IPS still take precedence
// and disable both strategies (operator pinning).
func xffStrategy() string {
	s := strings.ToLower(os.Getenv("ZAI_XFF_STRATEGY"))
	if s == "pool" {
		return "pool"
	}
	return "random"
}

// randomXffBankIP draws one uniformly random host address from the CIDR
// banks. crypto/rand so an attacker observing XFF values cannot predict
// the next identity.
func randomXffBankIP() string {
	bankBig, err := rand.Int(rand.Reader, big.NewInt(int64(len(defaultXffCIDRBanks))))
	if err != nil {
		return defaultXffIPPool[0] // unreachable unless the system RNG fails
	}
	return randomIPFromCIDR(defaultXffCIDRBanks[bankBig.Int64()])
}

// xffPool returns the active rotation pool, loading the ZAI_XFF_IPS override
// (comma-separated) on first use.
func xffPool() []string {
	xffPoolOnce.Do(func() {
		if s := os.Getenv("ZAI_XFF_IPS"); s != "" {
			var ips []string
			for _, p := range strings.Split(s, ",") {
				if p = strings.TrimSpace(p); p != "" {
					ips = append(ips, p)
				}
			}
			if len(ips) > 0 {
				xffPoolVal = ips
				return
			}
		}
		xffPoolVal = defaultXffIPPool
	})
	return xffPoolVal
}

// xffBadTTL is how long a 405-ed address stays quarantined before it is
// eligible to be retried (pool strategy only). Tuned to outlast the typical
// Aliyun rolling blocklist window (observed to be tens of minutes) without
// being so long that the bridge gives up on a recoverable address.
const xffBadTTL = 30 * time.Minute

// xffBadCount returns how many pool entries are currently quarantined.
// Caller must NOT hold xffBadMu (it acquires it).
func xffBadCount(pool []string) int {
	now := time.Now()
	xffBadMu.Lock()
	defer xffBadMu.Unlock()
	n := 0
	for _, ip := range pool {
		if t, ok := xffBad[ip]; ok && now.Sub(t) < xffBadTTL {
			n++
		}
	}
	return n
}

// xffIsBad reports whether ip is currently quarantined.
func xffIsBad(ip string) bool {
	now := time.Now()
	xffBadMu.Lock()
	defer xffBadMu.Unlock()
	t, ok := xffBad[ip]
	return ok && now.Sub(t) < xffBadTTL
}

// markXffIPBad quarantines ip after it produced a WAF 405. If every entry in
// the active pool is now bad the entire bad-set is cleared so rotation can
// retry the addresses — Aliyun's WAF blocklist is rolling, so an address
// that 405s now often recovers within minutes.
func markXffIPBad(ip string) {
	if ip == "" {
		return
	}
	// Random strategy has no persistent identity set to quarantine: every
	// request already starts from a fresh random address, so the bad-set (and
	// the pool-exhaustion refresh machinery) only applies to the pool strategy.
	if xffStrategy() == "random" && os.Getenv("ZAI_XFF_IPS") == "" {
		return
	}
	pool := xffPool()
	now := time.Now()
	xffBadMu.Lock()
	xffBad[ip] = now
	// Count bad entries across the active pool (not just this ip).
	bad := 0
	for _, p := range pool {
		if t, ok := xffBad[p]; ok && now.Sub(t) < xffBadTTL {
			bad++
		}
	}
	if bad >= len(pool) {
		// Whole pool exhausted: clear the bad-set so rotation can proceed,
		// AND trigger an async refresh that fetches fresh foreign IPs from
		// a public CIDR list (Cloudflare). Those new addresses are merged
		// into the active pool, giving the bridge new working egress IPs
		// instead of forever retrying the same burned ones.
		xffBad = map[string]time.Time{}
		xffBadMu.Unlock()
		logInfo("XFF pool fully blocklisted; cleared bad-set and refreshing pool from upstream")
		go refreshXffPoolAsync()
		return
	}
	xffBadMu.Unlock()
}

// xffRefreshMu serialises pool-refresh attempts so concurrent 405 storms do
// not fire multiple Cloudflare fetches simultaneously.
var (
	xffRefreshMu   sync.Mutex
	xffRefreshing  bool
	xffRefreshLast time.Time
)

// xffRefreshCooldown bounds how often we hit the upstream CIDR list. Even if
// the pool keeps getting burned we don't want to spam Cloudflare.
const xffRefreshCooldown = 2 * time.Minute

// xffRefreshTarget is how many fresh IPs we try to add per refresh.
const xffRefreshTarget = 8

// refreshXffPoolAsync is the entry point for background pool refresh. It is
// rate-limited by xffRefreshCooldown and silently no-ops if a refresh is
// already in flight or was performed too recently.
func refreshXffPoolAsync() {
	xffRefreshMu.Lock()
	if xffRefreshing {
		xffRefreshMu.Unlock()
		return
	}
	if time.Since(xffRefreshLast) < xffRefreshCooldown {
		xffRefreshMu.Unlock()
		return
	}
	xffRefreshing = true
	xffRefreshMu.Unlock()

	defer func() {
		xffRefreshMu.Lock()
		xffRefreshing = false
		xffRefreshLast = time.Now()
		xffRefreshMu.Unlock()
	}()

	fresh, err := fetchFreshForeignIPs(xffRefreshTarget)
	if err != nil {
		logError(fmt.Sprintf("XFF pool refresh failed: %s", err.Error()))
		return
	}
	if len(fresh) == 0 {
		logError("XFF pool refresh returned no IPs")
		return
	}

	xffMu.Lock()
	defer xffMu.Unlock()
	pool := xffPool()
	seen := map[string]bool{}
	for _, ip := range pool {
		seen[ip] = true
	}
	added := 0
	for _, ip := range fresh {
		if !seen[ip] {
			xffPoolVal = append(xffPoolVal, ip)
			seen[ip] = true
			added++
		}
	}
	if added > 0 {
		logInfo(fmt.Sprintf("XFF pool refreshed: +%d new IPs (pool size now %d)", added, len(xffPoolVal)))
	}
}

// cloudflareCIDRListURL returns the public Cloudflare IPv4 CIDR list. These
// ranges are foreign (US/EU) and large enough that Aliyun's WAF cannot have
// blocklisted more than a tiny fraction — picking a few random IPs across
// them gives us an effectively unbounded supply of fresh egress addresses.
const cloudflareCIDRListURL = "https://www.cloudflare.com/ips-v4"

// fetchFreshForeignIPs fetches the Cloudflare IPv4 CIDR list and returns
// `count` random host IPs drawn from random ranges. We use Cloudflare's own
// published ranges because they are a stable, well-known foreign source and
// are extremely unlikely to be entirely blocklisted by Aliyun WAF.
func fetchFreshForeignIPs(count int) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", cloudflareCIDRListURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "zai-proxy/1.0 (xff-refresh)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cloudflare ips-v4 returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var cidrs []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "#") {
			continue
		}
		if _, _, err := net.ParseCIDR(line); err == nil {
			cidrs = append(cidrs, line)
		}
	}
	if len(cidrs) == 0 {
		return nil, errors.New("no CIDRs parsed from cloudflare list")
	}

	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		// pick a random CIDR
		idxBig, err := rand.Int(rand.Reader, big.NewInt(int64(len(cidrs))))
		if err != nil {
			continue
		}
		cidr := cidrs[idxBig.Int64()]
		ip := randomIPFromCIDR(cidr)
		if ip != "" {
			out = append(out, ip)
		}
	}
	return out, nil
}

// randomIPFromCIDR picks a random host IP from the given IPv4 CIDR. It skips
// the network and broadcast addresses for /30 and shorter prefixes.
func randomIPFromCIDR(cidr string) string {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ip := ipnet.IP.To4()
	if ip == nil {
		return ""
	}
	mask := ipnet.Mask
	ones, bits := mask.Size()
	if bits != 32 {
		return ""
	}
	hostBits := uint32(bits - ones)
	if hostBits == 0 {
		return ip.String()
	}
	// random offset in [1, size-2] to skip network/broadcast (only meaningful
	// when hostBits >= 2). For hostBits == 1 there are only 2 hosts; we still
	// pick one of them.
	max := uint64(1) << hostBits
	var offset uint64
	if hostBits >= 2 {
		offBig, err := rand.Int(rand.Reader, big.NewInt(int64(max-2)))
		if err != nil {
			return ""
		}
		offset = offBig.Uint64() + 1
	} else {
		offBig, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
		if err != nil {
			return ""
		}
		offset = offBig.Uint64()
	}
	base := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	val := base + uint32(offset)
	return fmt.Sprintf("%d.%d.%d.%d", (val>>24)&0xff, (val>>16)&0xff, (val>>8)&0xff, val&0xff)
}

// xffPickGood scans the pool starting at `startIdx` (modulo pool len) for the
// first entry not currently quarantined. Returns its index, or -1 if every
// entry is bad. Caller must hold xffMu.
func xffPickGood(pool []string, startIdx int) int {
	now := time.Now()
	xffBadMu.Lock()
	defer xffBadMu.Unlock()
	for i := 0; i < len(pool); i++ {
		idx := (startIdx + i) % len(pool)
		ip := pool[idx]
		if t, ok := xffBad[ip]; ok && now.Sub(t) < xffBadTTL {
			continue
		}
		return idx
	}
	return -1
}

// zaiXffIP returns the foreign client IP advertised via X-Forwarded-For when
// the ALB geo-bypass is active. Pin a single address with ZAI_XFF_IP (this
// disables both strategies); ZAI_XFF_IPS pins the legacy pool. Otherwise the
// DEFAULT random-bank strategy draws a fresh foreign address per request
// (see defaultXffCIDRBanks — per-identity WAF budgets are never reached),
// and the legacy pool rotation (ZAI_XFF_STRATEGY=pool) advances through the
// pool, skipping quarantined addresses.
func zaiXffIP() string {
	if ip := os.Getenv("ZAI_XFF_IP"); ip != "" {
		return ip
	}
	if xffStrategy() == "random" && os.Getenv("ZAI_XFF_IPS") == "" {
		return randomXffBankIP()
	}
	pool := xffPool()
	xffMu.Lock()
	defer xffMu.Unlock()
	idx := xffPickGood(pool, xffIndex)
	if idx < 0 {
		// Everything is bad — wipe and retry from current index. This is
		// the safety net so a fresh request can still proceed even if
		// markXffIPBad hasn't run since the last block.
		xffBadMu.Lock()
		xffBad = map[string]time.Time{}
		xffBadMu.Unlock()
		idx = xffIndex % len(pool)
	}
	xffIndex = idx
	return pool[idx]
}

// rotateXffIP advances to the next usable egress identity after a WAF 405.
// With the default random strategy this simply draws a fresh random bank
// address (the previous identity's budget is irrelevant — a new identity is
// a clean slate). With the pool strategy (ZAI_XFF_STRATEGY=pool) it advances
// through the pool, skipping quarantined addresses. Operator pinning via
// ZAI_XFF_IP returns the pinned address unchanged.
func rotateXffIP() string {
	if ip := os.Getenv("ZAI_XFF_IP"); ip != "" {
		return ip
	}
	if xffStrategy() == "random" && os.Getenv("ZAI_XFF_IPS") == "" {
		return randomXffBankIP()
	}
	pool := xffPool()
	xffMu.Lock()
	defer xffMu.Unlock()
	start := (xffIndex + 1) % len(pool)
	idx := xffPickGood(pool, start)
	if idx < 0 {
		// Whole pool bad — clear and retry from `start`.
		xffBadMu.Lock()
		xffBad = map[string]time.Time{}
		xffBadMu.Unlock()
		idx = start
	}
	xffIndex = idx
	return pool[idx]
}

// resolveZAIAlbIP resolves an ALB IP fronting chat.z.ai by looking up z.ai,
// whose DNS is a CNAME to the Aliyun ALB. Returns "" if it cannot resolve.
func resolveZAIAlbIP() string {
	ips, err := net.LookupIP("z.ai")
	if err != nil {
		return ""
	}
	for _, ip := range ips {
		if ip4 := ip.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return ""
}

// dialUTLS creates a TLS connection using Chrome's ClientHello fingerprint.
// Respects HTTP_PROXY/HTTPS_PROXY environment variables for proxy tunneling.
func dialUTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	var rawConn net.Conn

	// Check for proxy (HTTP_PROXY / HTTPS_PROXY / ALL_PROXY)
	proxyStr := os.Getenv("HTTPS_PROXY")
	if proxyStr == "" {
		proxyStr = os.Getenv("HTTP_PROXY")
	}
	if proxyStr == "" {
		proxyStr = os.Getenv("ALL_PROXY")
	}
	if proxyStr == "" {
		proxyStr = os.Getenv("https_proxy")
	}
	if proxyStr == "" {
		proxyStr = os.Getenv("http_proxy")
	}
	if proxyStr == "" {
		proxyStr = os.Getenv("all_proxy")
	}

	if proxyStr != "" {
		// Parse proxy URL
		proxyURL, err := url.Parse(proxyStr)
		if err == nil && proxyURL.Host != "" {
			// Connect to proxy
			proxyConn, err := dialer.DialContext(ctx, "tcp", proxyURL.Host)
			if err != nil {
				return nil, fmt.Errorf("proxy connect: %w", err)
			}

			// Send CONNECT request for HTTPS tunneling
			connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
			_, err = proxyConn.Write([]byte(connectReq))
			if err != nil {
				proxyConn.Close()
				return nil, fmt.Errorf("proxy CONNECT write: %w", err)
			}

			// Read CONNECT response
			br := bufio.NewReader(proxyConn)
			line, err := br.ReadString('\n')
			if err != nil {
				proxyConn.Close()
				return nil, fmt.Errorf("proxy CONNECT read: %w", err)
			}
			if !strings.Contains(line, "200") {
				proxyConn.Close()
				return nil, fmt.Errorf("proxy CONNECT failed: %s", strings.TrimSpace(line))
			}
			// Drain remaining headers
			for {
				line, err = br.ReadString('\n')
				if err != nil || strings.TrimSpace(line) == "" {
					break
				}
			}

			// If bufio reader buffered extra data, unwrap it
			if br.Buffered() > 0 {
				buffered := make([]byte, br.Buffered())
				br.Read(buffered)
				rawConn = &concatConn{
					Conn:   proxyConn,
					buffer: buffered,
				}
			} else {
				rawConn = proxyConn
			}

			logInfo(fmt.Sprintf("[uTLS] Using proxy %s for %s", proxyURL.Host, addr))
		} else {
			rawConn, err = dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
		}
	} else {
		// Direct connection (no proxy)
		dialAddr := addr
		if host == zaiProdHost && albBypassEnabled() {
			if albIP := resolveZAIAlbIP(); albIP != "" {
				dialAddr = net.JoinHostPort(albIP, port)
				logInfo(fmt.Sprintf("[uTLS] ALB geo-bypass: %s -> %s (SNI=%s)", addr, dialAddr, host))
			}
		}
		rawConn, err = dialer.DialContext(ctx, network, dialAddr)
		if err != nil {
			return nil, err
		}
	}

	// uTLS config — advertise HTTP/1.1 only to avoid HTTP/2 fingerprinting
	config := &utls.Config{
		ServerName:         host,
		NextProtos:         []string{"http/1.1"},
		InsecureSkipVerify: false,
	}

	// Chrome 120 fingerprint
	uConn := utls.UClient(rawConn, config, utls.HelloChrome_120)

	if err := uConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, err
	}

	return uConn, nil
}

// concatConn wraps a connection that has pre-buffered data from a bufio.Reader.
type concatConn struct {
	net.Conn
	buffer []byte
}

func (c *concatConn) Read(b []byte) (int, error) {
	if len(c.buffer) > 0 {
		n := copy(b, c.buffer)
		c.buffer = c.buffer[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

// Z.AI client with cookie jar + uTLS Chrome fingerprint
var zaiJar = &cookieJar{}

var zaiHTTPClient = &http.Client{
	Transport: newPacedTransport(&http.Transport{
		DialTLSContext:        dialUTLS,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		MaxConnsPerHost:       20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     false,
	}),
	Jar: zaiJar,
}

// isDeadConnError reports whether a transport error is a pooled connection
// that died before the request was sent. net/http returns ErrServerClosedIdle
// wrapped as "http: server closed idle connection", and a conn the server
// RSTs mid-request-write surfaces as "use of closed network connection" or
// "broken pipe"/"connection reset by peer". None of these prove the request
// reached the server — retrying is safe. (Go's transport normally retries
// GETs on an idle conn itself, but never a request with a body it may have
// started streaming, which is exactly the POST case here.)
func isDeadConnError(msg string) bool {
	for _, s := range []string{
		"use of closed network connection",
		"server closed idle connection",
		"broken pipe",
		"connection reset by peer",
		"EOF",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// ============================================================================
// COOKIE JAR — minimal implementation, thread-safe
// ============================================================================

type cookieEntry struct {
	name   string
	value  string
	domain string
	path   string
}

type cookieJar struct {
	mu      sync.Mutex
	cookies []cookieEntry
}

func (j *cookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cookies {
		filtered := j.cookies[:0]
		for _, e := range j.cookies {
			if e.name == c.Name && e.domain == c.Domain && e.path == c.Path {
				continue
			}
			filtered = append(filtered, e)
		}
		j.cookies = filtered
		j.cookies = append(j.cookies, cookieEntry{
			name:   c.Name,
			value:  c.Value,
			domain: c.Domain,
			path:   c.Path,
		})
	}
}

func (j *cookieJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []*http.Cookie
	for _, e := range j.cookies {
		out = append(out, &http.Cookie{
			Name:   e.name,
			Value:  e.value,
			Domain: e.domain,
			Path:   e.path,
		})
	}
	return out
}

// ============================================================================
// WARM-UP — acquire acw_tc anti-bot cookies before API calls
// ============================================================================

func warmupCookies() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", BASE_URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", zaiUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("sec-ch-ua", `"Not=A?Brand";v="99", "Brave";v="151", "Chromium";v="151"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)

	resp, err := zaiHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if config.Logging.Level == "debug" {
		cookies := zaiJar.Cookies(req.URL)
		for _, c := range cookies {
			v := c.Value
			if len(v) > 20 {
				v = v[:20]
			}
			log.Printf("[Warmup] Cookie: %s=%s...", c.Name, v)
		}
	}

	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

//
// ============================================================================
// UTILITY FUNCTIONS
// ============================================================================

func randomUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------- UUID v4 — manual hex encoding, no fmt.Sprintf ----------

func generateUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0F) | 0x40
	b[8] = (b[8] & 0x3F) | 0x80

	var dst [36]byte
	j := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			dst[j] = '-'
			j++
		}
		dst[j] = hexLower[b[i]>>4]
		dst[j+1] = hexLower[b[i]&0xF]
		j += 2
	}
	return string(dst[:])
}

// ---------- Timestamp helpers ----------

func getTimestampUTC() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

func currentTimeMillis() int64 {
	return time.Now().UnixMilli()
}

// ---------- Token estimation ----------

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

// ---------- Message helpers ----------

func getMessageContent(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var arr []interface{}
	if err := json.Unmarshal(content, &arr); err == nil {
		var texts []string
		for _, item := range arr {
			switch v := item.(type) {
			case string:
				texts = append(texts, v)
			case map[string]interface{}:
				t, _ := v["type"].(string)
				if t == "text" {
					if txt, ok := v["text"].(string); ok {
						texts = append(texts, txt)
					}
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(content)
}

func messagesToPrompt(messages []Message) string {
	var sb strings.Builder
	for _, msg := range messages {
		content := getMessageContent(msg.Content)
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

func boolPtr(b bool) *bool { return &b }

// ============================================================================
// URL ENCODING — custom lookup table, zero allocations for safe chars
// ============================================================================

const hexUpper = "0123456789ABCDEF"
const hexLower = "0123456789abcdef"

var baseSafeTable [256]bool

func urlEncode(s string, safe string) string {
	var safeTable [256]bool
	safeTable = baseSafeTable
	for i := 0; i < len(safe); i++ {
		safeTable[safe[i]] = true
	}

	var b strings.Builder
	b.Grow(len(s)*3 + 16)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if safeTable[c] {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&0x0F])
		}
	}
	return b.String()
}

func fromHex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return 0
	}
}

// ============================================================================
// CRYPTO HELPERS
// ============================================================================

func base64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func hmacSHA1(key, msg []byte) []byte {
	h := hmac.New(sha1.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

func base64Decode(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s + "=="); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s + "=="); err == nil {
		return b, nil
	}
	return nil, errors.New("base64 decode failed")
}

// ============================================================================
// JSON MARSHALING — disables HTML escaping, uses pooled buffer
// ============================================================================

func jsonMarshal(v interface{}) ([]byte, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		bufPool.Put(buf)
		return nil, err
	}
	raw := buf.Bytes()
	result := make([]byte, len(raw)-1)
	copy(result, raw)
	bufPool.Put(buf)
	return result, nil
}
