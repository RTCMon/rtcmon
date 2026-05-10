package emos

import (
	"testing"
	"time"
)

func TestObservation_PacketLossSustained(t *testing.T) {
	samples := []StatRow{
		{Timestamp: time.Unix(0, 0), PacketLossRatio: 0.08},
		{Timestamp: time.Unix(10, 0), PacketLossRatio: 0.07},
		{Timestamp: time.Unix(20, 0), PacketLossRatio: 0.09},
		{Timestamp: time.Unix(30, 0), PacketLossRatio: 0.01},
	}
	thresholds := map[string]interface{}{
		"packet_loss_sustained.threshold":   0.05,
		"packet_loss_sustained.min_samples": 3.0,
	}

	obs := evaluatePacketLossSustained(samples, thresholds)
	if len(obs) != 1 {
		t.Errorf("expected 1 observation, got %d", len(obs))
	}
	if obs[0].RuleName != "packet_loss_sustained" {
		t.Errorf("expected rule 'packet_loss_sustained', got %s", obs[0].RuleName)
	}
	if obs[0].Severity != "warning" {
		t.Errorf("expected severity 'warning', got %s", obs[0].Severity)
	}
}

func TestObservation_PacketLossSpike(t *testing.T) {
	samples := []StatRow{
		{Timestamp: time.Unix(0, 0), PacketLossRatio: 0.02},
		{Timestamp: time.Unix(10, 0), PacketLossRatio: 0.20},
		{Timestamp: time.Unix(20, 0), PacketLossRatio: 0.01},
	}
	thresholds := map[string]interface{}{
		"packet_loss_spike.threshold": 0.15,
	}

	obs := evaluatePacketLossSpike(samples, thresholds)
	if len(obs) != 1 {
		t.Errorf("expected 1 observation, got %d", len(obs))
	}
	if obs[0].Severity != "critical" {
		t.Errorf("expected severity 'critical', got %s", obs[0].Severity)
	}
}

func TestObservation_RTTHigh(t *testing.T) {
	samples := []StatRow{
		{Timestamp: time.Unix(0, 0), RTT: 350},
		{Timestamp: time.Unix(10, 0), RTT: 380},
		{Timestamp: time.Unix(20, 0), RTT: 100},
	}
	thresholds := map[string]interface{}{
		"rtt_high.threshold":   300.0,
		"rtt_high.min_samples": 2.0,
	}

	obs := evaluateRTTHigh(samples, thresholds)
	if len(obs) != 1 {
		t.Errorf("expected 1 observation, got %d", len(obs))
	}
	if obs[0].RuleName != "rtt_high" {
		t.Errorf("expected rule 'rtt_high', got %s", obs[0].RuleName)
	}
}

func TestObservation_RTTSpike(t *testing.T) {
	samples := []StatRow{
		{Timestamp: time.Unix(0, 0), RTT: 50},
		{Timestamp: time.Unix(10, 0), RTT: 180},
		{Timestamp: time.Unix(20, 0), RTT: 190},
	}
	thresholds := map[string]interface{}{
		"rtt_spike_delta.threshold": 100.0,
	}

	obs := evaluateRTTSpike(samples, thresholds)
	if len(obs) != 1 {
		t.Errorf("expected 1 observation, got %d", len(obs))
	}
	if obs[0].RuleName != "rtt_spike" {
		t.Errorf("expected rule 'rtt_spike', got %s", obs[0].RuleName)
	}
}

func TestObservation_JitterHigh(t *testing.T) {
	samples := []StatRow{
		{Timestamp: time.Unix(0, 0), Jitter: 60},
		{Timestamp: time.Unix(10, 0), Jitter: 55},
		{Timestamp: time.Unix(20, 0), Jitter: 65},
		{Timestamp: time.Unix(30, 0), Jitter: 10},
	}
	thresholds := map[string]interface{}{
		"jitter_high.threshold":   50.0,
		"jitter_high.min_samples": 3.0,
	}

	obs := evaluateJitterHigh(samples, thresholds)
	if len(obs) != 1 {
		t.Errorf("expected 1 observation, got %d", len(obs))
	}
	if obs[0].RuleName != "jitter_high" {
		t.Errorf("expected rule 'jitter_high', got %s", obs[0].RuleName)
	}
}

func TestObservation_VideoFreezeRepeated(t *testing.T) {
	samples := []StatRow{
		{Timestamp: time.Unix(0, 0), VideoFreezeCount: 0},
		{Timestamp: time.Unix(10, 0), VideoFreezeCount: 2},
		{Timestamp: time.Unix(20, 0), VideoFreezeCount: 5},
		{Timestamp: time.Unix(30, 0), VideoFreezeCount: 5},
	}
	thresholds := map[string]interface{}{}

	obs := evaluateVideoFreezeRepeated(samples, thresholds)
	if len(obs) != 1 {
		t.Errorf("expected 1 observation, got %d", len(obs))
	}
	if obs[0].RuleName != "video_freeze_repeated" {
		t.Errorf("expected rule 'video_freeze_repeated', got %s", obs[0].RuleName)
	}
}

func TestObservation_AudioConcealmentHigh(t *testing.T) {
	samples := []StatRow{
		{Timestamp: time.Unix(0, 0), AudioConcealmentRatio: 0.15},
		{Timestamp: time.Unix(10, 0), AudioConcealmentRatio: 0.12},
		{Timestamp: time.Unix(20, 0), AudioConcealmentRatio: 0.18},
		{Timestamp: time.Unix(30, 0), AudioConcealmentRatio: 0.02},
	}
	thresholds := map[string]interface{}{
		"audio_concealment_high.threshold":   0.10,
		"audio_concealment_high.min_samples": 3.0,
	}

	obs := evaluateAudioConcealmentHigh(samples, thresholds)
	if len(obs) != 1 {
		t.Errorf("expected 1 observation, got %d", len(obs))
	}
	if obs[0].RuleName != "audio_concealment_high" {
		t.Errorf("expected rule 'audio_concealment_high', got %s", obs[0].RuleName)
	}
}

func TestObservation_BitrateCollapse(t *testing.T) {
	samples := []StatRow{
		{Timestamp: time.Unix(0, 0), OutboundBitrate: 2000000},
		{Timestamp: time.Unix(10, 0), OutboundBitrate: 800000},
		{Timestamp: time.Unix(20, 0), OutboundBitrate: 500000},
	}
	thresholds := map[string]interface{}{
		"bitrate_collapse_ratio.threshold": 0.50,
	}

	obs := evaluateBitrateCollapse(samples, thresholds)
	if len(obs) != 1 {
		t.Errorf("expected 1 observation, got %d", len(obs))
	}
	if obs[0].RuleName != "bitrate_collapse" {
		t.Errorf("expected rule 'bitrate_collapse', got %s", obs[0].RuleName)
	}
	if obs[0].Severity != "critical" {
		t.Errorf("expected severity 'critical', got %s", obs[0].Severity)
	}
}

func TestObservation_NoStats(t *testing.T) {
	samples := []StatRow{}
	thresholds := map[string]interface{}{
		"packet_loss_sustained.threshold":   0.05,
		"packet_loss_sustained.min_samples": 3.0,
	}

	obs := evaluatePacketLossSustained(samples, thresholds)
	if len(obs) != 0 {
		t.Errorf("expected 0 observations for empty samples, got %d", len(obs))
	}
}

func TestObservation_PerAppThresholdOverride(t *testing.T) {
	samples := []StatRow{
		{Timestamp: time.Unix(0, 0), PacketLossRatio: 0.04},
		{Timestamp: time.Unix(10, 0), PacketLossRatio: 0.04},
		{Timestamp: time.Unix(20, 0), PacketLossRatio: 0.04},
	}
	// Default threshold is 0.05, so this should NOT trigger.
	defaultThresholds := map[string]interface{}{
		"packet_loss_sustained.threshold":   0.05,
		"packet_loss_sustained.min_samples": 3.0,
	}
	obs := evaluatePacketLossSustained(samples, defaultThresholds)
	if len(obs) != 0 {
		t.Errorf("expected 0 observations with default threshold 0.05, got %d", len(obs))
	}

	// App threshold is 0.03, so this SHOULD trigger.
	appThresholds := map[string]interface{}{
		"packet_loss_sustained.threshold":   0.03,
		"packet_loss_sustained.min_samples": 3.0,
	}
	obs = evaluatePacketLossSustained(samples, appThresholds)
	if len(obs) != 1 {
		t.Errorf("expected 1 observation with app threshold 0.03, got %d", len(obs))
	}
}

func TestObservation_AllRulesRegistered(t *testing.T) {
	rules := DefaultRules()
	if len(rules) != 9 {
		t.Errorf("expected 9 rules, got %d", len(rules))
	}

	expectedRules := map[string]bool{
		"packet_loss_sustained":  false,
		"packet_loss_spike":      false,
		"rtt_high":               false,
		"rtt_spike":              false,
		"jitter_high":            false,
		"video_freeze_repeated":  false,
		"audio_concealment_high": false,
		"bitrate_collapse":       false,
		"ice_failure":            false,
	}

	for _, rule := range rules {
		if _, found := expectedRules[rule.Name]; !found {
			t.Errorf("unexpected rule: %s", rule.Name)
		}
		expectedRules[rule.Name] = true
	}

	for ruleName, found := range expectedRules {
		if !found {
			t.Errorf("missing rule: %s", ruleName)
		}
	}
}

func TestObservation_SingleSampleSkipped(t *testing.T) {
	samples := []StatRow{
		{Timestamp: time.Unix(0, 0), PacketLossRatio: 0.08},
	}
	thresholds := map[string]interface{}{
		"packet_loss_sustained.threshold":   0.05,
		"packet_loss_sustained.min_samples": 3.0,
	}

	obs := evaluatePacketLossSustained(samples, thresholds)
	if len(obs) != 0 {
		t.Errorf("expected 0 observations for single sample, got %d", len(obs))
	}
}

func TestObservation_MultipleSpikes(t *testing.T) {
	samples := []StatRow{
		{Timestamp: time.Unix(0, 0), PacketLossRatio: 0.02},
		{Timestamp: time.Unix(10, 0), PacketLossRatio: 0.20},
		{Timestamp: time.Unix(20, 0), PacketLossRatio: 0.01},
		{Timestamp: time.Unix(30, 0), PacketLossRatio: 0.18},
		{Timestamp: time.Unix(40, 0), PacketLossRatio: 0.01},
	}
	thresholds := map[string]interface{}{
		"packet_loss_spike.threshold": 0.15,
	}

	obs := evaluatePacketLossSpike(samples, thresholds)
	if len(obs) != 2 {
		t.Errorf("expected 2 spike observations, got %d", len(obs))
	}
}

func TestObservation_HelperAveragePacketLoss(t *testing.T) {
	samples := []StatRow{
		{PacketLossRatio: 0.10},
		{PacketLossRatio: 0.20},
		{PacketLossRatio: 0.30},
	}
	avg := averagePacketLoss(samples)
	expected := 0.20
	// Use approximate comparison for floats
	if avg < expected-0.001 || avg > expected+0.001 {
		t.Errorf("expected average packet loss %.2f, got %.2f", expected, avg)
	}
}

func TestObservation_HelperAverageRTT(t *testing.T) {
	samples := []StatRow{
		{RTT: 100},
		{RTT: 200},
		{RTT: 300},
	}
	avg := averageRTT(samples)
	expected := int32(200)
	if avg != expected {
		t.Errorf("expected average RTT %d, got %d", expected, avg)
	}
}

func TestObservation_HelperAverageJitter(t *testing.T) {
	samples := []StatRow{
		{Jitter: 10},
		{Jitter: 30},
		{Jitter: 50},
	}
	avg := averageJitter(samples)
	expected := int32(30)
	if avg != expected {
		t.Errorf("expected average jitter %d, got %d", expected, avg)
	}
}

func TestObservation_HelperAverageAudioConcealment(t *testing.T) {
	samples := []StatRow{
		{AudioConcealmentRatio: 0.05},
		{AudioConcealmentRatio: 0.15},
		{AudioConcealmentRatio: 0.25},
	}
	avg := averageAudioConcealment(samples)
	expected := 0.15
	// Use approximate comparison for floats
	if avg < expected-0.001 || avg > expected+0.001 {
		t.Errorf("expected average audio concealment %.2f, got %.2f", expected, avg)
	}
}
