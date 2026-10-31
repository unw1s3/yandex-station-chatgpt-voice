package voice

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

const detectorQueueFrames = 40
const captureQueueFrames = 600
const setupTimeout = 8 * time.Second

var errAlicePriority = errors.New("native Alice has priority")
var errCleanup = errors.New("resource cleanup failed")
var errStationAudio = errors.New("Station audio")
var errCaptureOverflow = fmt.Errorf("%w: processed capture queue overflow", errStationAudio)

type voiceCallError struct{ error }

func (e voiceCallError) Unwrap() error { return e.error }

type stationEpoch struct {
	number uint64
	ctx    context.Context
	cancel context.CancelCauseFunc
	output *audio
	report *log.Logger
	debug *log.Logger
	diag *runtimeDiagnostics
	led  *stationLED
}

func newStationEpoch(parent context.Context, number uint64) *stationEpoch {
	ctx, cancel := context.WithCancelCause(parent)
	e := &stationEpoch{number: number, ctx: ctx, cancel: cancel, report: log.New(io.Discard, "", 0), debug: log.New(io.Discard, "", 0), diag: &runtimeDiagnostics{}, led: newStationLED("/sys/class/leds")}
	e.output = newAudio(e)
	return e
}

func (e *stationEpoch) revoke(cause error) {
	e.cancel(cause)
}

func (e *stationEpoch) allowed() bool {
	return e.ctx.Err() == nil
}

type stationGate struct {
	mu      sync.Mutex
	state   aliceState
	number  uint64
	current *stationEpoch
	changed chan struct{}
}

func newStationGate() *stationGate { return &stationGate{changed: make(chan struct{}, 1)} }

func (g *stationGate) observe(ev nativeEvent) {
	if !ev.activation && !ev.stateChanged {
		return
	}
	g.mu.Lock()
	if ev.activation {
		g.state = aliceUnknown
	} else {
		g.state = ev.state
	}
	if g.state != aliceIdle {
		g.number++
		if g.current != nil {
			g.current.revoke(errAlicePriority)
		}
	}
	g.mu.Unlock()
	select {
	case g.changed <- struct{}{}:
	default:
	}
}

func (g *stationGate) begin(ctx context.Context) *stationEpoch {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != aliceIdle || g.current != nil || ctx.Err() != nil {
		return nil
	}
	g.number++
	g.current = newStationEpoch(ctx, g.number)
	return g.current
}

func (g *stationGate) activate(e *stationEpoch) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cause := context.Cause(e.ctx); cause != nil {
		return cause
	}
	if g.state != aliceIdle || g.current != e || g.number != e.number {
		return errAlicePriority
	}
	return nil
}

func (g *stationGate) completion(parent context.Context, old *stationEpoch) *stationEpoch {
	g.mu.Lock()
	defer g.mu.Unlock()
	if parent.Err() != nil || g.current != old || g.number != old.number || g.state != aliceIdle {
		return nil
	}
	old.revoke(context.Canceled)
	g.number++
	g.current = newStationEpoch(parent, g.number)
	return g.current
}

func (g *stationGate) finish(e *stationEpoch, result error) error {
	if cause := context.Cause(e.ctx); errors.Is(cause, errAlicePriority) {
		result = errors.Join(result, cause)
	}
	e.revoke(context.Canceled)
	if err := e.led.restore(); err != nil {
		e.debug.Print("Station LED restore failed: ", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.current == e {
		g.current = nil
		g.number++
	}
	return result
}

type capturePhase uint8

const (
	captureDetecting capturePhase = iota
	captureConnecting
	captureStreaming
)

type stationCapture struct {
	mu       sync.Mutex
	epoch    *stationEpoch
	phase    capturePhase
	partial  captureFrame
	used     int
	detector chan captureFrame
	input    chan captureFrame
}

func newStationCapture(e *stationEpoch) *stationCapture {
	return &stationCapture{
		epoch:    e,
		partial:  captureFrame{epoch: e.number},
		detector: make(chan captureFrame, detectorQueueFrames),
		input:    make(chan captureFrame, captureQueueFrames),
	}
}

func (c *stationCapture) beginConnecting() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.epoch.allowed() {
		return context.Cause(c.epoch.ctx)
	}
	if c.phase != captureDetecting {
		return errors.New("processed capture is not detecting")
	}
	c.phase = captureConnecting
	c.used = 0
	return nil
}

func (c *stationCapture) beginStreaming(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if !c.epoch.allowed() {
		return context.Cause(c.epoch.ctx)
	}
	if c.phase != captureConnecting {
		return errors.New("processed capture is not connecting")
	}
	c.phase = captureStreaming
	c.used = 0
	return nil
}

func (c *stationCapture) pushPCM(pcm []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.epoch.allowed() {
		return context.Cause(c.epoch.ctx)
	}
	c.epoch.diag.observePCM(pcm)
	if c.phase == captureConnecting {
		return nil
	}
	output := c.detector
	if c.phase == captureStreaming {
		output = c.input
	}
	for len(pcm) > 0 {
		c.partial.pcm[c.used] = int16(binary.LittleEndian.Uint16(pcm))
		pcm = pcm[2:]
		c.used++
		if c.used != captureSamples {
			continue
		}
		if !c.epoch.allowed() {
			return context.Cause(c.epoch.ctx)
		}
		select {
		case output <- c.partial:
			if c.phase == captureStreaming { c.epoch.diag.streamingFrames.Add(1) } else { c.epoch.diag.detectorFrames.Add(1) }
			c.partial.sequence++
			c.used = 0
		default:
			c.epoch.diag.captureQueueDrops.Add(1)
			c.epoch.revoke(errCaptureOverflow)
			return errCaptureOverflow
		}
	}
	return nil
}

func runStation(parent context.Context, credentialFile string, settings sessionSettings, out io.Writer) error {
	auth, err := loadCredential(credentialFile)
	if err != nil {
		return err
	}
	debugFile, err := openRotatingLog(debugLogPath, debugLogMax)
	if err != nil {
		return err
	}
	defer debugFile.Close()
	debug := log.New(debugFile, "", log.LstdFlags|log.Lmicroseconds)
	conn, err := dialStation(parent, "127.0.0.1:9875")
	if err != nil {
		if parent.Err() != nil {
			return parent.Err()
		}
		return errors.New("cannot connect to native Alice state")
	}
	return serveStation(parent, conn, auth, out, func(e *stationEpoch, gate *stationGate, auth credential) error {
		return runStationEpoch(e, gate, auth, settings)
	}, playEndCue, debug)
}

func serveStation(parent context.Context, conn net.Conn, auth credential, out io.Writer, attempt func(*stationEpoch, *stationGate, credential) error, endCue func(context.Context) error, debugLoggers ...*log.Logger) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	gate := newStationGate()
	report := log.New(out, "", 0)
	debug := log.New(io.Discard, "", 0)
	if len(debugLoggers) > 0 && debugLoggers[0] != nil {
		debug = debugLoggers[0]
	}
	defer report.Print("Station voice stopped.")
	stateDone := make(chan error, 1)
	var stateWorker sync.WaitGroup
	stateWorker.Go(func() { stateDone <- readAlice(ctx, conn, gate) })
	defer func() { cancel(); stateWorker.Wait() }()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-stateDone:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("native Alice state transport ended: %w", err)
		default:
		}
		if epoch := gate.begin(ctx); epoch != nil {
			epoch.report = report
			epoch.debug = debug
			err := attempt(epoch, gate, auth)
			_, callFailed := errors.AsType[voiceCallError](err)
			if callFailed && !errors.Is(context.Cause(epoch.ctx), errAlicePriority) {
				if ledErr := epoch.led.set(stationLEDError); ledErr != nil {
					epoch.debug.Print("Station LED update failed: ", ledErr)
				} else {
					timer := time.NewTimer(900 * time.Millisecond)
				errorVisible:
					for {
						select {
						case <-timer.C:
							break errorVisible
						case <-ctx.Done():
							timer.Stop()
							break errorVisible
						case <-gate.changed:
							gate.mu.Lock()
							nativeActive := gate.state != aliceIdle
							gate.mu.Unlock()
							if nativeActive {
								timer.Stop()
								break errorVisible
							}
						}
					}
				}
			}
			var cue *stationEpoch
			if err == nil {
				cue = gate.completion(ctx, epoch)
			}
			err = gate.finish(epoch, err)
			if cue != nil {
				cueErr := gate.finish(cue, endCue(cue.ctx))
				if cueErr != nil && !errors.Is(cueErr, errAlicePriority) && ctx.Err() == nil {
					report.Print("Completion cue failed: ", cueErr)
				}
			}
			if dropped := epoch.output.playbackDrops.Load(); dropped > 0 {
				report.Printf("Playback queue overflow: %d audio frames dropped.", dropped)
			}
			if errors.Is(err, errStationAudio) || errors.Is(err, errCleanup) {
				return err
			}
			if callFailed {
				report.Print("Voice attempt failed: ", err)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil && !callFailed && !errors.Is(err, errAlicePriority) && !errors.Is(err, context.Canceled) {
				return err
			}
			if errors.Is(err, errAlicePriority) {
				report.Print("Voice yielded to native Alice.")
			} else if err == nil {
				report.Print("Voice call ended normally.")
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-stateDone:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("native Alice state transport ended: %w", err)
		case <-gate.changed:
		}
	}
}

func runStationEpoch(e *stationEpoch, gate *stationGate, auth credential, settings sessionSettings) (result error) {
	detector, err := newNativeSpotter()
	if err != nil {
		e.diag.spotterInitErrors.Add(1)
		return err
	}
	defer func() {
		if detector != nil {
			result = errors.Join(result, detector.close())
		}
	}()
	if !e.allowed() {
		return context.Cause(e.ctx)
	}
	conn, err := dialStation(e.ctx, "127.0.0.1:9850")
	if err != nil {
		if !e.allowed() {
			return context.Cause(e.ctx)
		}
		return errors.New("cannot connect to native processed audio")
	}
	capture := newStationCapture(e)
	defer func() { e.debug.Print(e.diag.snapshot()) }()
	var reader sync.WaitGroup
	reader.Go(func() {
		if err := readCapture(e.ctx, conn, capture); err != nil {
			if e.ctx.Err() == nil {
				e.diag.captureReadErrors.Add(1)
				if errors.Is(err, errCaptureStall) { e.diag.captureStalls.Add(1) }
				e.revoke(fmt.Errorf("%w: %w", errStationAudio, err))
			}
		}
	})
	defer func() { e.revoke(context.Canceled); reader.Wait() }()
	e.report.Print("Yandex wake detection armed.")
	for {
		select {
		case <-e.ctx.Done():
			return context.Cause(e.ctx)
		case frame := <-capture.detector:
			if !e.allowed() {
				return context.Cause(e.ctx)
			}
			confirmed, err := detector.feed(frame.pcm[:])
			if err != nil {
				e.diag.spotterFeedErrors.Add(1)
				return err
			}
			if !confirmed {
				continue
			}
			e.diag.wakeDetections.Add(1)
			if err := gate.activate(e); err != nil {
				return err
			}
			if err := capture.beginConnecting(); err != nil {
				return err
			}
			err = detector.close()
			detector = nil
			if err != nil {
				return err
			}
			e.report.Print("Yandex wake confirmed; connecting voice.")
			if err := e.led.activate(); err != nil {
				e.debug.Print("Station LED activation unavailable: ", err)
			}
			err = runCall(e, auth, capture, settings)
			if cause := context.Cause(e.ctx); errors.Is(cause, errStationAudio) && !errors.Is(err, errStationAudio) {
				return errors.Join(cause, err)
			}
			return err
		}
	}
}
