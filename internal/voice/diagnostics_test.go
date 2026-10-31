package voice

import (
	"encoding/binary"
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
