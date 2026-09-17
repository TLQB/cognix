// qmint.go — pure-Go device-token minting on a BLESSED Q (qbless.json).
//
// Research 2026-09-03 (EXP_CAPTCHA_RESULTS.md §10): the Aliyun server binds
// a per-Q trust score at Log1 time to the CONNECTING CLIENT's fingerprint.
// A Q registered from real Chrome (via cmd/q-bless, ~30s of Chromium every
// few hours) lets pure Go mint UNLIMITED tokens on it — verified 3/3 PASS at
// Q-age 3h15m. This file is the bridge-side mint on that Q.
//
// getNextToken() (see captcha.go):
//  1. THIS: qbless.json session — unlimited, no browser, no daemon
//  2. tokens.sqlite fallback — emergency reserve
//
// QBLESS_FILE overrides the session path (default: qbless.json next to the
// binary, then the repo root).
//
// Q LIFETIME MODEL (measured 2026-09-05, EXP_CAPTCHA_RESULTS.md §10.4):
// Q has NO time-based TTL — observed PASS at 18h39m age (plus a 14h01m
// 38-round probe run), minting unlimited tokens, from a DIFFERENT IP than
// the bless IP. The SDK stores Q in localStorage forever and has no refresh
// mechanism, so the server accepts it indefinitely. Q dies by EVENT, not
// time: schema/appKey rotation or trust revocation. Therefore there is no
// max-age gate anymore — the session is trusted until its verify results
// say otherwise (see qblessHealth below). QBLESS_MAX_AGE is still honored
// when set, as an operator override to force fallback to the DB reserve.
package zbridge

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// blessSession mirrors cmd/q-bless's BlessSession file format.
type blessSession struct {
	Q          string `json:"q"`
	SK         string `json:"sk"`
	QTS        int64  `json:"qts"`
	IP         string `json:"ip"`
	SessA      string `json:"sessA"`
	SessB      string `json:"sessB"`
	BootTS     int64  `json:"bootTS"`
	DeviceTag  string `json:"deviceTag"`
	Verified   bool   `json:"verified"`
	VerifiedAt int64  `json:"verifiedAt"`
	Canary     string `json:"canary"`
	BlessedAt  int64  `json:"blessedAt"`
}

var (
	qblessMu      sync.Mutex
	qblessLoaded  *blessSession
	qblessChecked time.Time // stat/mtime cache — don't re-read on every mint
)

// qblessHealth tracks the blessed session's observed liveness from REAL
// verify outcomes (no extra probe traffic). States:
//   - unknown/alive: mint freely
//   - dead (consecutive verify FAILs): stop minting, fall back to the DB
//     reserve, nag once per hour for the operator to re-run q-bless
//
// A single verify FAIL can be transient (network, param race) — only a
// streak marks the Q as dead. Any success marks it alive again. Death is
// recorded PER Q: a re-blessed session carrying a DIFFERENT Q is trusted
// immediately, while a process restart re-reading the SAME dead Q stays
// dead until it verifies again or the operator re-blesses.
type qblessHealth struct {
	mu         sync.Mutex
	failStreak int
	deadQ      string // the Q observed dead ("" = none)
	nagged     time.Time
}

var qHealth qblessHealth

// qblessReportVerify feeds the health tracker with a real VerifyCaptchaV3
// outcome for a token minted by qmintMint on Q.
func qblessReportVerifyQ(q string, ok bool) {
	qHealth.mu.Lock()
	defer qHealth.mu.Unlock()
	if qHealth.deadQ != "" && qHealth.deadQ != q {
		// A different Q is verifying — the old death verdict doesn't apply.
		qHealth.deadQ = ""
		qHealth.failStreak = 0
	}
	if ok {
		qHealth.failStreak = 0
		qHealth.deadQ = ""
		return
	}
	if qHealth.deadQ == q {
		return // already dead, no need to re-log
	}
	qHealth.failStreak++
	if qHealth.failStreak >= 3 {
		qHealth.deadQ = q
		logOperational(fmt.Sprintf("[QBless] Q marked DEAD after %d consecutive verify FAILs — run q-bless to re-bless (bridge falls back to the tokens.sqlite reserve)", qHealth.failStreak))
	}
}

// qblessQDead reports whether this exact Q has been observed dead.
func qblessQDead(q string) bool {
	qHealth.mu.Lock()
	defer qHealth.mu.Unlock()
	return q != "" && qHealth.deadQ == q
}

// qblessIsDead reports whether the CURRENT session's Q is observed dead.
func qblessIsDead() bool {
	s := qblessLoad()
	if s == nil {
		return false
	}
	return qblessQDead(s.Q)
}

// qblessSessionReset is kept for compatibility — with per-Q death tracking
// a session change needs no explicit reset (a different Q is alive by
// definition), but tests use it to force a clean slate.
func qblessSessionReset() {
	qHealth.mu.Lock()
	defer qHealth.mu.Unlock()
	qHealth.deadQ = ""
	qHealth.failStreak = 0
}

// qblessFile locates the session file. Search order:
//  1. QBLESS_FILE — but only if it exists; a stale path (moved bundle dir)
//     must NOT mask the healthy fallbacks below (production incident
//     2026-09-10: env pointed into a deleted bundle dir while a good
//     session sat in ~/.config/zai-proxy and the proxy ran "no session")
//  2. ./qbless.json (cwd)
//  3. next to tokens.sqlite (repo root)
//  4. ~/.config/zai-proxy/qbless.json — where start.sh blesses to and the
//     bundle docs point users (survives upgrades)
//  5. /var/lib/zai/qbless.json (system install)
func qblessFile() string {
	candidates := []string{}
	if v := os.Getenv("QBLESS_FILE"); v != "" {
		candidates = append(candidates, v)
	}
	candidates = append(candidates, "qbless.json")
	if abs, err := filepath.Abs(dbPath); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(abs), "qbless.json"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "zai-proxy", "qbless.json"))
	}
	candidates = append(candidates, "/var/lib/zai/qbless.json")
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// qblessLoad returns the cached session, re-reading the file when its mtime
// changed. nil ⇒ no usable session.
func qblessLoad() *blessSession {
	qblessMu.Lock()
	defer qblessMu.Unlock()

	path := qblessFile()
	if path == "" {
		qblessLoaded = nil
		return nil
	}
	st, err := os.Stat(path)
	if err != nil {
		qblessLoaded = nil
		return nil
	}
	if qblessLoaded != nil && !st.ModTime().After(qblessChecked) {
		s := qblessLoaded
		return s
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var s blessSession
	if err := json.Unmarshal(raw, &s); err != nil || s.Q == "" || s.SK == "" {
		logError("[QBless] bad session file " + path + ": " + fmt.Sprint(err))
		qblessLoaded = nil
		return nil
	}
	// Death is tracked PER Q (qHealth.deadQ), so no reset is needed here: a
	// re-blessed file with a different Q is alive by definition; a re-read of
	// the same dead Q correctly stays dead even after a process restart.
	qblessLoaded = &s
	qblessChecked = st.ModTime()
	return qblessLoaded
}

// qblessMaxAge returns the operator-forced max session age, or 0 when
// unset — by default there is NO age limit (Q has no observed time-based
// TTL; see the lifetime model notes atop this file). QBLESS_MAX_AGE stays
// available as an explicit operator override to force DB fallback.
func qblessMaxAge() time.Duration {
	if v := os.Getenv("QBLESS_MAX_AGE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			return time.Duration(h) * time.Hour
		}
	}
	return 0
}

// qmintMint produces one fresh Token-A device token on the blessed Q.
// Returns ("", false, nil) when no usable session exists (fall through to
// the DB); ("" , false, err) on real errors worth logging.
func qmintMint() (token string, ok bool, err error) {
	// ALWAYS load first: qblessLoad() re-stats the session file, and death is
	// tracked per-Q, so a re-blessed file with a new Q is trusted immediately
	// while the same dead Q stays dead — no ordering hazard either way.
	s := qblessLoad()
	if s == nil {
		return "", false, nil
	}
	if qblessQDead(s.Q) {
		// Dead Q: minting would burn verify requests. Fall through to the DB
		// reserve; nag once per hour so the operator re-runs q-bless.
		qHealth.mu.Lock()
		if time.Since(qHealth.nagged) > time.Hour {
			qHealth.nagged = time.Now()
			logOperational(fmt.Sprintf("[QBless] Q DEAD (session %.1fh old) — run q-bless to re-bless; falling back to DB reserve", time.Since(time.UnixMilli(s.BlessedAt)).Hours()))
		}
		qHealth.mu.Unlock()
		return "", false, nil
	}
	if max := qblessMaxAge(); max > 0 {
		if age := time.Since(time.UnixMilli(s.BlessedAt)); age > max {
			// Operator forced a max age — nag once per hour, then fall through.
			qHealth.mu.Lock()
			if time.Since(qHealth.nagged) > time.Hour {
				qHealth.nagged = time.Now()
				logError(fmt.Sprintf("[QBless] session %.1fh old (> QBLESS_MAX_AGE %.1fh) — falling back to DB reserve; refresh with q-bless to re-enable minting", age.Hours(), max.Hours()))
			}
			qHealth.mu.Unlock()
			return "", false, nil
		}
	}

	convURL := "https://chat.z.ai/c/" + generateUUID()
	w := buildQWFields(convURL, s.IP, s)
	plain := deriveQWToken(w)
	block, err := aes.NewCipher([]byte(s.SK))
	if err != nil {
		return "", false, err
	}
	pt := []byte(plain)
	padLen := aes.BlockSize - len(pt)%aes.BlockSize
	pt = append(pt, bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	ct := make([]byte, len(pt))
	cipher.NewCBCEncrypter(block, []byte("0123456789ABCDEF")).CryptBlocks(ct, pt)
	wCt := base64.StdEncoding.EncodeToString(ct)

	const tF = "SG_WEB"
	sum := md5.Sum([]byte(tF + "#" + s.Q + "#" + wCt + "#0#daye,raolewoba!"))
	return base64.StdEncoding.EncodeToString([]byte(
		tF + "#" + s.Q + "#" + wCt + "#0#" + hex.EncodeToString(sum[:]))), true, nil
}

// buildQWFields — the 111-field Token-A w-blob (field-by-field ground truth
// lives in scripts/research/nosdk/gomint.go buildWLog2Fields; verified by
// BURNIN 3/3 PASS on a blessed Q). Session-stable fields (71/72/73/78/87)
// come from the session so all tokens share ONE device identity.
func buildQWFields(convURL, serverIP string, s *blessSession) [111]string {
	var w [111]string
	w[0] = "W.10054"
	w[5], w[6], w[7] = "Win32", "Chrome", "151.0.0.0"
	ab := make([]byte, 8)
	rand.Read(ab)
	w[20], w[21], w[22] = "17", base64.StdEncoding.EncodeToString(ab), "8"
	w[32], w[34] = qRandHex32(), "8"
	w[36], w[37] = "Windows", "10"
	w[42] = serverIP
	m11 := 1850 + qRandIntn(120)
	m20 := m11 + 4 + qRandIntn(3)
	m23 := m11 + 525 + qRandIntn(40)
	m30 := m23 + 4 + qRandIntn(3)
	m40 := m30 + 20 + qRandIntn(6)
	m90 := m23 + 300 + qRandIntn(40)
	m91 := m90 + 40 + qRandIntn(90)
	w[43] = fmt.Sprintf("10-0|11-%d|20-%d|23-%d|30-%d|40-%d|90-%d|91-%d|92-%d",
		m11, m20, m23, m30, m40, m90, m91, m91)
	w[44], w[45] = "true", "true"
	w[47] = "1440*1920"
	w[53] = convURL
	w[63] = "151.0.0.0"
	w[64] = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"
	w[67], w[68] = "saf-captcha", "1"
	w[71] = s.SessA
	w[72] = strconv.FormatInt(s.BootTS, 10)
	w[73] = s.SessB
	w[74] = strconv.FormatInt(s.BootTS+int64(m91), 10)
	w[75] = "desktop"
	w[77] = "" // Token-A shape (page-load) — the shape that PASSes verify
	w[78] = s.DeviceTag
	w[80] = "5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"
	w[85], w[86] = "0", "0"
	w[87] = strconv.FormatInt(s.QTS, 10)
	w[110] = "[Chromium,Not=A?Brand]"
	return w
}

// qblessTokenQ structurally extracts the Q embedded in a device token
// ("" when the token is not a decodable qbless shape). Works without the
// session file, so health verdicts stay bound to the token's own Q even if
// the session file changes mid-flight.
func qblessTokenQ(deviceToken string) string {
	if deviceToken == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(deviceToken)
	if err != nil {
		return ""
	}
	parts := strings.SplitN(string(raw), "#", 3)
	if len(parts) < 2 || parts[1] == "" {
		return ""
	}
	return parts[1]
}

// qblessMintedToken reports whether deviceToken was minted on the CURRENT
// blessed session (as opposed to coming from the DB reserve).
func qblessMintedToken(deviceToken string) bool {
	s := qblessLoad()
	if s == nil {
		return false
	}
	return qblessTokenQ(deviceToken) == s.Q
}

// deriveQWToken — token w = w + "|93-…|94-…" appended to w[43] (the ONLY
// diff vs Log2#1; ground truth aes_log[21]).
func deriveQWToken(w [111]string) string {
	parts := strings.Split(w[43], "|")
	var m91 int
	for _, p := range parts {
		if strings.HasPrefix(p, "91-") {
			m91, _ = strconv.Atoi(p[3:])
		}
	}
	m93 := m91 + 20 + qRandIntn(40)
	m94 := m93 + 3 + qRandIntn(15)
	w[43] = w[43] + "|93-" + strconv.Itoa(m93) + "|94-" + strconv.Itoa(m94)
	return strings.Join(w[:], "#")
}

func qRandHex32() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func qRandIntn(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return n / 2
	}
	return int(v.Int64())
}
