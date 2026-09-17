// scripts/e2e/main.go
//
// E2E agent harness: drives the bridge over the OpenAI streaming protocol
// with a REAL tool loop, the way an agent client (Zed/Cognix) does —
// send tools, receive streamed tool_calls, execute them against a sandbox
// workspace, feed results back, repeat until finish_reason=stop.
//
// Each task ends with a hard verification against the workspace, so a turn
// that "stops mid-task" (the production bug we are hunting) fails loudly
// instead of looking like a successful stream.
//
// Usage:
//
//	go run ./scripts/e2e -bridge http://127.0.0.1:39997 -model glm-4.7 \
//	                    -task analyze,crud,cmd,multi -repeats 2
//
// Every turn and result is logged as RESULT:/TURN:/FAIL: lines so a whole
// matrix run can be summarized with grep.

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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	bridge   = flag.String("bridge", "http://127.0.0.1:3001", "bridge base URL")
	model    = flag.String("model", "glm-4.7", "model name to request")
	token    = flag.String("token", "Waguri", "bridge auth token")
	taskSel  = flag.String("task", "analyze,crud,cmd,multi,big", "comma-separated task names")
	repeats  = flag.Int("repeats", 1, "how many times to run the whole task set")
	wsRoot   = flag.String("ws", "/tmp/e2e-ws", "workspace root (task dirs are created under it)")
	turnWait = flag.Duration("turn-timeout", 300*time.Second, "per-turn wall-clock timeout")
	maxTurns = flag.Int("max-turns", 15, "tool-loop turn limit per task")
	verbose  = flag.Bool("v", false, "print per-turn info")
	retries  = flag.Int("retries", 4, "retries per turn for transient infra errors (WAF 405, model capacity, captcha empty)")
)

// transientInfraErr reports whether a turn error is server/infra noise
// (Aliyun WAF bursts, model capacity, a dead Q mid-run) rather than a
// real agent/bridge failure. Such turns are retried with backoff, the
// same way a production agent client would.
func transientInfraErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, pat := range []string{
		"MODEL_CONCURRENCY_LIMIT",
		"at capacity",
		"405", // Aliyun WAF block page (burst)
		"captcha generation returned empty payload", // Q died mid-run
		"captcha gate",
		"502 Bad Gateway",
		"503",
		"504",
		"EOF", // connection reset mid-burst
	} {
		if strings.Contains(s, pat) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- harness

type toolCallAcc struct {
	id   string
	name string
	args strings.Builder
}

type turnResult struct {
	content   string
	reasoning string
	calls     []toolCallAcc
	finish    string
}

var logMu sync.Mutex

func logf(format string, a ...interface{}) {
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Printf(format+"\n", a...)
}

func failf(format string, a ...interface{}) error {
	return fmt.Errorf(format, a...)
}

func chatOnce(ctx context.Context, msgs []interface{}, tools []interface{}) (turnResult, error) {
	var tr turnResult
	body := map[string]interface{}{
		"model":    *model,
		"messages": msgs,
		"stream":   true,
	}
	if tools != nil {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	bj, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", *bridge+"/v1/chat/completions", bytes.NewReader(bj))
	if err != nil {
		return tr, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+*token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return tr, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return tr, failf("http %d: %s", resp.StatusCode, string(b))
	}

	var contentB strings.Builder
	var reasonB strings.Builder
	byIdx := map[int]*toolCallAcc{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return tr, failf("bad SSE chunk: %v: %.200s", err, data)
		}
		errObj, _ := chunk["error"].(map[string]interface{})
		if errObj != nil {
			msg, _ := errObj["message"].(string)
			return tr, failf("bridge error chunk: %s", msg)
		}
		choices, _ := chunk["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		ch0, _ := choices[0].(map[string]interface{})
		if fr, ok := ch0["finish_reason"].(string); ok && fr != "" {
			tr.finish = fr
		}
		delta, _ := ch0["delta"].(map[string]interface{})
		if delta == nil {
			continue
		}
		if c, ok := delta["content"].(string); ok {
			contentB.WriteString(c)
		}
		if r, ok := delta["reasoning_content"].(string); ok {
			reasonB.WriteString(r)
		}
		if tcs, ok := delta["tool_calls"].([]interface{}); ok {
			for _, t := range tcs {
				tm, _ := t.(map[string]interface{})
				if tm == nil {
					continue
				}
				idx := 0
				switch v := tm["index"].(type) {
				case float64:
					idx = int(v)
				}
				acc := byIdx[idx]
				if acc == nil {
					acc = &toolCallAcc{}
					byIdx[idx] = acc
				}
				if id, ok := tm["id"].(string); ok && id != "" {
					acc.id = id
				}
				if fn, ok := tm["function"].(map[string]interface{}); ok {
					if n, ok := fn["name"].(string); ok && n != "" {
						acc.name = n
					}
					if a, ok := fn["arguments"].(string); ok {
						acc.args.WriteString(a)
					}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return tr, failf("stream read: %v", err)
	}
	tr.content = contentB.String()
	tr.reasoning = reasonB.String()
	for i := 0; i < len(byIdx); i++ {
		if c, ok := byIdx[i]; ok {
			tr.calls = append(tr.calls, *c)
		}
	}
	return tr, nil
}

// ---------------------------------------------------------------- tools

func wsPath(ws, p string) (string, error) {
	if p == "" {
		return "", failf("tool path is empty")
	}
	if filepath.IsAbs(p) {
		return "", failf("tool path must be relative to the workspace, got absolute %q", p)
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", failf("tool path escapes the workspace: %q", p)
	}
	return filepath.Join(ws, clean), nil
}

func readFileTool(ws string, args map[string]interface{}) string {
	rel, _ := args["path"].(string)
	full, err := wsPath(ws, rel)
	if err != nil {
		return "Error: " + err.Error()
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "Error: " + err.Error()
	}
	s := string(b)
	if len(s) > 32*1024 {
		s = s[:32*1024] + "\n... [truncated]"
	}
	return s
}

func writeFileTool(ws string, args map[string]interface{}) string {
	rel, _ := args["path"].(string)
	content, _ := args["content"].(string)
	full, err := wsPath(ws, rel)
	if err != nil {
		return "Error: " + err.Error()
	}
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return "Error: " + err.Error()
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("wrote %s (%d bytes)", rel, len(content))
}

func editFileTool(ws string, args map[string]interface{}) string {
	rel, _ := args["path"].(string)
	oldText, _ := args["old_text"].(string)
	newText, _ := args["new_text"].(string)
	full, err := wsPath(ws, rel)
	if err != nil {
		return "Error: " + err.Error()
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "Error: " + err.Error()
	}
	s := string(b)
	switch strings.Count(s, oldText) {
	case 0:
		return failf("old_text not found in %s", rel).Error()
	case 1:
		s = strings.Replace(s, oldText, newText, 1)
	default:
		return failf("old_text matches %d locations in %s", strings.Count(s, oldText), rel).Error()
	}
	if err := os.WriteFile(full, []byte(s), 0644); err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("edited %s", rel)
}

func deletePathTool(ws string, args map[string]interface{}) string {
	rel, _ := args["path"].(string)
	full, err := wsPath(ws, rel)
	if err != nil {
		return "Error: " + err.Error()
	}
	if err := os.RemoveAll(full); err != nil {
		return "Error: " + err.Error()
	}
	return "deleted " + rel
}

func listDirTool(ws string, args map[string]interface{}) string {
	rel, _ := args["path"].(string)
	if rel == "" {
		rel = "."
	}
	full, err := wsPath(ws, rel)
	if err != nil {
		return "Error: " + err.Error()
	}
	ents, err := os.ReadDir(full)
	if err != nil {
		return "Error: " + err.Error()
	}
	var sb strings.Builder
	for _, e := range ents {
		kind := "file"
		if e.IsDir() {
			kind = "dir"
		}
		sb.WriteString(e.Name() + " (" + kind + ")\n")
	}
	return sb.String()
}

func runCommandTool(ws string, args map[string]interface{}) string {
	cmdStr, _ := args["command"].(string)
	if cmdStr == "" {
		return "Error: command is empty"
	}
	timeout := 45 * time.Second
	if t, ok := args["timeout"].(float64); ok && t > 0 && t < 120 {
		timeout = time.Duration(t) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = ws
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	s := out.String()
	if len(s) > 32*1024 {
		s = s[:32*1024] + "\n... [truncated]"
	}
	if err != nil {
		s += "\n(exit error: " + err.Error() + ")"
	}
	return s
}

func execTool(ws string, c toolCallAcc) string {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(c.args.String()), &args); err != nil {
		return failf("invalid tool arguments JSON: %v (raw: %.200s)", err, c.args.String()).Error()
	}
	switch c.name {
	case "read_file":
		return readFileTool(ws, args)
	case "write_file":
		return writeFileTool(ws, args)
	case "edit_file":
		return editFileTool(ws, args)
	case "delete_path":
		return deletePathTool(ws, args)
	case "list_directory":
		return listDirTool(ws, args)
	case "run_command":
		return runCommandTool(ws, args)
	default:
		return failf("unknown tool %q", c.name).Error()
	}
}

func toolDefs() []interface{} {
	f := func(name, desc string, props map[string]interface{}, required []string) interface{} {
		return map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": desc,
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": props,
					"required":   required,
				},
			},
		}
	}
	str := func(d string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": d}
	}
	num := func(d string) map[string]interface{} {
		return map[string]interface{}{"type": "number", "description": d}
	}
	return []interface{}{
		f("read_file", "Read a file from the workspace", map[string]interface{}{"path": str("relative path")}, []string{"path"}),
		f("write_file", "Create or overwrite a file", map[string]interface{}{"path": str("relative path"), "content": str("full file content")}, []string{"path", "content"}),
		f("edit_file", "Edit a file by exact string replacement", map[string]interface{}{"path": str("relative path"), "old_text": str("text to find (must be unique)"), "new_text": str("replacement text")}, []string{"path", "old_text", "new_text"}),
		f("delete_path", "Delete a file or directory", map[string]interface{}{"path": str("relative path")}, []string{"path"}),
		f("list_directory", "List workspace directory entries", map[string]interface{}{"path": str("relative path (default .)")}, nil),
		f("run_command", "Run a shell command inside the workspace", map[string]interface{}{"command": str("shell command"), "timeout": num("timeout in seconds (max 120)")}, []string{"command"}),
	}
}

// ---------------------------------------------------------------- tasks

type seedFile struct {
	path    string
	content string
}

type taskDef struct {
	name    string
	seed    []seedFile
	prompt  string
	verify  func(ws string) error
	slowCmd bool // task involves go build/run — allow longer turns
}

func mustRead(ws, rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(ws, rel))
	if err != nil {
		return "", failf("%s missing: %v", rel, err)
	}
	return string(b), nil
}

func taskSet() []taskDef {
	return []taskDef{
		{
			name: "analyze",
			seed: []seedFile{
				{"README.md", "# mini-calc\n\nA tiny Go calculator.\n\nUsage: `go run .`\n"},
				{"go.mod", "module mini\n\ngo 1.21\n"},
				{"calc.go", "package main\n\nimport \"fmt\"\n\nfunc main() {\n\ta, b := 6, 7\n\tfmt.Printf(\"%d+%d=%d\\n\", a, b, a+b)\n}\n"},
			},
			prompt: "Phân tích project trong workspace: đọc README.md và file .go, rồi tạo file ANALYSIS.md tóm tắt (mục đích project, các file, ngôn ngữ). Chỉ làm việc trong workspace.",
			verify: func(ws string) error {
				s, err := mustRead(ws, "ANALYSIS.md")
				if err != nil {
					return err
				}
				if len(s) < 80 {
					return failf("ANALYSIS.md too short (%d bytes)", len(s))
				}
				return nil
			},
		},
		{
			name: "crud",
			seed: []seedFile{
				{"notes.txt", "seed data\n"},
			},
			prompt: "Làm đúng theo các bước: 1) Tạo file alpha.txt với nội dung đúng là 'alpha-v1' (không thêm gì khác). 2) Tạo file beta.txt với nội dung 'beta-v1'. 3) Sửa alpha.txt: thay 'alpha-v1' bằng 'alpha-v2' bằng edit. 4) Xóa file beta.txt. Xong rồi báo 'done'.",
			verify: func(ws string) error {
				a, err := mustRead(ws, "alpha.txt")
				if err != nil {
					return err
				}
				if strings.TrimSpace(a) != "alpha-v2" {
					return failf("alpha.txt = %q, want alpha-v2", a)
				}
				if _, err := os.Stat(filepath.Join(ws, "beta.txt")); err == nil {
					return failf("beta.txt still exists")
				}
				return nil
			},
		}, {
			name:   "cmdfile",
			prompt: "Chạy shell command 'printf hello > out.txt' trong workspace, sau đó đọc out.txt và tạo file report.txt chứa nội dung của out.txt.",
			verify: func(ws string) error {
				o, err := mustRead(ws, "out.txt")
				if err != nil {
					return err
				}
				if strings.TrimSpace(o) != "hello" {
					return failf("out.txt = %q, want 'hello'", o)
				}
				r, err := mustRead(ws, "report.txt")
				if err != nil {
					return err
				}
				if !strings.Contains(r, "hello") {
					return failf("report.txt = %q, want contains 'hello'", r)
				}
				return nil
			},
		},
		{
			// Stall-reproduction task (docs/log_stall.md class): a Vietnamese
			// "phân tích dự án" request like the Cognix chats. The model MUST
			// list directories and read several files before it can satisfy the
			// verification tokens — a text-only turn (announcement or made-up
			// answer) fails the verify, which is exactly the "nói mà không làm"
			// stall we are hunting.
			name: "deepdive",
			seed: []seedFile{
				{"README.md", "# dataflow\n\nA tiny Go service. `go run ./cmd/server` starts an HTTP server on :8080 that serves store stats.\n"},
				{"go.mod", "module dataflow\n\ngo 1.21\n"},
				{"cmd/server/main.go", "package main\n\nimport (\n\t\"fmt\"\n\n\t\"dataflow/internal/store\"\n\t\"dataflow/internal/web\"\n)\n\nfunc main() {\n\ts := store.New()\n\ts.Set(\"a\", 1)\n\ts.Set(\"b\", 2)\n\ts.Set(\"c\", 3)\n\tfmt.Println(web.Describe(s))\n}\n"},
				{"internal/store/store.go", "package store\n\n// Store is a trivial string->int map with a Len method.\n// BUG: Len returns 0 always — fix it.\ntype Store struct {\n\tm map[string]int\n}\n\nfunc New() *Store { return &Store{m: map[string]int{}} }\n\nfunc (s *Store) Set(k string, v int) { s.m[k] = v }\n\nfunc (s *Store) Get(k string) int { return s.m[k] }\n\nfunc (s *Store) Len() int { return 0 }\n"},
				{"internal/store/store_test.go", "package store\n\nimport \"testing\"\n\nfunc TestLen(t *testing.T) {\n\ts := New()\n\ts.Set(\"a\", 1)\n\ts.Set(\"b\", 2)\n\ts.Set(\"c\", 3)\n\tif s.Len() != 3 {\n\t\tt.Fatalf(\"Len() = %d, want 3\", s.Len())\n\t}\n}\n"},
				{"internal/web/server.go", "package web\n\nimport (\n\t\"fmt\"\n\n\t\"dataflow/internal/store\"\n)\n\n// Describe returns a one-line summary of the store.\nfunc Describe(s *store.Store) string {\n\treturn fmt.Sprintf(\"store has %d entries\", s.Len())\n}\n"},
			},
			prompt: "Phân tích và đánh giá dự án Go trong workspace này. QUAN TRỌNG: đừng trả lời từ trí nhớ — hãy thực sự dùng list_directory và read_file để đọc. Các bước: 1) list_directory thư mục gốc và các thư mục con (cmd, internal...). 2) Đọc go.mod, README.md và TẤT CẢ các file .go (kể cả trong internal/store và internal/web). 3) Tạo file ANALYSIS.md bằng write_file với nội dung: mục đích dự án, kiến trúc các package, và chính xác bug đang có trong internal/store (nhìn trong code, đừng đoán). ANALYSIS.md PHẢI chứa các từ: 'dataflow', 'Len', 'store', '8080' — đó là bằng chứng bạn đã đọc file thật. Xong thì báo 'done'.",
			verify: func(ws string) error {
				s, err := mustRead(ws, "ANALYSIS.md")
				if err != nil {
					return err
				}
				if len(s) < 300 {
					return failf("ANALYSIS.md too short (%d bytes)", len(s))
				}
				for _, tok := range []string{"dataflow", "Len", "store", "8080"} {
					if !strings.Contains(s, tok) {
						return failf("ANALYSIS.md missing evidence token %q (model answered without reading files?)", tok)
					}
				}
				return nil
			},
		},
		{
			// Stall-reproduction task: a question whose correct answer requires
			// reading the seeded files (the docs class: deploy/concurrency
			// questions asked against a real project). The answer must be
			// written to ANSWER.md so the harness can verify it came from the
			// files, not from a text-only announcement.
			name: "answerin",
			seed: []seedFile{
				{"README.md", "# dataflow\n\nA tiny Go service. `go run ./cmd/server` starts an HTTP server on :8080 that serves store stats.\n"},
				{"go.mod", "module dataflow\n\ngo 1.21\n"},
				{"cmd/server/main.go", "package main\n\nimport (\n\t\"fmt\"\n\n\t\"dataflow/internal/store\"\n\t\"dataflow/internal/web\"\n)\n\nfunc main() {\n\ts := store.New()\n\ts.Set(\"a\", 1)\n\ts.Set(\"b\", 2)\n\ts.Set(\"c\", 3)\n\tfmt.Println(web.Describe(s))\n}\n"},
				{"internal/store/store.go", "package store\n\n// Store is a trivial string->int map with a Len method.\n// BUG: Len returns 0 always — fix it.\ntype Store struct {\n\tm map[string]int\n}\n\nfunc New() *Store { return &Store{m: map[string]int{}} }\n\nfunc (s *Store) Set(k string, v int) { s.m[k] = v }\n\nfunc (s *Store) Get(k string) int { return s.m[k] }\n\nfunc (s *Store) Len() int { return 0 }\n"},
			},
			prompt: "Đọc dự án Go trong workspace rồi trả lời 2 câu hỏi, dựa trên NỘI DUNG FILE THỰC TẾ (phải list_directory + read_file trước, không đoán): 1) Server listen ở cổng nào? 2) Trong internal/store có bug gì và cách sửa? Sau khi đã đọc xong, tạo file ANSWER.md (bằng write_file) chứa câu trả lời đầy đủ cả hai câu. Xong báo 'done'.",
			verify: func(ws string) error {
				s, err := mustRead(ws, "ANSWER.md")
				if err != nil {
					return err
				}
				if !strings.Contains(s, "8080") || !strings.Contains(s, "Len") {
					return failf("ANSWER.md missing evidence tokens (want '8080' and 'Len'), got %d bytes", len(s))
				}
				if len(s) < 200 {
					return failf("ANSWER.md too short (%d bytes)", len(s))
				}
				return nil
			},
		},
		{
			name:   "cmd",
			prompt: "Chạy shell command 'printf hello > out.txt' trong workspace, sau đó đọc out.txt và tạo file report.txt chứa nội dung của out.txt.",
			verify: func(ws string) error {
				o, err := mustRead(ws, "out.txt")
				if err != nil {
					return err
				}
				if strings.TrimSpace(o) != "hello" {
					return failf("out.txt = %q, want 'hello'", o)
				}
				r, err := mustRead(ws, "report.txt")
				if err != nil {
					return err
				}
				if !strings.Contains(r, "hello") {
					return failf("report.txt = %q, want contains 'hello'", r)
				}
				return nil
			},
		},
		{
			name: "big",
			seed: []seedFile{
				{"go.mod", "module bigapp\n\ngo 1.21\n"},
				{"main.go", "package main\n\nimport (\n\t\"fmt\"\n\n\t\"bigapp/store\"\n)\n\nfunc main() {\n\ts := store.New()\n\ts.Set(\"a\", 1)\n\ts.Set(\"b\", 2)\n\tfmt.Println(s.Get(\"a\"), s.Get(\"b\"), s.Len())\n}\n"},
				{"store/store.go", "package store\n\n// Store is a trivial string->int map with a Len method.\n// BUG: Len returns 0 always — fix it.\ntype Store struct {\n\tm map[string]int\n}\n\nfunc New() *Store { return &Store{m: map[string]int{}} }\n\nfunc (s *Store) Set(k string, v int) { s.m[k] = v }\n\nfunc (s *Store) Get(k string) int { return s.m[k] }\n\nfunc (s *Store) Len() int { return 0 }\n"},
			},
			prompt: "Workspace có project Go 'bigapp' với 1 bug: store.Len() luôn trả 0. Các bước: 1) Đọc store/store.go và main.go. 2) Sửa bug Len() để trả số entry thực của map. 3) Chạy 'go run .' — phải in '1 2 2'. 4) Chạy 'go vet ./...'. 5) Tạo file REVIEW.md tóm tắt bug đã sửa (nguyên nhân, cách fix). Hoàn tất thì báo 'done'.",
			verify: func(ws string) error {
				st, err := mustRead(ws, "store/store.go")
				if err != nil {
					return err
				}
				if !strings.Contains(st, "len(s.m)") {
					return failf("store.go Len not fixed")
				}
				r, err := mustRead(ws, "REVIEW.md")
				if err != nil {
					return err
				}
				if len(r) < 60 {
					return failf("REVIEW.md too short")
				}
				return nil
			},
			slowCmd: true,
		},
	}
}

// ---------------------------------------------------------------- runner

func runTask(t taskDef, run int) (dur time.Duration, err error) {
	start := time.Now()
	ws := filepath.Join(*wsRoot, fmt.Sprintf("%s-%d", t.name, run))
	if err := os.RemoveAll(ws); err != nil {
		return time.Since(start), err
	}
	// The workspace must exist even for tasks with no seed files —
	// run_command chdirs into it and would fail on a missing directory.
	if err := os.MkdirAll(ws, 0755); err != nil {
		return time.Since(start), err
	}
	for _, sf := range t.seed {
		full := filepath.Join(ws, sf.path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return time.Since(start), err
		}
		if err := os.WriteFile(full, []byte(sf.content), 0644); err != nil {
			return time.Since(start), err
		}
	}

	msgs := []interface{}{
		map[string]interface{}{
			"role": "system",
			"content": "You are a precise coding agent working inside a workspace directory. " +
				"Use the provided tools for every filesystem/shell operation — never claim an action without executing the tool. " +
				"Paths are relative to the workspace. Finish the task completely, then reply with a short final answer.",
		},
		map[string]interface{}{"role": "user", "content": t.prompt},
	}
	tools := toolDefs()

	totalCalls := 0
	for turn := 1; turn <= *maxTurns; turn++ {
		turnTimeout := *turnWait
		if t.slowCmd {
			turnTimeout += 60 * time.Second
		}
		var tr turnResult
		var err error
		// Retry the same turn on transient infra errors (WAF burst, model
		// capacity, Q death mid-run) — a production agent client does the
		// same. Non-transient errors fail the task immediately.
		for attempt := 0; ; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
			tr, err = chatOnce(ctx, msgs, tools)
			cancel()
			if err == nil || !transientInfraErr(err) || attempt >= *retries {
				break
			}
			backoff := time.Duration(attempt+1) * 15 * time.Second
			logf("RETRY task=%s run=%d turn=%d attempt=%d backoff=%s err=%.180s", t.name, run, turn, attempt+1, backoff, err)
			time.Sleep(backoff)
		}
		if err != nil {
			return time.Since(start), failf("turn %d: %v", turn, err)
		}
		logf("TURN task=%s run=%d turn=%d finish=%s calls=%d contentLen=%d", t.name, run, turn, tr.finish, len(tr.calls), len(tr.content))
		// Stall diagnosis: a turn that stops with NO tool call is the "nói mà
		// không làm" candidate — dump what the model actually said (content)
		// and what it was planning (reasoning) so stalls are characterizable
		// from the client side without server logs.
		if len(tr.calls) == 0 && (tr.finish == "stop" || tr.finish == "") && (len(tr.content) < 400 || len(tr.reasoning) > 0) {
			logf("STALLLOOK task=%s run=%d turn=%d content=%.300q reason=%.300q", t.name, run, turn, tr.content, tr.reasoning)
		}

		if len(tr.calls) > 0 {
			// Record the assistant tool_calls turn exactly as the OpenAI
			// protocol does, then append one tool result per call.
			oaiCalls := make([]interface{}, 0, len(tr.calls))
			for _, c := range tr.calls {
				argsJSON := json.RawMessage(c.args.String())
				if len(argsJSON) == 0 {
					argsJSON = json.RawMessage("{}")
				}
				oaiCalls = append(oaiCalls, map[string]interface{}{
					"id":   orDefault(c.id, "call_fixup"),
					"type": "function",
					"function": map[string]interface{}{
						"name":      c.name,
						"arguments": c.args.String(),
					},
				})
			}
			msgs = append(msgs, map[string]interface{}{
				"role":       "assistant",
				"content":    tr.content,
				"tool_calls": oaiCalls,
			})
			for _, c := range tr.calls {
				totalCalls++
				out := execTool(ws, c)
				id := c.id
				if id == "" {
					id = "call_fixup"
				}
				msgs = append(msgs, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": id,
					"content":      out,
				})
			}
			continue
		}

		// No tool calls: the model produced its final answer.
		if tr.finish == "stop" || tr.finish == "" {
			if err := t.verify(ws); err != nil {
				return time.Since(start), failf("stopped after %d turns but verification failed: %v (final answer: %.300s)", turn, err, tr.content)
			}
			return time.Since(start), nil
		}
		return time.Since(start), failf("turn %d: unexpected finish=%q with no tool calls", turn, tr.finish)
	}
	return time.Since(start), failf("hit max turns (%d) without finishing; %d tool calls total", *maxTurns, totalCalls)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func main() {
	flag.Parse()
	if err := os.MkdirAll(*wsRoot, 0755); err != nil {
		logf("FAIL setup: %v", err)
		os.Exit(1)
	}
	selected := map[string]bool{}
	for _, s := range strings.Split(*taskSel, ",") {
		selected[strings.TrimSpace(s)] = true
	}
	var tasks []taskDef
	for _, t := range taskSet() {
		if selected[t.name] {
			tasks = append(tasks, t)
		}
	}
	if len(tasks) == 0 {
		logf("FAIL: no tasks selected")
		os.Exit(1)
	}

	pass, fail := 0, 0
	for r := 1; r <= *repeats; r++ {
		for _, t := range tasks {
			dur, err := runTask(t, r)
			status := "PASS"
			if err != nil {
				status = "FAIL"
				fail++
			} else {
				pass++
			}
			if err != nil {
				logf("RESULT task=%s run=%d status=%s dur=%s err=%v", t.name, r, status, dur.Round(time.Second), err)
			} else {
				logf("RESULT task=%s run=%d status=%s dur=%s", t.name, r, status, dur.Round(time.Second))
			}
		}
	}
	logf("SUMMARY model=%s pass=%d fail=%d", *model, pass, fail)
	if fail > 0 {
		os.Exit(1)
	}
}
