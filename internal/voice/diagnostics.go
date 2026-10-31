package voice

import (
	"fmt"
	"math"
	"sync/atomic"
	"time"
)

const diagnosticSilenceFloor = 120

// runtimeDiagnostics observes the existing live pipeline. It never reads IPC,
// retains PCM, or records conversation content.
type runtimeDiagnostics struct {
	pcmChunks          atomic.Uint64
	pcmBytes           atomic.Uint64
	pcmSamples         atomic.Uint64
	detectorFrames     atomic.Uint64
	streamingFrames    atomic.Uint64
	spotterInitErrors  atomic.Uint64
	spotterFeedErrors  atomic.Uint64
	wakeDetections     atomic.Uint64
	captureStalls      atomic.Uint64
	captureReadErrors  atomic.Uint64
	captureQueueDrops  atomic.Uint64
	lastPCMUnixNano    atomic.Int64
	lastVoiceUnixNano  atomic.Int64
	lastRMSBits        atomic.Uint64
	lastPeak           atomic.Uint32
}

func (d *runtimeDiagnostics) observePCM(pcm []byte) {
	if len(pcm) < 2 || len(pcm)%2 != 0 {
		return
	}
	var sumSquares uint64
	var peak uint32
	samples := len(pcm) / 2
	for i := 0; i < len(pcm); i += 2 {
		value := int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8)
		magnitude := uint32(value)
		if value < 0 {
			magnitude = uint32(-int32(value))
		}
		if magnitude > peak {
			peak = magnitude
		}
		sumSquares += uint64(int64(value) * int64(value))
	}
	now := time.Now()
	rms := math.Sqrt(float64(sumSquares) / float64(samples))
	d.pcmChunks.Add(1)
	d.pcmBytes.Add(uint64(len(pcm)))
	d.pcmSamples.Add(uint64(samples))
	d.lastPCMUnixNano.Store(now.UnixNano())
	d.lastRMSBits.Store(math.Float64bits(rms))
	d.lastPeak.Store(peak)
	if rms >= diagnosticSilenceFloor {
		d.lastVoiceUnixNano.Store(now.UnixNano())
	}
}

func (d *runtimeDiagnostics) snapshot() string {
	now := time.Now()
	lastPCM := d.lastPCMUnixNano.Load()
	lastVoice := d.lastVoiceUnixNano.Load()
	rms := math.Float64frombits(d.lastRMSBits.Load())
	pcmAge := "not observed"
	if lastPCM != 0 {
		pcmAge = now.Sub(time.Unix(0, lastPCM)).Round(time.Millisecond).String()
	}
	silence := "no non-silent PCM observed"
	if lastVoice != 0 {
		silence = now.Sub(time.Unix(0, lastVoice)).Round(time.Millisecond).String()
	}
	return fmt.Sprintf(
		"Runtime audio: chunks=%d bytes=%d samples=%d detector_frames=%d streaming_frames=%d last_pcm_age=%s rms=%.0f peak=%d silence=%s spotter_init_errors=%d spotter_feed_errors=%d wake_detections=%d capture_stalls=%d capture_read_errors=%d queue_overflows=%d",
		d.pcmChunks.Load(), d.pcmBytes.Load(), d.pcmSamples.Load(),
		d.detectorFrames.Load(), d.streamingFrames.Load(), pcmAge, rms,
		d.lastPeak.Load(), silence, d.spotterInitErrors.Load(),
		d.spotterFeedErrors.Load(), d.wakeDetections.Load(),
		d.captureStalls.Load(), d.captureReadErrors.Load(),
		d.captureQueueDrops.Load(),
	)
}
