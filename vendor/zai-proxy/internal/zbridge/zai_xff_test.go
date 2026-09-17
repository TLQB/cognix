package zbridge

import (
	"net"
	"os"
	"testing"
)

// TestXffRotation verifies the POOL strategy (ZAI_XFF_STRATEGY=pool): each
// call to rotateXffIP advances to a different pool address, and a full cycle
// returns to the starting address. Skipped when the operator pinned
// ZAI_XFF_IP/_IPS, since rotation is intentionally disabled in that case.
func TestXffRotation(t *testing.T) {
	if os.Getenv("ZAI_XFF_IP") != "" || os.Getenv("ZAI_XFF_IPS") != "" {
		t.Skip("ZAI_XFF_IP/ZAI_XFF_IPS set; egress rotation is operator-pinned")
	}
	t.Setenv("ZAI_XFF_STRATEGY", "pool")

	pool := xffPool()
	if len(pool) < 2 {
		t.Fatalf("rotation pool too small: %d entries", len(pool))
	}

	start := zaiXffIP()

	first := rotateXffIP()
	if first == start {
		t.Errorf("first rotation returned the same address %q", first)
	}

	// len(pool)-1 further rotations complete the cycle and return to start.
	var cur string
	for i := 1; i < len(pool); i++ {
		cur = rotateXffIP()
	}
	if cur != start {
		t.Errorf("full cycle: got %q, want start %q", cur, start)
	}
}

// TestXffRandomBankStrategy pins the DEFAULT random-bank behavior established
// by the 2026-09-05 WAF research: every draw must be a valid foreign host
// address inside one of the CIDR banks, and consecutive draws must explore
// new identities (the per-identity WAF budget of ~a dozen requests is only
// safe because identities never repeat in practice).
func TestXffRandomBankStrategy(t *testing.T) {
	if os.Getenv("ZAI_XFF_IP") != "" || os.Getenv("ZAI_XFF_IPS") != "" {
		t.Skip("ZAI_XFF_IP/ZAI_XFF_IPS set; egress selection is operator-pinned")
	}
	t.Setenv("ZAI_XFF_STRATEGY", "") // default = random

	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		ip := zaiXffIP()
		if net.ParseIP(ip) == nil {
			t.Fatalf("draw %d: %q is not a valid IP", i, ip)
		}
		matched := false
		for _, cidr := range defaultXffCIDRBanks {
			if _, ipnet, err := net.ParseCIDR(cidr); err == nil && ipnet.Contains(net.ParseIP(ip)) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("draw %d: %q is outside every CIDR bank", i, ip)
		}
		seen[ip] = true
		// rotateXffIP must also draw a fresh bank address in random mode.
		if got := rotateXffIP(); net.ParseIP(got) == nil {
			t.Fatalf("rotate draw %d: %q is not a valid IP", i, got)
		}
	}
	if len(seen) < 45 {
		t.Errorf("expected ~all draws to be distinct identities, got %d/50 distinct", len(seen))
	}
}

// TestXffPoolStrategyEnvOverride locks the escape hatch: ZAI_XFF_STRATEGY=pool
// restores the legacy rotation even though random is the default.
func TestXffPoolStrategyEnvOverride(t *testing.T) {
	if os.Getenv("ZAI_XFF_IP") != "" || os.Getenv("ZAI_XFF_IPS") != "" {
		t.Skip("ZAI_XFF_IP/ZAI_XFF_IPS set; egress selection is operator-pinned")
	}
	t.Setenv("ZAI_XFF_STRATEGY", "pool")
	if xffStrategy() != "pool" {
		t.Fatalf("xffStrategy() = %q, want pool", xffStrategy())
	}
	t.Setenv("ZAI_XFF_STRATEGY", "")
	if xffStrategy() != "random" {
		t.Fatalf("default xffStrategy() = %q, want random", xffStrategy())
	}
}
