// scripts/latency/main.go
//
// Latency probe for the GLM bridge: measures, from the outside, the latency a
// client actually experiences.
//
//  1. Endpoint overhead      — GET /health, /status, /v1/models (no upstream)
//  2. Non-stream chat        — POST /v1/chat/completions stream:false, wall time
//  3. Streaming chat         — TTFT (first delta / first text), wall time to [DONE]
//  4. Generation throughput  — chars/s and est. tok/s once the first text lands
//  5. Concurrency            — C parallel streaming requests (pool/queueing view)
//
// Usage:
//
//	go run ./scripts/latency -bridge http://127.0.0.1:3001 -token Waguri \
//	                          -model glm-4.7 -n 10 -concurrency 5
//
// Every sample is printed as LAT:/FAIL: lines so runs can be grepped/summarized,
// mirroring the RESULT:/TURN: conventions of scripts/e2e.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	bridge  = flag.String("bridge", "http://127.0.0.1:3001", "bridge base URL")
	token   = flag.String("token", "Waguri", "bridge auth token")
	model   = flag.String("model", "glm-4.7", "model to request")
	n       = flag.Int("n", 10, "samples per measurement")
	cc      = flag.Int("concurrency", 5, "parallel streaming requests in the concurrency pass")
	timeout = flag.Duration("timeout", 120*time.Second, "per-request timeout")
)

const shortPrompt = "Reply with exactly: OK"
const mediumPrompt = "Write a short paragraph (about 5 sentences) summarizing the history of the postal service."

var client = &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}}

// ------------------------------------------------------------------ stats

type sample struct {
	label string
	path  string // for plain GET endpoints
	ms    []float64
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func (s *sample) add(d time.Duration) { s.ms = append(s.ms, float64(d.Microseconds())/1000.0) }

func (s *sample) report() {
	if len(s.ms) == 0 {
		fmt.Printf("LAT %-28s no samples\n", s.label)
		return
	}
	sorted := append([]float64(nil), s.ms...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, v := range sorted {
		sum += v
	}
	avg := sum / float64(len(sorted))
	fmt.Printf("LAT %-28s n=%2d  min=%7.1fms  p50=%7.1fms  avg=%7.1fms  p95=%7.1fms  max=%7.1fms\n",
		s.label, len(sorted), sorted[0], pct(sorted, 0.50), avg, pct(sorted, 0.95), sorted[len(sorted)-1])
}

// ------------------------------------------------------------------ http

func doReq(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, *bridge+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+*token)
	return client.Do(req)
}

// transientErr mirrors scripts/e2e's notion of server/infra noise worth a retry.
func transientErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, pat := range []string{"405", "502", "503", "504", "MODEL_CONCURRENCY_LIMIT", "at capacity", "EOF", "captcha"} {
		if strings.Contains(s, pat) {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ chat

type chatMetrics struct {
	total       time.Duration // wall: request sent -> [DONE] / response read
	firstDelta  time.Duration // -> first SSE data chunk with any delta
	firstText   time.Duration // -> first non-empty delta.content
	chars       int
	reasonChars int
	reasoning   bool // model emitted reasoning before text
}

// chatOnce runs one OpenAI-compatible completion and measures it.
func chatOnce(ctx context.Context, prompt string, stream bool) (chatMetrics, error) {
	var m chatMetrics
	body, _ := json.Marshal(map[string]interface{}{
		"model":    *model,
		"stream":   stream,
		"messages": []map[string]interface{}{{"role": "user", "content": prompt}},
	})
	t0 := time.Now()
	resp, err := doReq(ctx, "POST", "/v1/chat/completions", body)
	if err != nil {
		return m, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return m, fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if !stream {
		// Whole body arrives when the response is complete.
		raw, err := io.ReadAll(resp.Body)
		m.total = time.Since(t0)
		if err != nil {
			return m, err
		}
		var out struct {
			Choices []struct {
				Message struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return m, fmt.Errorf("bad json: %v", err)
		}
		if len(out.Choices) > 0 {
			m.chars = len(out.Choices[0].Message.Content)
			m.reasonChars = len(out.Choices[0].Message.Reasoning)
			m.reasoning = m.reasonChars > 0
		}
		return m, nil
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	seenDelta, seenText, done := false, false, false
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return m, fmt.Errorf("bridge error chunk: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if !seenDelta && (d.Content != "" || d.ReasoningContent != "") {
			m.firstDelta = time.Since(t0)
			seenDelta = true
		}
		if !seenText && d.Content != "" {
			m.firstText = time.Since(t0)
			seenText = true
		}
		if d.ReasoningContent != "" {
			m.reasoning = true
		}
		m.chars += len(d.Content)
		m.reasonChars += len(d.ReasoningContent)
	}
	if err := sc.Err(); err != nil {
		return m, err
	}
	if !done {
		return m, fmt.Errorf("stream ended without [DONE]")
	}
	m.total = time.Since(t0)
	return m, nil
}

func chatWithRetry(prompt string, stream bool) (chatMetrics, error) {
	var m chatMetrics
	var err error
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		m, err = chatOnce(ctx, prompt, stream)
		cancel()
		if err == nil || !transientErr(err) || attempt >= 2 {
			return m, err
		}
		backoff := time.Duration(attempt+1) * 5 * time.Second
		fmt.Printf("RETRY stream=%v attempt=%d backoff=%s err=%.120s\n", stream, attempt+1, backoff, err)
		time.Sleep(backoff)
	}
}

// ------------------------------------------------------------------ passes

func endpointPasses() {
	var samples = []*sample{
		{label: "GET /health", path: "/health"},
		{label: "GET /status", path: "/status"},
		{label: "GET /v1/models", path: "/v1/models"},
	}
	for _, s := range samples {
		for i := 0; i < *n; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t0 := time.Now()
			resp, err := doReq(ctx, "GET", s.path, nil)
			s.add(time.Since(t0))
			code := 0
			if resp != nil {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				code = resp.StatusCode
			}
			cancel()
			if err != nil || code != 200 {
				fmt.Printf("FAIL %s i=%d err=%v code=%d\n", s.label, i, err, code)
			}
		}
		s.report()
	}
}

func nonStreamPass() {
	label := fmt.Sprintf("chat non-stream (%s)", *model)
	s := &sample{label: label}
	for i := 0; i < *n; i++ {
		m, err := chatWithRetry(shortPrompt, false)
		if err != nil {
			fmt.Printf("FAIL %s i=%d err=%.200s\n", label, i, err)
			time.Sleep(300 * time.Millisecond)
			continue
		}
		s.add(m.total)
		fmt.Printf("LAT %-28s i=%2d total=%8.1fms chars=%d reason=%d\n", label, i, ms(m.total), m.chars, m.reasonChars)
		time.Sleep(300 * time.Millisecond)
	}
	s.report()
}

func streamPass() {
	label := fmt.Sprintf("chat stream (%s)", *model)
	ttftDelta := &sample{label: label + " ttft(delta)"}
	ttftText := &sample{label: label + " ttft(text)"}
	total := &sample{label: label + " total"}
	for i := 0; i < *n; i++ {
		m, err := chatWithRetry(shortPrompt, true)
		if err != nil {
			fmt.Printf("FAIL %s i=%d err=%.200s\n", label, i, err)
			time.Sleep(300 * time.Millisecond)
			continue
		}
		ttftDelta.add(m.firstDelta)
		ttftText.add(m.firstText)
		total.add(m.total)
		fmt.Printf("LAT %-28s i=%2d ttftDelta=%7.1fms ttftText=%7.1fms total=%8.1fms chars=%d reason=%d\n",
			label, i, ms(m.firstDelta), ms(m.firstText), ms(m.total), m.chars, m.reasonChars)
		time.Sleep(300 * time.Millisecond)
	}
	ttftDelta.report()
	ttftText.report()
	total.report()
}

func throughputPass() {
	label := fmt.Sprintf("gen throughput (%s)", *model)
	for i := 0; i < 3; i++ {
		m, err := chatWithRetry(mediumPrompt, true)
		if err != nil {
			fmt.Printf("FAIL %s i=%d err=%.200s\n", label, i, err)
			continue
		}
		gen := m.total - m.firstText
		if gen <= 0 || m.chars <= 0 {
			continue
		}
		cps := float64(m.chars) / gen.Seconds()
		tps := cps / 4.0 // rough: ~4 chars/token for English prose
		fmt.Printf("LAT %-28s i=%d gen=%7.1fms chars=%d %6.0f chars/s (~%6.0f tok/s est)\n",
			label, i, ms(gen), m.chars, cps, tps)
	}
}

func concurrencyPass() {
	label := fmt.Sprintf("stream x%d concurrent (%s)", *cc, *model)
	perTotal := &sample{label: label + " total"}
	perText := &sample{label: label + " ttft(text)"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var fails []string
	for i := 0; i < *cc; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m, err := chatWithRetry(shortPrompt, true)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fails = append(fails, fmt.Sprintf("i=%d: %.120s", i, err))
				return
			}
			perTotal.add(m.total)
			perText.add(m.firstText)
			fmt.Printf("LAT %-28s i=%2d ttftText=%7.1fms total=%8.1fms chars=%d\n", label, i, ms(m.firstText), ms(m.total), m.chars)
		}(i)
	}
	wg.Wait()
	perTotal.report()
	perText.report()
	for _, f := range fails {
		fmt.Printf("FAIL %s %s\n", label, f)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

// ------------------------------------------------------------------ main

func main() {
	flag.Parse()
	fmt.Printf("== latency probe bridge=%s model=%s n=%d concurrency=%d at %s\n",
		*bridge, *model, *n, *cc, time.Now().Format(time.RFC3339))

	fmt.Println("\n-- endpoint overhead (no upstream) --")
	endpointPasses()

	fmt.Println("\n-- non-stream chat --")
	nonStreamPass()

	fmt.Println("\n-- streaming chat --")
	streamPass()

	fmt.Println("\n-- generation throughput (medium prompt) --")
	throughputPass()

	fmt.Println("\n-- concurrency --")
	concurrencyPass()

	fmt.Println("\n-- done --")
}
