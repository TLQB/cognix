package zbridge

import "testing"

func TestStatusFromErrorQuota(t *testing.T) {
	cases := []struct {
		msg  string
		want int
	}{
		{`Qwen error (200 JSON): {"success":false,"data":{"code":"RateLimited","details":"You've reached the upper limit for today's usage.","num":10}}`, 429},
		{`Qwen error (200 JSON): {"data":{"code":"RateLimited","template":"You have reached the daily usage limit. Please wait {{num}} hours before trying again."}}`, 429},
		{`Qwen connection error: 503 boom`, 503},
	}
	for _, c := range cases {
		if got := statusFromError(c.msg); got != c.want {
			t.Errorf("statusFromError(%q) = %d, want %d", c.msg, got, c.want)
		}
	}
}
