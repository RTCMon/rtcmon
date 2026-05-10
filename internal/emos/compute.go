// Package emos implements the eMOS computation algorithm from RFC §3.7
// (simplified R-factor model). It is intentionally free of I/O so the
// algorithm can be unit-tested without a database.
package emos

const (
	// DefaultLossCoeff (K) is the high-loss impairment coefficient for the
	// Ie formula when loss% >= 5. Calibrated against known reference points;
	// override via EMOS_LOSS_COEFF env var at startup.
	DefaultLossCoeff = 7.5
)

// ComputeEMOS returns the estimated MOS (1.0–5.0) and outcome classification
// for a session, given median packet-loss rate (0.0–1.0), median RTT in
// milliseconds, and the high-loss coefficient K (pass DefaultLossCoeff when
// no per-deployment tuning is needed).
func ComputeEMOS(lossRate, rttMs, lossCoeff float64) (emos float32, outcome string) {
	lossPct := lossRate * 100

	// Ie — packet-loss impairment.
	var ie float64
	switch {
	case lossPct < 1:
		ie = 0
	case lossPct < 5:
		ie = (lossPct - 1) * 3
	default:
		ie = 12 + (lossPct-5)*lossCoeff
	}

	// Id — RTT impairment.
	var id float64
	switch {
	case rttMs < 50:
		id = 0
	case rttMs < 250:
		id = (rttMs - 50) * 0.04
	default:
		id = 8 + (rttMs-250)*0.025
	}

	// R-factor: clamp to [0, 100] so the MOS polynomial stays in its valid range.
	r := 93.2 - ie - id
	if r < 0 {
		r = 0
	}
	if r > 100 {
		r = 100
	}

	mos := 1 + 0.035*r + r*(r-60)*(100-r)*7e-6
	if mos < 1.0 {
		mos = 1.0
	}
	if mos > 5.0 {
		mos = 5.0
	}

	switch {
	case mos >= 3.5:
		outcome = "success"
	case mos >= 2.0:
		outcome = "degraded"
	default:
		outcome = "failed"
	}

	return float32(mos), outcome
}
