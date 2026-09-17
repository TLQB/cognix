// Code moved from the original main.go monolith during the internal/ restructure.
// See README "Project Structure". Part of the Qwen bridge core (package zbridge).

package zbridge

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// TYPE DEFINITIONS
// ============================================================================

// ---------- Qwen types ----------

type Features struct {
	WebSearch     bool `json:"webSearch"`
	AutoWebSearch bool `json:"autoWebSearch"`
	Thinking      bool `json:"thinking"`
	ImageGen      bool `json:"imageGen"`
	PreviewMode   bool `json:"previewMode"`
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type SessionState struct {
	mu           sync.Mutex
	Token        string
	UserID       string
	UserName     string
	ChatID       string
	Messages     []Message
	SaltKey      string
	FeVersion    string
	Features     Features
	Initialized  bool
	Initializing bool
}

type QwenResult struct {
	Chunk     string
	FullText  string
	Reasoning string
	Err       error
}

type QwenSendOptions struct {
	Model             string
	WebSearch         *bool
	Thinking          *bool
	ImageGen          *bool
	PreviewMode       *bool
	ChatID            string
	Messages          []Message
	ClientMessagesRaw json.RawMessage
	ReasoningEffort   string // "high" or "max"; only forwarded if model supports it
	// RequestID carries the handler-generated client-visible request id, so
	// debug log lines emitted while the upstream SSE stream is parsed can be
	// attributed to their request when several are in flight (issue #36).
	// Purely local: never reaches the upstream payload.
	RequestID string
}

type ResponseResult struct {
	Content      string
	Text         string
	Prompt       string
	FinishReason string
	Reasoning    string
}

// ---------- Captcha JSON struct types ----------

type InitCaptchaResponse struct {
	CertifyID string `json:"CertifyId"`
}

type CVP struct {
	CertifyID   string `json:"certifyId"`
	Data        string `json:"data"`
	DeviceToken string `json:"deviceToken"`
	SceneID     string `json:"sceneId"`
}

type VerifyCaptchaResponse struct {
	Success bool `json:"Success"`
	Result  struct {
		VerifyResult  bool   `json:"VerifyResult"`
		SecurityToken string `json:"securityToken"`
		CertifyID     string `json:"certifyId"`
	} `json:"Result"`
}

type FinalPayload struct {
	CertifyID     string `json:"certifyId"`
	IsSign        bool   `json:"isSign"`
	SceneID       string `json:"sceneId"`
	SecurityToken string `json:"securityToken"`
}

type TrackList struct {
	FI        string `json:"fi"`
	KS        string `json:"ks"`
	MC        string `json:"mc"`
	MP        string `json:"mp"`
	MU        string `json:"mu"`
	StartTime int64  `json:"startTime"`
	TC        string `json:"tc"`
	TE        string `json:"te"`
	TMV       string `json:"tmv"`
}

type Track struct {
	TrackList      TrackList `json:"TrackList"`
	TrackStartTime int64     `json:"TrackStartTime"`
	VerifyTime     int64     `json:"VerifyTime"`
	Arg            string    `json:"arg"`
}

// ============================================================================
// GLOBAL STATE
// ============================================================================

// ---------- Captcha globals ----------

var (
	verbose  bool
	gRunning atomic.Bool
	dbMu     sync.Mutex
	logMu    sync.Mutex
)

// ---------- Qwen globals ----------

var session = &SessionState{
	UserName:  "Guest",
	SaltKey:   SALT_KEY,
	FeVersion: DEFAULT_FE_VERSION,
	Features:  Features{Thinking: true}, // enable_thinking on by default
}

type ModelInfo struct {
	ID           string
	Name         string
	Description  string
	Capabilities map[string]interface{}
}

var (
	modelsCache     []ModelInfo
	modelsCacheTime time.Time
	modelsCacheMu   sync.Mutex
)

const modelsCacheTTL = 5 * time.Minute

// Fallback if the Qwen models API is unreachable and cache is empty
// (verified live via /api/v2/models, 2026-08-31).
var fallbackModels = []ModelInfo{
	{ID: "qwen3.8-max", Name: "Qwen3.8-Max", Description: "Flagship model, excels at complex reasoning tasks"},
	{ID: "qwen3.7-plus", Name: "Qwen3.7-Plus", Description: "Newer high-performance model"},
	{ID: "qwen3.7-max", Name: "Qwen3.7-Max", Description: "Max-tier variant"},
	{ID: "qwen3.6-plus", Name: "Qwen3.6-Plus", Description: "Balanced default model"},
	{ID: "qwen3.6-max-preview", Name: "Qwen3.6-Max-Preview", Description: "Preview max model"},
	{ID: "qwen3.5-plus", Name: "Qwen3.5-Plus", Description: "Previous-generation plus model"},
	{ID: "qwen3.5-flash", Name: "Qwen3.5-Flash", Description: "Fast, lightweight model"},
	{ID: "qwen3-coder-plus", Name: "Qwen3-Coder-Plus", Description: "Coding-specialised model"},
}

// ---------- Per-model feature state (dynamic, model-aware) ----------

// ModelFeatureState tracks per-model feature configuration.
// IncludeAll: when true, ALL server capabilities are sent to /completions.
// Overrides: user-supplied per-model feature overrides (snake_case keys).
type ModelFeatureState struct {
	IncludeAll bool
	Overrides  map[string]interface{}
}

var (
	modelFeatureStates   = make(map[string]*ModelFeatureState)
	modelFeatureStatesMu sync.Mutex
)

// normalizeFeatureKey converts a camelCase key to snake_case.
// No alias mapping — users must use the real server capability key name.
// Special handling for reasoning/thinking -> enable_thinking is done in featuresHandler.
