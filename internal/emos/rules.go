package emos

import (
	"fmt"
)

// DefaultRules returns the 9 built-in observation rules.
func DefaultRules() []ObservationRule {
	return []ObservationRule{
		{Name: "packet_loss_sustained", Severity: "warning", Evaluate: evaluatePacketLossSustained},
		{Name: "packet_loss_spike", Severity: "critical", Evaluate: evaluatePacketLossSpike},
		{Name: "rtt_high", Severity: "warning", Evaluate: evaluateRTTHigh},
		{Name: "rtt_spike", Severity: "warning", Evaluate: evaluateRTTSpike},
		{Name: "jitter_high", Severity: "warning", Evaluate: evaluateJitterHigh},
		{Name: "video_freeze_repeated", Severity: "warning", Evaluate: evaluateVideoFreezeRepeated},
		{Name: "audio_concealment_high", Severity: "warning", Evaluate: evaluateAudioConcealmentHigh},
		{Name: "bitrate_collapse", Severity: "critical", Evaluate: evaluateBitrateCollapse},
		{Name: "ice_failure", Severity: "critical", Evaluate: evaluateICEFailureRule},
	}
}

// evaluatePacketLossSustained detects sustained packet loss (3+ consecutive samples).
func evaluatePacketLossSustained(samples []StatRow, thresholds map[string]interface{}) []Observation {
	threshold := thresholds["packet_loss_sustained.threshold"].(float64)
	minSamples := int(thresholds["packet_loss_sustained.min_samples"].(float64))

	var observations []Observation
	var consecutive struct {
		start int
		count int
	}

	for i, sample := range samples {
		if sample.PacketLossRatio > threshold {
			if consecutive.count == 0 {
				consecutive.start = i
			}
			consecutive.count++
		} else {
			if consecutive.count >= minSamples {
				avgLoss := averagePacketLoss(samples[consecutive.start:i])
				observations = append(observations, Observation{
					RuleName: "packet_loss_sustained",
					Severity: "warning",
					Message: fmt.Sprintf("Packet loss sustained at %.1f%% for %d samples",
						avgLoss*100, consecutive.count),
					StartTS: samples[consecutive.start].Timestamp,
					EndTS:   samples[i-1].Timestamp,
					Context: map[string]interface{}{
						"average_loss": avgLoss,
						"sample_count": consecutive.count,
					},
				})
			}
			consecutive.count = 0
		}
	}

	// Handle case where threshold is met through end of samples.
	if consecutive.count >= minSamples {
		avgLoss := averagePacketLoss(samples[consecutive.start:])
		observations = append(observations, Observation{
			RuleName: "packet_loss_sustained",
			Severity: "warning",
			Message: fmt.Sprintf("Packet loss sustained at %.1f%% for %d samples",
				avgLoss*100, consecutive.count),
			StartTS: samples[consecutive.start].Timestamp,
			EndTS:   samples[len(samples)-1].Timestamp,
			Context: map[string]interface{}{
				"average_loss": avgLoss,
				"sample_count": consecutive.count,
			},
		})
	}

	return observations
}

// evaluatePacketLossSpike detects single sample of high packet loss.
func evaluatePacketLossSpike(samples []StatRow, thresholds map[string]interface{}) []Observation {
	threshold := thresholds["packet_loss_spike.threshold"].(float64)

	var observations []Observation
	for _, sample := range samples {
		if sample.PacketLossRatio > threshold {
			observations = append(observations, Observation{
				RuleName: "packet_loss_spike",
				Severity: "critical",
				Message:  fmt.Sprintf("Packet loss spike detected: %.1f%%", sample.PacketLossRatio*100),
				StartTS:  sample.Timestamp,
				EndTS:    sample.Timestamp,
				Context: map[string]interface{}{
					"loss_ratio": sample.PacketLossRatio,
				},
			})
		}
	}

	return observations
}

// evaluateRTTHigh detects sustained high RTT (2+ consecutive samples).
func evaluateRTTHigh(samples []StatRow, thresholds map[string]interface{}) []Observation {
	threshold := int32(thresholds["rtt_high.threshold"].(float64))
	minSamples := int(thresholds["rtt_high.min_samples"].(float64))

	var observations []Observation
	var consecutive struct {
		start int
		count int
	}

	for i, sample := range samples {
		if sample.RTT > threshold {
			if consecutive.count == 0 {
				consecutive.start = i
			}
			consecutive.count++
		} else {
			if consecutive.count >= minSamples {
				avgRTT := averageRTT(samples[consecutive.start:i])
				observations = append(observations, Observation{
					RuleName: "rtt_high",
					Severity: "warning",
					Message:  fmt.Sprintf("RTT high at %d ms for %d samples", avgRTT, consecutive.count),
					StartTS:  samples[consecutive.start].Timestamp,
					EndTS:    samples[i-1].Timestamp,
					Context: map[string]interface{}{
						"average_rtt":  avgRTT,
						"sample_count": consecutive.count,
					},
				})
			}
			consecutive.count = 0
		}
	}

	// Handle case where threshold is met through end of samples.
	if consecutive.count >= minSamples {
		avgRTT := averageRTT(samples[consecutive.start:])
		observations = append(observations, Observation{
			RuleName: "rtt_high",
			Severity: "warning",
			Message:  fmt.Sprintf("RTT high at %d ms for %d samples", avgRTT, consecutive.count),
			StartTS:  samples[consecutive.start].Timestamp,
			EndTS:    samples[len(samples)-1].Timestamp,
			Context: map[string]interface{}{
				"average_rtt":  avgRTT,
				"sample_count": consecutive.count,
			},
		})
	}

	return observations
}

// evaluateRTTSpike detects large RTT increase between consecutive samples.
func evaluateRTTSpike(samples []StatRow, thresholds map[string]interface{}) []Observation {
	delta := int32(thresholds["rtt_spike_delta.threshold"].(float64))

	var observations []Observation
	for i := 1; i < len(samples); i++ {
		increase := samples[i].RTT - samples[i-1].RTT
		if increase > delta {
			observations = append(observations, Observation{
				RuleName: "rtt_spike",
				Severity: "warning",
				Message: fmt.Sprintf("RTT spike: %d ms -> %d ms (delta: %d ms)",
					samples[i-1].RTT, samples[i].RTT, increase),
				StartTS: samples[i-1].Timestamp,
				EndTS:   samples[i].Timestamp,
				Context: map[string]interface{}{
					"previous_rtt": samples[i-1].RTT,
					"current_rtt":  samples[i].RTT,
					"delta":        increase,
				},
			})
		}
	}

	return observations
}

// evaluateJitterHigh detects sustained high jitter (3+ consecutive samples).
func evaluateJitterHigh(samples []StatRow, thresholds map[string]interface{}) []Observation {
	threshold := int32(thresholds["jitter_high.threshold"].(float64))
	minSamples := int(thresholds["jitter_high.min_samples"].(float64))

	var observations []Observation
	var consecutive struct {
		start int
		count int
	}

	for i, sample := range samples {
		if sample.Jitter > threshold {
			if consecutive.count == 0 {
				consecutive.start = i
			}
			consecutive.count++
		} else {
			if consecutive.count >= minSamples {
				avgJitter := averageJitter(samples[consecutive.start:i])
				observations = append(observations, Observation{
					RuleName: "jitter_high",
					Severity: "warning",
					Message:  fmt.Sprintf("Jitter high at %d ms for %d samples", avgJitter, consecutive.count),
					StartTS:  samples[consecutive.start].Timestamp,
					EndTS:    samples[i-1].Timestamp,
					Context: map[string]interface{}{
						"average_jitter": avgJitter,
						"sample_count":   consecutive.count,
					},
				})
			}
			consecutive.count = 0
		}
	}

	// Handle case where threshold is met through end of samples.
	if consecutive.count >= minSamples {
		avgJitter := averageJitter(samples[consecutive.start:])
		observations = append(observations, Observation{
			RuleName: "jitter_high",
			Severity: "warning",
			Message:  fmt.Sprintf("Jitter high at %d ms for %d samples", avgJitter, consecutive.count),
			StartTS:  samples[consecutive.start].Timestamp,
			EndTS:    samples[len(samples)-1].Timestamp,
			Context: map[string]interface{}{
				"average_jitter": avgJitter,
				"sample_count":   consecutive.count,
			},
		})
	}

	return observations
}

// evaluateVideoFreezeRepeated detects repeated video freezes.
func evaluateVideoFreezeRepeated(samples []StatRow, thresholds map[string]interface{}) []Observation {
	var observations []Observation
	var consecutive struct {
		start int
		count int
	}

	for i, sample := range samples {
		if i > 0 && sample.VideoFreezeCount > samples[i-1].VideoFreezeCount {
			if consecutive.count == 0 {
				consecutive.start = i - 1
			}
			consecutive.count++
		} else if consecutive.count > 0 {
			if consecutive.count >= 2 {
				freezeCount := samples[consecutive.start+consecutive.count].VideoFreezeCount - samples[consecutive.start].VideoFreezeCount
				observations = append(observations, Observation{
					RuleName: "video_freeze_repeated",
					Severity: "warning",
					Message:  fmt.Sprintf("Video freeze repeated: %d freeze events", freezeCount),
					StartTS:  samples[consecutive.start].Timestamp,
					EndTS:    samples[consecutive.start+consecutive.count].Timestamp,
					Context: map[string]interface{}{
						"freeze_count": freezeCount,
					},
				})
			}
			consecutive.count = 0
		}
	}

	if consecutive.count >= 2 {
		freezeCount := samples[len(samples)-1].VideoFreezeCount - samples[consecutive.start].VideoFreezeCount
		observations = append(observations, Observation{
			RuleName: "video_freeze_repeated",
			Severity: "warning",
			Message:  fmt.Sprintf("Video freeze repeated: %d freeze events", freezeCount),
			StartTS:  samples[consecutive.start].Timestamp,
			EndTS:    samples[len(samples)-1].Timestamp,
			Context: map[string]interface{}{
				"freeze_count": freezeCount,
			},
		})
	}

	return observations
}

// evaluateAudioConcealmentHigh detects sustained high audio concealment.
func evaluateAudioConcealmentHigh(samples []StatRow, thresholds map[string]interface{}) []Observation {
	threshold := thresholds["audio_concealment_high.threshold"].(float64)
	minSamples := int(thresholds["audio_concealment_high.min_samples"].(float64))

	var observations []Observation
	var consecutive struct {
		start int
		count int
	}

	for i, sample := range samples {
		if sample.AudioConcealmentRatio > threshold {
			if consecutive.count == 0 {
				consecutive.start = i
			}
			consecutive.count++
		} else {
			if consecutive.count >= minSamples {
				avgConcealment := averageAudioConcealment(samples[consecutive.start:i])
				observations = append(observations, Observation{
					RuleName: "audio_concealment_high",
					Severity: "warning",
					Message: fmt.Sprintf("Audio concealment high at %.1f%% for %d samples",
						avgConcealment*100, consecutive.count),
					StartTS: samples[consecutive.start].Timestamp,
					EndTS:   samples[i-1].Timestamp,
					Context: map[string]interface{}{
						"average_concealment": avgConcealment,
						"sample_count":        consecutive.count,
					},
				})
			}
			consecutive.count = 0
		}
	}

	// Handle case where threshold is met through end of samples.
	if consecutive.count >= minSamples {
		avgConcealment := averageAudioConcealment(samples[consecutive.start:])
		observations = append(observations, Observation{
			RuleName: "audio_concealment_high",
			Severity: "warning",
			Message: fmt.Sprintf("Audio concealment high at %.1f%% for %d samples",
				avgConcealment*100, consecutive.count),
			StartTS: samples[consecutive.start].Timestamp,
			EndTS:   samples[len(samples)-1].Timestamp,
			Context: map[string]interface{}{
				"average_concealment": avgConcealment,
				"sample_count":        consecutive.count,
			},
		})
	}

	return observations
}

// evaluateBitrateCollapse detects large outbound bitrate drops.
func evaluateBitrateCollapse(samples []StatRow, thresholds map[string]interface{}) []Observation {
	ratio := thresholds["bitrate_collapse_ratio.threshold"].(float64)

	var observations []Observation
	for i := 1; i < len(samples); i++ {
		if samples[i-1].OutboundBitrate > 0 {
			drop := 1.0 - float64(samples[i].OutboundBitrate)/float64(samples[i-1].OutboundBitrate)
			if drop > ratio {
				percentDrop := drop * 100
				observations = append(observations, Observation{
					RuleName: "bitrate_collapse",
					Severity: "critical",
					Message: fmt.Sprintf("Bitrate collapse: %.1f%% drop (%d bps -> %d bps)",
						percentDrop, samples[i-1].OutboundBitrate, samples[i].OutboundBitrate),
					StartTS: samples[i-1].Timestamp,
					EndTS:   samples[i].Timestamp,
					Context: map[string]interface{}{
						"previous_bitrate": samples[i-1].OutboundBitrate,
						"current_bitrate":  samples[i].OutboundBitrate,
						"drop_percent":     percentDrop,
					},
				})
			}
		}
	}

	return observations
}

// evaluateICEFailureRule is a placeholder (actual ICE failures handled separately).
func evaluateICEFailureRule(samples []StatRow, thresholds map[string]interface{}) []Observation {
	return nil // ICE failures are evaluated separately from connection_stats.
}

// Helper functions.

func averagePacketLoss(samples []StatRow) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += s.PacketLossRatio
	}
	return sum / float64(len(samples))
}

func averageRTT(samples []StatRow) int32 {
	if len(samples) == 0 {
		return 0
	}
	var sum int32
	for _, s := range samples {
		sum += s.RTT
	}
	return sum / int32(len(samples))
}

func averageJitter(samples []StatRow) int32 {
	if len(samples) == 0 {
		return 0
	}
	var sum int32
	for _, s := range samples {
		sum += s.Jitter
	}
	return sum / int32(len(samples))
}

func averageAudioConcealment(samples []StatRow) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += s.AudioConcealmentRatio
	}
	return sum / float64(len(samples))
}
