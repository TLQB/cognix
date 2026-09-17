// Qwen Bridge — chat.qwen.ai Proxy API.
//
// Thin entry point: the whole bridge lives in internal/zbridge (see README
// "Project Structure"). Forked from the Qwen bridge; the transport layer
// (session init, chat completions, SSE parsing) talks to chat.qwen.ai while
// everything else (agent-mode interceptor, stall detection, session pool,
// OpenAI/Anthropic handlers) is inherited unchanged.
//
// Build:  go build -trimpath -ldflags="-s -w" -gcflags="all=-l=4" -o qwen-proxy .
// Run:    ./qwen-proxy --agent-mode

package main

import "qwen-proxy/internal/zbridge"

func main() {
	zbridge.Run()
}
