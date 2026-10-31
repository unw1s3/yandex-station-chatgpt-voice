package voice

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"time"
)

func TestRuntimeDiagnosticsObservePCM(t *testing.T) {
	d := &runtimeDiagnostics{}
	pcm := make([]byte, 320*2)
	for i := 0; i < 320; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(1000)))
	}
	d.observePCM(pcm)
	if got := d.pcmChunks.Load(); got != 1 { t.Fatalf("chunks = %d, want 1", got) }
	if got := d.pcmBytes.Load(); got != uint64(len(pcm)) { t.Fatalf("bytes = %d, want %d", got, len(pcm)) }
	if got := d.pcmSamples.Load(); got != 320 { t.Fatalf("samples = %d, want 320", got) }
	if got := d.lastPeak.Load(); got != 1000 { t.Fatalf("peak = %d, want 1000", got) }
	if got := d.lastVoiceUnixNano.Load(); got == 0 { t.Fatal("non-silent PCM did not update voice timestamp") }
	if got := d.maxCapturePeak.Load(); got != 1000 { t.Fatalf("max capture peak = %d, want 1000", got) }
	if got := d.maxCaptureBEPeak.Load(); got != 59395 { t.Fatalf("byte-swapped max capture peak = %d, want 59395", got) }
	if !strings.Contains(d.snapshot(), "detector_frames=0") { t.Fatal("snapshot missing counters") }
}

func TestRuntimeDiagnosticsSilenceAndInvalidPCM(t *testing.T) {
	d := &runtimeDiagnostics{}
	d.observePCM([]byte{1})
	if got := d.pcmChunks.Load(); got != 0 { t.Fatalf("invalid PCM counted: %d", got) }
	d.observePCM(make([]byte, 640))
	if got := d.lastVoiceUnixNano.Load(); got != 0 { t.Fatal("silence marked as voice") }
	if got := d.firstPCMUnixNano.Load(); got == 0 { t.Fatal("first PCM timestamp missing") }
	time.Sleep(time.Millisecond)
	if !strings.Contains(d.snapshot(), "silence=") { t.Fatal("snapshot missing silence duration") }
}

func TestOutboundPCMLevelsAreMeasuredBeforeEncoding(t *testing.T) {
	d := &runtimeDiagnostics{}
	d.observeOutboundPCM([]int16{1000, -1000, 500, -500})
	d.observeOutboundPCM(make([]int16, 4))
	if got := d.outboundPCMFrames.Load(); got != 2 { t.Fatalf("outbound frames = %d, want 2", got) }
	if got := d.outboundVoiceFrames.Load(); got != 1 { t.Fatalf("voice frames = %d, want 1", got) }
	if got := d.maxOutboundPeak.Load(); got != 1000 { t.Fatalf("max outbound peak = %d, want 1000", got) }
	if got := math.Float64frombits(d.maxOutboundRMSBits.Load()); got < 790 || got > 800 { t.Fatalf("max outbound RMS = %.1f, want about 790.6", got) }
	if !strings.Contains(d.snapshot(), "opus_input_voice_frames=1") { t.Fatal("snapshot missing outbound PCM counters") }
}

func TestInboundPCMAndPlaybackDiagnostics(t *testing.T) {
	d := &runtimeDiagnostics{}
	d.observeInboundPCM([]int16{1000, -1000, 500, -500})
	d.observeInboundPCM(make([]int16, 4))
	if got := d.inboundPCMBlocks.Load(); got != 2 {
		t.Fatalf("inbound PCM blocks = %d, want 2", got)
	}
	if got := d.inboundPCMSilentBlocks.Load(); got != 1 {
		t.Fatalf("silent inbound blocks = %d, want 1", got)
	}
	if got := d.lastInboundPCMPeak.Load(); got != 0 {
		t.Fatalf("last inbound peak = %d, want 0 for silence", got)
	}
	d.setPlaybackQueueDepth(3)
	d.setPlaybackQueueDepth(7)
	d.setPlaybackQueueDepth(2)
	if got := d.playbackQueueDepth.Load(); got != 2 {
		t.Fatalf("queue depth = %d, want 2", got)
	}
	if got := d.playbackQueueMaxDepth.Load(); got != 7 {
		t.Fatalf("max queue depth = %d, want 7", got)
	}
	output := make([]byte, 16)
	binary.LittleEndian.PutUint16(output[0:], uint16(int16(2000)))
	negativeSample := int16(-2000)
	binary.LittleEndian.PutUint16(output[2:], uint16(negativeSample))
	d.observePlaybackOutput(output, 1)
	if got := d.lastPlaybackPeak.Load(); got != 2000 {
		t.Fatalf("playback peak = %d, want 2000", got)
	}
	if got := d.playbackNonzeroFrames.Load(); got != 1 {
		t.Fatalf("nonzero playback frames = %d, want 1", got)
	}
	if got := d.playbackSilentCallbacks.Load(); got != 0 {
		t.Fatalf("silent playback callbacks = %d, want 0", got)
	}
}
