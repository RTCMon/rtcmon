package emos

import (
	"math"
	"testing"
)

func TestEMOS_ZeroLossLowRTT(t *testing.T) {
	emos, outcome := ComputeEMOS(0, 20, DefaultLossCoeff)
	if math.Abs(float64(emos)-4.4) > 0.1 {
		t.Errorf("expected eMOS ≈ 4.4, got %v", emos)
	}
	if outcome != "success" {
		t.Errorf("expected outcome 'success', got %q", outcome)
	}
}

func TestEMOS_HighLoss(t *testing.T) {
	// (10% loss, 50ms RTT) → eMOS < 3.0 per acceptance criteria.
	emos, _ := ComputeEMOS(0.10, 50, DefaultLossCoeff)
	if emos >= 3.0 {
		t.Errorf("expected eMOS < 3.0, got %v", emos)
	}
}

func TestEMOS_RFCSpec_HighLossHighRTT(t *testing.T) {
	// RFC §3.7: (10% loss, 100ms RTT) → eMOS ≈ 2.2 (±0.1)
	emos, _ := ComputeEMOS(0.10, 100, DefaultLossCoeff)
	if math.Abs(float64(emos)-2.2) > 0.1 {
		t.Errorf("RFC (10%%, 100ms): want ~2.2, got %v", emos)
	}
}

func TestEMOS_VeryHighLoss(t *testing.T) {
	// (30% loss, 300ms RTT): R goes negative → clamped to 0 → eMOS = 1.0.
	emos, outcome := ComputeEMOS(0.30, 300, DefaultLossCoeff)
	if emos != 1.0 {
		t.Errorf("expected eMOS = 1.0 (clamped), got %v", emos)
	}
	if outcome != "failed" {
		t.Errorf("expected outcome 'failed', got %q", outcome)
	}
}

func TestEMOS_ClampAbove5(t *testing.T) {
	// Best-case inputs: the formula has an upper ceiling of 5.0.
	emos, _ := ComputeEMOS(0, 0, DefaultLossCoeff)
	if emos > 5.0 {
		t.Errorf("eMOS must not exceed 5.0, got %v", emos)
	}
}

func TestEMOS_ClampBelow1(t *testing.T) {
	// 15% loss, 0ms RTT → R ≈ 6.2, unclamped MOS ≈ 0.998 → clamped to 1.0.
	emos, outcome := ComputeEMOS(0.15, 0, DefaultLossCoeff)
	if emos != 1.0 {
		t.Errorf("expected eMOS = 1.0 (clamped), got %v", emos)
	}
	if outcome != "failed" {
		t.Errorf("expected outcome 'failed', got %q", outcome)
	}
}

func TestEMOS_OutcomeSuccess(t *testing.T) {
	// (0% loss, 20ms RTT) → eMOS ≈ 4.4 ≥ 3.5.
	_, outcome := ComputeEMOS(0, 20, DefaultLossCoeff)
	if outcome != "success" {
		t.Errorf("expected 'success', got %q", outcome)
	}
}

func TestEMOS_OutcomeDegraded(t *testing.T) {
	// (10% loss, 50ms RTT) → eMOS ≈ 2.25, in [2.0, 3.5).
	_, outcome := ComputeEMOS(0.10, 50, DefaultLossCoeff)
	if outcome != "degraded" {
		t.Errorf("expected 'degraded', got %q", outcome)
	}
}

func TestEMOS_OutcomeFailed(t *testing.T) {
	// (15% loss, 0ms RTT) → eMOS clamped to 1.0 < 2.0.
	_, outcome := ComputeEMOS(0.15, 0, DefaultLossCoeff)
	if outcome != "failed" {
		t.Errorf("expected 'failed', got %q", outcome)
	}
}

func TestEMOS_CustomLossCoeff(t *testing.T) {
	// A lower coefficient reduces the loss penalty → higher eMOS for same inputs.
	hi, _ := ComputeEMOS(0.10, 50, DefaultLossCoeff) // 7.5
	lo, _ := ComputeEMOS(0.10, 50, 1.5)              // weaker penalty
	if lo <= hi {
		t.Errorf("lower lossCoeff should produce higher eMOS: got coeff=7.5→%v, coeff=1.5→%v", hi, lo)
	}
}
