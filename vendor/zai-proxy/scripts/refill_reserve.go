//go:build ignore

// refill_reserve.go — mint N single-use device tokens in pure Go on a blessed
// Q (qbless.json) and top up the tokens.sqlite emergency reserve.
//
// Usage:
//
//	go run scripts/refill_reserve.go [--target 100] [--db tokens.sqlite] [--qfile qbless.json]
//
// Refuses to touch the DB when no usable session exists (missing, unverified,
// or older than QBLESS_MAX_AGE / 6h) — the reserve must only ever hold tokens
// minted on a Q proven live by the canary.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver (same as the bridge)
)

type blessSession struct {
	Q         string `json:"q"`
	SK        string `json:"sk"`
	QTS       int64  `json:"qts"`
	IP        string `json:"ip"`
	SessA     string `json:"sessA"`
	SessB     string `json:"sessB"`
	BootTS    int64  `json:"bootTS"`
	DeviceTag string `json:"deviceTag"`
	Verified  bool   `json:"verified"`
	BlessedAt int64  `json:"blessedAt"`
}

func randHex32() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func randIntn(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return n / 2
	}
	return int(v.Int64())
}

// buildWFields — 111-field Token-A w-blob (mirror of internal/zbridge/qmint.go
// buildQWFields, verified by BURNIN 3/3 PASS on a blessed Q).
func buildWFields(convURL, serverIP string, s *blessSession) [111]string {
	var w [111]string
	w[0] = "W.10054"
	w[5], w[6], w[7] = "Win32", "Chrome", "151.0.0.0"
	ab := make([]byte, 8)
	rand.Read(ab)
	w[20], w[21], w[22] = "17", base64.StdEncoding.EncodeToString(ab), "8"
	w[32], w[34] = randHex32(), "8"
	w[36], w[37] = "Windows", "10"
	w[42] = serverIP
	m11 := 1850 + randIntn(120)
	m20 := m11 + 4 + randIntn(3)
	m23 := m11 + 525 + randIntn(40)
	m30 := m23 + 4 + randIntn(3)
	m40 := m30 + 20 + randIntn(6)
	m90 := m23 + 300 + randIntn(40)
	m91 := m90 + 40 + randIntn(90)
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
	w[77] = "" // Token-A shape — the shape that PASSes verify
	w[78] = s.DeviceTag
	w[80] = "5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36"
	w[85], w[86] = "0", "0"
	w[87] = strconv.FormatInt(s.QTS, 10)
	w[110] = "[Chromium,Not=A?Brand]"
	return w
}

// deriveWToken — token w = w + "|93-…|94-…" appended to w[43].
func deriveWToken(w [111]string) string {
	parts := strings.Split(w[43], "|")
	var m91 int
	for _, p := range parts {
		if strings.HasPrefix(p, "91-") {
			m91, _ = strconv.Atoi(p[3:])
		}
	}
	m93 := m91 + 20 + randIntn(40)
	m94 := m93 + 3 + randIntn(15)
	w[43] = w[43] + "|93-" + strconv.Itoa(m93) + "|94-" + strconv.Itoa(m94)
	return strings.Join(w[:], "#")
}

func mintOnQ(s *blessSession) (string, error) {
	convURL := "https://chat.z.ai/c/" + randHex32() + randHex32()[:16]
	w := buildWFields(convURL, s.IP, s)
	plain := deriveWToken(w)
	block, err := aes.NewCipher([]byte(s.SK))
	if err != nil {
		return "", err
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
		tF + "#" + s.Q + "#" + wCt + "#0#" + hex.EncodeToString(sum[:]))), nil
}

func convURLUUID() string {
	return randHex32()[:8] + "-" + randHex32()[:4] + "-" + randHex32()[:4] + "-" + randHex32()[:4] + "-" + randHex32()[:12]
}

func main() {
	target := flag.Int("target", 100, "top the reserve up to this many tokens")
	db := flag.String("db", "tokens.sqlite", "reserve DB path")
	qfile := flag.String("qfile", "qbless.json", "blessed session file")
	maxAge := flag.Duration("max-age", 6*time.Hour, "max session age")
	flag.Parse()

	raw, err := os.ReadFile(*qfile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ read %s: %v\n", *qfile, err)
		os.Exit(1)
	}
	var s blessSession
	if err := json.Unmarshal(raw, &s); err != nil || s.Q == "" || s.SK == "" {
		fmt.Fprintf(os.Stderr, "❌ bad session file %s: %v\n", *qfile, err)
		os.Exit(1)
	}
	age := time.Since(time.UnixMilli(s.BlessedAt))
	if age > *maxAge {
		fmt.Fprintf(os.Stderr, "❌ session %.1fh old (max %.1fh) — run q-bless first\n", age.Hours(), maxAge.Hours())
		os.Exit(1)
	}
	if !s.Verified {
		fmt.Fprintln(os.Stderr, "❌ session not canary-verified — run q-bless first")
		os.Exit(1)
	}
	fmt.Printf("✅ session OK: Q=%.32s… age %.1fh (blessed %s)\n",
		s.Q, age.Hours(), time.UnixMilli(s.BlessedAt).Format("15:04:05"))

	dbc, err := sql.Open("sqlite", *db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ open %s: %v\n", *db, err)
		os.Exit(1)
	}
	defer dbc.Close()
	if _, err := dbc.Exec(`CREATE TABLE IF NOT EXISTS tokens (
		id    INTEGER PRIMARY KEY AUTOINCREMENT,
		token TEXT    NOT NULL,
		batch INTEGER NOT NULL
	)`); err != nil {
		fmt.Fprintf(os.Stderr, "❌ create table: %v\n", err)
		os.Exit(1)
	}
	var have int
	if err := dbc.QueryRow("SELECT COUNT(*) FROM tokens").Scan(&have); err != nil {
		fmt.Fprintf(os.Stderr, "❌ count: %v\n", err)
		os.Exit(1)
	}
	need := *target - have
	if need <= 0 {
		fmt.Printf("✅ reserve already at %d/%d — nothing to do\n", have, *target)
		return
	}
	fmt.Printf("minting %d tokens (have %d, target %d)…\n", need, have, *target)

	// One batch id for this refill run.
	var batch int64
	if err := dbc.QueryRow("SELECT COALESCE(MAX(batch), 0) + 1 FROM tokens").Scan(&batch); err != nil {
		batch = time.Now().Unix()
	}

	start := time.Now()
	tx, err := dbc.Begin()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ begin: %v\n", err)
		os.Exit(1)
	}
	stmt, err := tx.Prepare("INSERT INTO tokens (token, batch) VALUES (?, ?)")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ prepare: %v\n", err)
		os.Exit(1)
	}
	defer stmt.Close()
	for i := 0; i < need; i++ {
		tok, err := mintOnQ(&s)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ mint #%d: %v\n", i+1, err)
			tx.Rollback()
			os.Exit(1)
		}
		if _, err := stmt.Exec(tok, batch); err != nil {
			fmt.Fprintf(os.Stderr, "❌ insert #%d: %v\n", i+1, err)
			tx.Rollback()
			os.Exit(1)
		}
	}
	if err := tx.Commit(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ commit: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ reserve refilled: %d new tokens in %s (batch %d) → %s\n",
		need, time.Since(start).Round(time.Millisecond), batch, filepath.Join(*db))
}
