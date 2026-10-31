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
	signalingAttempts atomic.Uint64
	signalingHTTPStatus atomic.Int32
	signalingDurationMs atomic.Int64
	signalingSuccess atomic.Bool
	firstPCMUnixNano   atomic.Int64
	lastPCMUnixNano    atomic.Int64
	lastVoiceUnixNano  atomic.Int64
	lastRMSBits        atomic.Uint64
	lastPeak           atomic.Uint32

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
	d.firstPCMUnixNano.CompareAndSwap(0, now.UnixNano())
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
	silence := "not observed"
	silenceSince := lastVoice
	if silenceSince == 0 { silenceSince = d.firstPCMUnixNano.Load() }
	if silenceSince != 0 { silence = now.Sub(time.Unix(0, silenceSince)).Round(time.Millisecond).String() }
	d.lastEventMu.Lock()
	lastEvent, responseStatus, serverFailure := d.lastEvent, d.lastResponseStatus, d.lastServerFailure
	d.lastEventMu.Unlock()
	return fmt.Sprintf(
		"Runtime audio: chunks=%d bytes=%d samples=%d detector_frames=%d streaming_frames=%d last_pcm_age=%s rms=%.0f peak=%d silence=%s silence_floor=%d spotter_init_errors=%d spotter_feed_errors=%d wake_detections=%d capture_stalls=%d capture_read_errors=%d queue_overflows=%d opus_out_packets=%d opus_out_bytes=%d rtp_in_packets=%d rtp_in_bytes=%d decoded_samples=%d playback_blocks=%d playback_queue_drops=%d playback_output_frames=%d setup_ms=%d signaling_attempts=%d signaling_http_status=%d signaling_ms=%d signaling_ok=%t realtime_events=%d response_created=%d response_done=%d response_audio_deltas=%d transcription_done=%d server_errors=%d last_event=%s response_status=%s server_failure=%s",
		d.pcmChunks.Load(), d.pcmBytes.Load(), d.pcmSamples.Load(),
		d.detectorFrames.Load(), d.streamingFrames.Load(), pcmAge, rms,
		d.lastPeak.Load(), silence, diagnosticSilenceFloor, d.spotterInitErrors.Load(),
		d.spotterFeedErrors.Load(), d.wakeDetections.Load(),
		d.captureStalls.Load(), d.captureReadErrors.Load(),
		d.captureQueueDrops.Load(), d.outboundOpusPackets.Load(),
		d.outboundOpusBytes.Load(), d.inboundRTPPackets.Load(),
		d.inboundRTPBytes.Load(), d.decodedSamples.Load(),
		d.playbackQueuedBlocks.Load(), d.playbackQueueDrops.Load(),
		d.playbackOutputFrames.Load(), d.setupDurationMs.Load(),
		d.signalingAttempts.Load(), d.signalingHTTPStatus.Load(),
		d.signalingDurationMs.Load(), d.signalingSuccess.Load(),
		d.realtimeEvents.Load(), d.responseCreated.Load(), d.responseDone.Load(),
		d.responseAudioDeltas.Load(), d.transcriptionDone.Load(), d.serverErrors.Load(),
		lastEvent, responseStatus, serverFailure,
	)
}
