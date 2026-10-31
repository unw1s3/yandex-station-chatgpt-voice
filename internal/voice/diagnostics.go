package voice

import (
	"fmt"
	"math"
	"sync/atomic"
	"sync"
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
	outboundOpusPackets atomic.Uint64
	outboundOpusBytes atomic.Uint64
	inboundRTPPackets atomic.Uint64
	inboundRTPBytes atomic.Uint64
	decodedSamples atomic.Uint64
	playbackQueuedBlocks atomic.Uint64
	playbackQueueDrops atomic.Uint64
	setupDurationMs atomic.Int64
	playbackOutputFrames atomic.Uint64
	inboundPCMBlocks atomic.Uint64
	inboundPCMSilentBlocks atomic.Uint64
	lastInboundPCMRMSBits atomic.Uint64
	lastInboundPCMPeak atomic.Uint32
	playbackQueueDepth atomic.Uint32
	playbackQueueMaxDepth atomic.Uint32
	playbackSilentCallbacks atomic.Uint64
	lastPlaybackRMSBits atomic.Uint64
	lastPlaybackPeak atomic.Uint32
	playbackNonzeroFrames atomic.Uint64
	signalingAttempts atomic.Uint64
	signalingHTTPStatus atomic.Int32
	signalingDurationMs atomic.Int64
	signalingSuccess atomic.Bool
	firstPCMUnixNano   atomic.Int64
	lastPCMUnixNano    atomic.Int64
	lastVoiceUnixNano  atomic.Int64
	lastRMSBits        atomic.Uint64
	lastPeak           atomic.Uint32
	maxCaptureRMSBits atomic.Uint64
	maxCapturePeak    atomic.Uint32
	maxCaptureBERMSBits atomic.Uint64
	maxCaptureBEPeak atomic.Uint32
	outboundPCMFrames atomic.Uint64
	outboundVoiceFrames atomic.Uint64
	maxOutboundRMSBits atomic.Uint64
	maxOutboundPeak atomic.Uint32

	realtimeEvents      atomic.Uint64
	responseCreated     atomic.Uint64
	responseDone        atomic.Uint64
	responseAudioDeltas atomic.Uint64
	transcriptionDone   atomic.Uint64
	serverErrors        atomic.Uint64

	lastEventMu        sync.Mutex
	lastEvent          string
	lastResponseStatus string
	lastServerFailure  string
}

func (d *runtimeDiagnostics) observeRealtimeEvent(ev event) {
	if ev.kind == "" { return }
	d.realtimeEvents.Add(1)
	switch ev.kind {
	case "response.created": d.responseCreated.Add(1)
	case "response.done": d.responseDone.Add(1)
	case "response.audio.delta": d.responseAudioDeltas.Add(1)
	case "conversation.item.input_audio_transcription.completed": d.transcriptionDone.Add(1)
	case "error": d.serverErrors.Add(1)
	}
	d.lastEventMu.Lock()
	d.lastEvent = ev.kind
	if ev.status != "" { d.lastResponseStatus = ev.status }
	if ev.failure != "" { d.lastServerFailure = ev.failure }
	d.lastEventMu.Unlock()
}

func storeMaxRMS(dst *atomic.Uint64, value float64) {
	bits := math.Float64bits(value)
	for old := dst.Load(); value > math.Float64frombits(old); old = dst.Load() {
		if dst.CompareAndSwap(old, bits) {
			return
		}
	}
}

func storeMaxPeak(dst *atomic.Uint32, value uint32) {
	for old := dst.Load(); value > old; old = dst.Load() {
		if dst.CompareAndSwap(old, value) {
			return
		}
	}
}

func (d *runtimeDiagnostics) observePCM(pcm []byte) {
	if len(pcm) < 2 || len(pcm)%2 != 0 {
		return
	}
	var sumSquares uint64
	var peak uint32
	var beSumSquares uint64
	var bePeak uint32
	samples := len(pcm) / 2
	for i := 0; i < len(pcm); i += 2 {
		value := int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8)
		beValue := int16(uint16(pcm[i+1]) | uint16(pcm[i])<<8)
		beMagnitude := uint32(beValue)
		if beValue < 0 { beMagnitude = uint32(-int32(beValue)) }
		if beMagnitude > bePeak { bePeak = beMagnitude }
		beSumSquares += uint64(int64(beValue) * int64(beValue))
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
	d.firstPCMUnixNano.CompareAndSwap(0, now.UnixNano())
	d.lastPCMUnixNano.Store(now.UnixNano())
	d.lastRMSBits.Store(math.Float64bits(rms))
	d.lastPeak.Store(peak)
	storeMaxRMS(&d.maxCaptureRMSBits, rms)
	storeMaxPeak(&d.maxCapturePeak, peak)
	storeMaxRMS(&d.maxCaptureBERMSBits, math.Sqrt(float64(beSumSquares)/float64(samples)))
	storeMaxPeak(&d.maxCaptureBEPeak, bePeak)
	if rms >= diagnosticSilenceFloor {
		d.lastVoiceUnixNano.Store(now.UnixNano())
	}
}

func (d *runtimeDiagnostics) observeOutboundPCM(pcm []int16) {
	if len(pcm) == 0 {
		return
	}
	var sumSquares uint64
	var peak uint32
	for _, sample := range pcm {
		value := int32(sample)
		magnitude := uint32(value)
		if value < 0 {
			magnitude = uint32(-value)
		}
		if magnitude > peak {
			peak = magnitude
		}
		sumSquares += uint64(value * value)
	}
	rms := math.Sqrt(float64(sumSquares) / float64(len(pcm)))
	d.outboundPCMFrames.Add(1)
	if rms >= diagnosticSilenceFloor {
		d.outboundVoiceFrames.Add(1)
	}
	storeMaxRMS(&d.maxOutboundRMSBits, rms)
	storeMaxPeak(&d.maxOutboundPeak, peak)
}

func (d *runtimeDiagnostics) observeInboundPCM(pcm []int16) {
	if len(pcm) == 0 {
		return
	}
	var sumSquares uint64
	var peak uint32
	for _, sample := range pcm {
		value := int32(sample)
		magnitude := uint32(value)
		if value < 0 {
			magnitude = uint32(-value)
		}
		if magnitude > peak {
			peak = magnitude
		}
		sumSquares += uint64(value * value)
	}
	rms := math.Sqrt(float64(sumSquares) / float64(len(pcm)))
	d.inboundPCMBlocks.Add(1)
	if rms < diagnosticSilenceFloor {
		d.inboundPCMSilentBlocks.Add(1)
	}
	d.lastInboundPCMRMSBits.Store(math.Float64bits(rms))
	d.lastInboundPCMPeak.Store(peak)
}

func (d *runtimeDiagnostics) setPlaybackQueueDepth(depth int) {
	if depth < 0 {
		depth = 0
	}
	value := uint32(depth)
	d.playbackQueueDepth.Store(value)
	for max := d.playbackQueueMaxDepth.Load(); value > max; max = d.playbackQueueMaxDepth.Load() {
		if d.playbackQueueMaxDepth.CompareAndSwap(max, value) {
			break
		}
	}
}

func (d *runtimeDiagnostics) observePlaybackOutput(output []byte, nonzeroFrames uint64) {
	if len(output) < 2 || len(output)%2 != 0 {
		return
	}
	var sumSquares uint64
	var peak uint32
	samples := len(output) / 2
	for i := 0; i < len(output); i += 2 {
		value := int16(uint16(output[i]) | uint16(output[i+1])<<8)
		magnitude := uint32(value)
		if value < 0 {
			magnitude = uint32(-int32(value))
		}
		if magnitude > peak {
			peak = magnitude
		}
		sumSquares += uint64(int64(value) * int64(value))
	}
	rms := math.Sqrt(float64(sumSquares) / float64(samples))
	d.lastPlaybackRMSBits.Store(math.Float64bits(rms))
	d.lastPlaybackPeak.Store(peak)
	d.playbackNonzeroFrames.Add(nonzeroFrames)
	if peak < diagnosticSilenceFloor {
		d.playbackSilentCallbacks.Add(1)
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
	silence := "not observed"
	silenceSince := lastVoice
	if silenceSince == 0 { silenceSince = d.firstPCMUnixNano.Load() }
	if silenceSince != 0 { silence = now.Sub(time.Unix(0, silenceSince)).Round(time.Millisecond).String() }
	d.lastEventMu.Lock()
	lastEvent, responseStatus, serverFailure := d.lastEvent, d.lastResponseStatus, d.lastServerFailure
	d.lastEventMu.Unlock()
	return fmt.Sprintf(
		"Runtime audio: chunks=%d bytes=%d samples=%d detector_frames=%d streaming_frames=%d last_pcm_age=%s rms=%.0f peak=%d capture_max_rms=%.0f capture_max_peak=%d capture_be_max_rms=%.0f capture_be_max_peak=%d opus_input_frames=%d opus_input_voice_frames=%d opus_input_max_rms=%.0f opus_input_max_peak=%d silence=%s silence_floor=%d spotter_init_errors=%d spotter_feed_errors=%d wake_detections=%d capture_stalls=%d capture_read_errors=%d queue_overflows=%d opus_out_packets=%d opus_out_bytes=%d rtp_in_packets=%d rtp_in_bytes=%d decoded_samples=%d inbound_pcm_blocks=%d inbound_pcm_silent_blocks=%d inbound_pcm_rms=%.0f inbound_pcm_peak=%d playback_blocks=%d playback_queue_depth=%d playback_queue_max=%d playback_queue_drops=%d playback_output_frames=%d playback_output_rms=%.0f playback_output_peak=%d playback_silent_callbacks=%d playback_nonzero_frames=%d setup_ms=%d signaling_attempts=%d signaling_http_status=%d signaling_ms=%d signaling_ok=%t realtime_events=%d response_created=%d response_done=%d response_audio_deltas=%d transcription_done=%d server_errors=%d last_event=%s response_status=%s server_failure=%s",
		d.pcmChunks.Load(), d.pcmBytes.Load(), d.pcmSamples.Load(),
		d.detectorFrames.Load(), d.streamingFrames.Load(), pcmAge, rms,
		d.lastPeak.Load(), math.Float64frombits(d.maxCaptureRMSBits.Load()), d.maxCapturePeak.Load(),
		math.Float64frombits(d.maxCaptureBERMSBits.Load()), d.maxCaptureBEPeak.Load(),
		d.outboundPCMFrames.Load(), d.outboundVoiceFrames.Load(),
		math.Float64frombits(d.maxOutboundRMSBits.Load()), d.maxOutboundPeak.Load(),
		silence, diagnosticSilenceFloor, d.spotterInitErrors.Load(),
		d.spotterFeedErrors.Load(), d.wakeDetections.Load(),
		d.captureStalls.Load(), d.captureReadErrors.Load(),
		d.captureQueueDrops.Load(), d.outboundOpusPackets.Load(),
		d.outboundOpusBytes.Load(), d.inboundRTPPackets.Load(),
		d.inboundRTPBytes.Load(), d.decodedSamples.Load(),
		d.inboundPCMBlocks.Load(), d.inboundPCMSilentBlocks.Load(),
		math.Float64frombits(d.lastInboundPCMRMSBits.Load()), d.lastInboundPCMPeak.Load(),
		d.playbackQueuedBlocks.Load(), d.playbackQueueDepth.Load(), d.playbackQueueMaxDepth.Load(), d.playbackQueueDrops.Load(),
		d.playbackOutputFrames.Load(), math.Float64frombits(d.lastPlaybackRMSBits.Load()),
		d.lastPlaybackPeak.Load(), d.playbackSilentCallbacks.Load(), d.playbackNonzeroFrames.Load(),
		d.setupDurationMs.Load(),
		d.signalingAttempts.Load(), d.signalingHTTPStatus.Load(),
		d.signalingDurationMs.Load(), d.signalingSuccess.Load(),
		d.realtimeEvents.Load(), d.responseCreated.Load(), d.responseDone.Load(),
		d.responseAudioDeltas.Load(), d.transcriptionDone.Load(), d.serverErrors.Load(),
		lastEvent, responseStatus, serverFailure,
	)
}
