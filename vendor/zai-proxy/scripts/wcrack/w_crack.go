// w_crack.go — offline attempt to decrypt the w-blob of Aliyun device tokens
// using the runtime constants recovered in the Fielin report (§19).
//
// Run: go run scripts/wcrack/w_crack.go [tokens.sqlite]
//
// The Fielin report tried the same constants on a single CN-region capture and
// saw no structure. We differ: the zai-proxy DB holds 4148 tokens from ONE
// 94-second session (one uuid1) — i.e. one runtime key — and their w-blobs
// share long block-aligned ciphertext prefixes (divergence at raw byte 48 /
// 240), consistent with ECB or a fixed-IV CBC. If any key/mode guess is right
// we should see JSON (the `b` collector object) in the plaintext.
package main

import (
	"crypto/aes"
	"crypto/md5"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

func b64d(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func scorePlain(b []byte) int {
	// crude JSON-likeness score
	n := 0
	for _, c := range b {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '"' || c == '{' || c == '}' ||
			c == ':' || c == ',' || c == ' ' || c == '_' || c == '-' || c == '.' {
			n++
		}
	}
	return n * 100 / len(b)
}

func tryDec(key, iv, blob []byte, mode string) (string, int) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", -1
	}
	if len(blob)%aes.BlockSize != 0 || len(blob) == 0 {
		return "", -1
	}
	dst := make([]byte, len(blob))
	switch mode {
	case "ecb":
		for off := 0; off < len(blob); off += aes.BlockSize {
			block.Decrypt(dst[off:off+aes.BlockSize], blob[off:off+aes.BlockSize])
		}
	case "cbc":
		prev := make([]byte, aes.BlockSize)
		if len(iv) == aes.BlockSize {
			copy(prev, iv)
		}
		tmp := make([]byte, aes.BlockSize)
		for off := 0; off < len(blob); off += aes.BlockSize {
			block.Decrypt(tmp, blob[off:off+aes.BlockSize])
			for i := 0; i < aes.BlockSize; i++ {
				dst[off+i] = tmp[i] ^ prev[i]
			}
			copy(prev, blob[off:off+aes.BlockSize])
		}
	default:
		return "", -1
	}
	preview := dst
	if len(preview) > 96 {
		preview = preview[:96]
	}
	return string(preview), scorePlain(dst)
}

func main() {
	dbPath := "tokens.sqlite"
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	var raw string
	if err := db.QueryRow("SELECT token FROM tokens ORDER BY id LIMIT 1").Scan(&raw); err != nil {
		panic(err)
	}
	dec, _ := base64.StdEncoding.DecodeString(raw)
	parts := strings.Split(string(dec), "#")
	if len(parts) != 5 {
		panic("bad token")
	}
	w := parts[2]
	blob := b64d(w)
	fmt.Printf("w-blob: %d base64 chars -> %d raw bytes (%d AES blocks)\n",
		len(w), len(blob), len(blob)/16)

	accessSec := []byte("FqJB6iRNVYdEGpwb")       // ACCESS_SEC 16B
	saltRaw := []byte("NLAoqT6K03oLbQXW2VS3zA==") // SALT as stored
	saltB64 := b64d("NLAoqT6K03oLbQXW2VS3zA==")   // SALT decoded 16B
	secret := []byte("daye,raolewoba!")           // md5 constant
	md5raw := func(b []byte) []byte { h := md5.Sum(b); return h[:] }
	keys := map[string][]byte{
		"ACCESS_SEC":       accessSec,
		"SALT_b64decoded":  saltB64,
		"SALT_raw_pad":     pad16(saltRaw),
		"secret_pad":       pad16(secret),
		"md5(ACCESS)":      md5raw(accessSec),
		"md5(SALT_b64)":    md5raw(saltB64),
		"md5(secret)":      md5raw(secret),
		"md5(ACCESS|SALT)": md5raw(concat(accessSec, saltB64)),
		"md5(SALT|ACCESS)": md5raw(concat(saltB64, accessSec)),
	}
	ivs := map[string][]byte{
		"zeros":     make([]byte, 16),
		"SALT_b64":  saltB64,
		"blob[:16]": blob[:16],
	}

	type result struct {
		name  string
		score int
		plain string
	}
	var results []result
	for kname, key := range keys {
		// ECB
		if p, s := tryDec(key, nil, blob, "ecb"); s > 0 {
			results = append(results, result{kname + "/ecb", s, p})
		}
		// CBC with each IV guess
		for ivname, iv := range ivs {
			if p, s := tryDec(key, iv, blob, "cbc"); s > 0 {
				results = append(results, result{kname + "/cbc/iv=" + ivname, s, p})
			}
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	if len(results) == 0 {
		fmt.Println("no candidate produced a decrypt (all keys failed)")
		return
	}
	for i, r := range results {
		if i >= 10 {
			break
		}
		fmt.Printf("%-36s score=%d%% head=%q\n", r.name, r.score, r.plain)
	}
}

func concat(a, b []byte) []byte {
	out := make([]byte, 0, len(a)+len(b))
	out = append(out, a...)
	return append(out, b...)
}

func pad16(b []byte) []byte {
	for len(b) < 16 {
		b = append(b, 0)
	}
	return b[:16]
}
