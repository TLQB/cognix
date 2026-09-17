package zbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestExtractCaptchaFlowError locks in detection of the Z.AI captcha gate
// error-in-200 (FRONTEND_CAPTCHA_REQUIRED). Both data.error and the nested
// data.data.error duplication must be recognized — both shapes were captured
// live during the 2026-09-02 captcha probes (see EXP_CAPTCHA_RESULTS.md and
// testdata/exp_captcha_FULL_20260902_015305.log), and they can appear
// independently in the same SSE stream.
func TestExtractCaptchaFlowError(t *testing.T) {
	cases := []struct {
		name       string
		json       string
		wantFound  bool
		wantType   string
		wantCode   string
		wantDetail string
	}{
		{
			// Live capture: verify_failed / F018 (param already consumed —
			// reuse-captcha #2/#3). Error object duplicated at both depths.
			name:       "nested verify_failed F018",
			json:       `{"data":{"data":{"done":true,"error":{"captcha_error_type":"verify_failed","code":"FRONTEND_CAPTCHA_REQUIRED","detail":"Captcha verification failed. Please verify again and retry.","error_code":"FRONTEND_CAPTCHA_REQUIRED","verify_code":"F018"}},"done":true,"error":{"captcha_error_type":"verify_failed","code":"FRONTEND_CAPTCHA_REQUIRED","detail":"Captcha verification failed. Please verify again and retry.","error_code":"FRONTEND_CAPTCHA_REQUIRED","verify_code":"F018"}},"type":"chat:completion"}`,
			wantFound:  true,
			wantType:   "verify_failed",
			wantCode:   "F018",
			wantDetail: "Captcha verification failed. Please verify again and retry.",
		},
		{
			// Live capture: missing_param (no captcha_verify_param at all).
			name:       "missing_param",
			json:       `{"data":{"error":{"captcha_error_type":"missing_param","code":"FRONTEND_CAPTCHA_REQUIRED","detail":"Please refresh the page to update the app, then try again.","error_code":"FRONTEND_CAPTCHA_REQUIRED"}},"done":true}`,
			wantFound:  true,
			wantType:   "missing_param",
			wantCode:   "",
			wantDetail: "Please refresh the page to update the app, then try again.",
		},
		{
			// Verify failure with verify_code F019 via error_code only
			// (guards the error_code fallback path).
			name:       "verify_failed F019 via error_code only",
			json:       `{"data":{"error":{"captcha_error_type":"verify_failed","error_code":"FRONTEND_CAPTCHA_REQUIRED","detail":"Captcha verification failed. Please verify again and retry.","verify_code":"F019"}}}`,
			wantFound:  true,
			wantType:   "verify_failed",
			wantCode:   "F019",
			wantDetail: "Captcha verification failed. Please verify again and retry.",
		},
		{
			// Non-captcha errors must NOT be classified as captcha flow.
			name:      "non-captcha error ignored",
			json:      `{"data":{"error":{"code":"RATE_LIMITED","detail":"too many requests"}}}`,
			wantFound: false,
		},
		{
			// Normal completion content — no error at all.
			name:      "normal payload ignored",
			json:      `{"data":{"edit_index":0,"edit_content":"OK","phase":"other"},"type":"chat:completion"}`,
			wantFound: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var j map[string]interface{}
			if err := json.Unmarshal([]byte(tc.json), &j); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			capErr := extractCaptchaFlowError(j)
			if !tc.wantFound {
				if capErr != nil {
					t.Fatalf("expected no captcha error, got %+v", capErr)
				}
				return
			}
			if capErr == nil {
				t.Fatal("expected captchaFlowError, got nil")
			}
			if capErr.errorType != tc.wantType {
				t.Errorf("errorType = %q, want %q", capErr.errorType, tc.wantType)
			}
			if capErr.verifyCode != tc.wantCode {
				t.Errorf("verifyCode = %q, want %q", capErr.verifyCode, tc.wantCode)
			}
			if capErr.detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", capErr.detail, tc.wantDetail)
			}
		})
	}
}

// TestCaptchaFlowErrorMessage verifies the human-readable error format
// surfaced to clients/logs.
func TestCaptchaFlowErrorMessage(t *testing.T) {
	e := &captchaFlowError{
		detail:     "Captcha verification failed. Please verify again and retry.",
		errorType:  "verify_failed",
		verifyCode: "F018",
	}
	got := e.Error()
	want := "Z.AI captcha error: Captcha verification failed. Please verify again and retry. (captcha_error_type: verify_failed, verify_code: F018)"
	if got != want {
		t.Errorf("Error() = %q\nwant %q", got, want)
	}

	e2 := &captchaFlowError{detail: "Please refresh the page to update the app, then try again.", errorType: "missing_param"}
	want2 := "Z.AI captcha error: Please refresh the page to update the app, then try again. (captcha_error_type: missing_param)"
	if e2.Error() != want2 {
		t.Errorf("Error() = %q\nwant %q", e2.Error(), want2)
	}
}

// TestStreamSSEResponseCaptchaGate feeds the exact F018 SSE body captured
// live (testdata/exp_captcha_FULL_20260902_015305.log, reuse-captcha #2)
// through streamSSEResponse and asserts it returns a typed captchaFlowError
// with emitted=false — the condition sendToZAIStream uses to decide a retry
// is safe. The follow-up variant verifies the guard: content streamed BEFORE
// the gate error flips emitted=true, so a mid-stream captcha error is never
// retried (that would duplicate content).
func TestStreamSSEResponseCaptchaGate(t *testing.T) {
	f018Body := "data: " + `{"data":{"data":{"done":true,"error":{"captcha_error_type":"verify_failed","code":"FRONTEND_CAPTCHA_REQUIRED","detail":"Captcha verification failed. Please verify again and retry.","error_code":"FRONTEND_CAPTCHA_REQUIRED","verify_code":"F018"}},"done":true,"error":{"captcha_error_type":"verify_failed","code":"FRONTEND_CAPTCHA_REQUIRED","detail":"Captcha verification failed. Please verify again and retry.","error_code":"FRONTEND_CAPTCHA_REQUIRED","verify_code":"F018"}},"type":"chat:completion"}` + "\n\ndata: [DONE]\n\n"

	t.Run("gate before any content — retryable", func(t *testing.T) {
		ch := make(chan ZAIResult, 10)
		err := streamSSEResponse(strings.NewReader(f018Body), ch, "test-req-id")
		close(ch)
		capErr, ok := err.(*captchaFlowError)
		if !ok {
			t.Fatalf("expected *captchaFlowError, got %T: %v", err, err)
		}
		if capErr.emitted {
			t.Error("emitted=true, want false (nothing was streamed)")
		}
		if capErr.verifyCode != "F018" || capErr.errorType != "verify_failed" {
			t.Errorf("verifyCode=%q errorType=%q, want F018/verify_failed", capErr.verifyCode, capErr.errorType)
		}
		for r := range ch {
			if r.Err != nil || r.Chunk != "" || r.Reasoning != "" {
				t.Errorf("unexpected stream result before the gate: %+v", r)
			}
		}
	})

	t.Run("gate after content — NOT retryable", func(t *testing.T) {
		body := "data: " + `{"data":{"edit_index":0,"edit_content":"partial answer","phase":"other"},"type":"chat:completion"}` + "\n\n" + f018Body
		ch := make(chan ZAIResult, 10)
		err := streamSSEResponse(strings.NewReader(body), ch, "test-req-id")
		close(ch)
		capErr, ok := err.(*captchaFlowError)
		if !ok {
			t.Fatalf("expected *captchaFlowError, got %T: %v", err, err)
		}
		if !capErr.emitted {
			t.Error("emitted=false, want true (content was already streamed — retry would duplicate it)")
		}
	})
}

// TestXffPoolGoogleLast locks in the XFF pool ordering: the Google DNS
// addresses (8.8.8.8, 8.8.4.4) must be the LAST candidates, not the first.
// Observed live on 2026-09-02: Aliyun's WAF blocklists these two first, so
// every request round wasted 2-3 rotations on them before reaching a good
// address. Keeping them last minimizes wasted rotations.
func TestXffPoolGoogleLast(t *testing.T) {
	pool := xffPool()
	n := len(pool)
	if n < 2 {
		t.Fatalf("pool too small: %d", n)
	}
	for i := n - 2; i < n; i++ {
		if pool[i] != "8.8.8.8" && pool[i] != "8.8.4.4" {
			t.Errorf("pool[%d] = %q, expected a Google DNS address in the last two slots", i, pool[i])
		}
	}
	for i := 0; i < n-2; i++ {
		if pool[i] == "8.8.8.8" || pool[i] == "8.8.4.4" {
			t.Errorf("pool[%d] = %q, Google DNS address must not appear before the last two slots", i, pool[i])
		}
	}
}
