package zbridge

import "testing"

// TestIsRetryableUpstreamStatus locks in which upstream Qwen statuses the
// bridge retries internally before surfacing an error to the client. 429 must
// stay excluded — fclaude applies its own client-side backoff for rate limits
// (see agent_utils.py), and retrying them here would only add latency.
func TestIsRetryableUpstreamStatus(t *testing.T) {
	retryable := []int{500, 502, 503, 504}
	for _, code := range retryable {
		if !isRetryableUpstreamStatus(code) {
			t.Errorf("expected %d to be retryable", code)
		}
	}

	notRetryable := []int{200, 201, 204, 400, 401, 403, 404, 405, 429}
	for _, code := range notRetryable {
		if isRetryableUpstreamStatus(code) {
			t.Errorf("expected %d to NOT be retryable", code)
		}
	}
}
