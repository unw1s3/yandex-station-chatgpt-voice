package voice

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	"github.com/pion/sctp"
	"github.com/pion/webrtc/v4"
)

const signalingURL = "https://chatgpt.com/realtime/wm?dcid=0"
const maxAnswerBytes = 1 << 20
const maxSCTPReceiveBufferBytes = 1 << 20

var errRemoteClosed = errors.New("remote voice connection closed")
var errProtocolHangup = errors.New("voice protocol hangup")

type peer struct {
	pc        *webrtc.PeerConnection
	dc        *webrtc.DataChannel
	mic       *webrtc.TrackLocalStaticSample
	ctx       context.Context
	cancel    context.CancelCauseFunc
	ready     chan struct{}
	tracks    chan *webrtc.TrackRemote
	trackSeen atomic.Bool
	wg        sync.WaitGroup
	close     func() error
	closing   atomic.Bool
	report    *log.Logger
	failureMu sync.Mutex
	failure   error
	diag      *runtimeDiagnostics
}

func newPeer(ctx context.Context, settings webrtc.SettingEngine, servers []webrtc.ICEServer, report *log.Logger, diagnostics ...*runtimeDiagnostics) (*peer, error) {
	settings.SetSCTPMaxMessageSize(maxMessageBytes)
	settings.SetSCTPMaxReceiveBufferSize(maxSCTPReceiveBufferBytes)
	settings.LoggerFactory = &logging.DefaultLoggerFactory{Writer: io.Discard, DefaultLogLevel: logging.LogLevelDisabled}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settings))
	ctx, cancel := context.WithCancelCause(ctx)
	p := &peer{report: report, ctx: ctx, cancel: cancel, ready: make(chan struct{}), tracks: make(chan *webrtc.TrackRemote, 1)}
	if len(diagnostics) > 0 { p.diag = diagnostics[0] }
	var err error
	p.pc, err = api.NewPeerConnection(webrtc.Configuration{ICEServers: servers, BundlePolicy: webrtc.BundlePolicyMaxBundle})
	if err != nil {
		cancel(err)
		return nil, errors.New("cannot create WebRTC peer")
	}
	p.close = sync.OnceValue(func() error {
		p.closing.Store(true)
		p.cancel(context.Canceled)
		// Peer closure tears down SCTP before joining the DataChannel read loop.
		pcErr := p.pc.GracefulClose()
		p.wg.Wait()
		if pcErr != nil {
			return fmt.Errorf("%w: WebRTC did not close cleanly", errCleanup)
		}
		return nil
	})
	fail := func(message string) (*peer, error) { return nil, errors.Join(errors.New(message), p.close()) }
	p.mic, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "minptime=10;useinbandfec=1"}, "microphone", "voice")
	if err != nil {
		return fail("cannot create Opus track")
	}
	tx, err := p.pc.AddTransceiverFromTrack(p.mic, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendrecv})
	if err != nil {
		return fail("cannot attach Opus track")
	}
	codecs := tx.Sender().GetParameters().Codecs
	var opusCodecs []webrtc.RTPCodecParameters
	for _, c := range codecs {
		if c.MimeType == webrtc.MimeTypeOpus && c.ClockRate == 48000 && c.Channels == 2 {
			opusCodecs = append(opusCodecs, c)
		}
	}
	if len(opusCodecs) == 0 || tx.SetCodecPreferences(opusCodecs) != nil {
		return fail("cannot select Opus codec")
	}
	p.dc, err = p.pc.CreateDataChannel("", &webrtc.DataChannelInit{Negotiated: new(true), Ordered: new(true), ID: new(uint16(0))})
	if err != nil {
		return fail("cannot create voice control channel")
	}
	p.dc.OnOpen(sync.OnceFunc(func() { close(p.ready); p.report.Print("WebRTC DataChannel open.") }))
	p.dc.OnMessage(func(message webrtc.DataChannelMessage) {
		if !message.IsString {
			p.fail(errors.New("unexpected binary voice control message"))
			return
		}
		p.handleMessage(message.Data)
	})
	p.dc.OnClose(func() { p.report.Print("WebRTC DataChannel closed."); p.cancel(errRemoteClosed) })
	p.dc.OnError(p.channelError)
	var disconnected atomic.Bool
	p.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		p.report.Printf("WebRTC peer connection state: %s; ICE=%s.", state, p.pc.ICEConnectionState())
		switch state {
		case webrtc.PeerConnectionStateDisconnected:
			disconnected.Store(true)
			p.report.Print("Voice transport disconnected; waiting for native recovery.")
		case webrtc.PeerConnectionStateConnected:
			if disconnected.Swap(false) {
				p.report.Print("Voice transport recovered.")
			}
		case webrtc.PeerConnectionStateFailed:
			p.fail(errors.New("WebRTC connection lost"))
		case webrtc.PeerConnectionStateClosed:
			p.cancel(errRemoteClosed)
		}
	})
	p.pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if p.ctx.Err() != nil {
			return
		}
		if track.Kind() != webrtc.RTPCodecTypeAudio || !p.trackSeen.CompareAndSwap(false, true) {
			p.cancel(errors.New("unexpected additional remote media track"))
			return
		}
		p.report.Printf("WebRTC remote audio track received; codec=%s clock=%d channels=%d.", track.Codec().MimeType, track.Codec().ClockRate, track.Codec().Channels)
		select {
		case p.tracks <- track:
		case <-p.ctx.Done():
		default:
			p.cancel(errors.New("remote track mailbox overflow"))
		}
	})
	p.wg.Go(func() {
		buffer := make([]byte, 1500)
		for {
			if _, _, err := tx.Sender().Read(buffer); err != nil {
				if p.ctx.Err() == nil && !closedTransport(err) {
					p.fail(errors.New("microphone RTCP transport failed"))
				}
				return
			}
		}
	})
	return p, nil
}

func (p *peer) statsSnapshot() string {
	var outPackets, inPackets uint64
	var outBytes, inBytes uint64
	var packetsLost int64
	var jitterMS, rttMS float64
	var nominatedState string
	for _, stat := range p.pc.GetStats() {
		switch value := stat.(type) {
		case webrtc.OutboundRTPStreamStats:
			if value.Kind == "audio" {
				outPackets += uint64(value.PacketsSent)
				outBytes += value.BytesSent
			}
		case webrtc.InboundRTPStreamStats:
			if value.Kind == "audio" {
				inPackets += uint64(value.PacketsReceived)
				inBytes += value.BytesReceived
				packetsLost += int64(value.PacketsLost)
				jitterMS = value.Jitter * 1000
			}
		case webrtc.ICECandidatePairStats:
			if value.Nominated {
				nominatedState = string(value.State)
				rttMS = value.CurrentRoundTripTime * 1000
			}
		}
	}
	return fmt.Sprintf("WebRTC stats: peer=%s ice=%s datachannel=%s audio_out_packets=%d audio_out_bytes=%d audio_in_packets=%d audio_in_bytes=%d audio_packets_lost=%d jitter_ms=%.2f nominated_pair=%s rtt_ms=%.2f",
		p.pc.ConnectionState(), p.pc.ICEConnectionState(), p.dc.ReadyState(),
		outPackets, outBytes, inPackets, inBytes, packetsLost, jitterMS,
		nominatedState, rttMS)
}

func closedTransport(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled)
}

func (p *peer) fail(err error) {
	p.failureMu.Lock()
	if p.failure == nil {
		p.failure = err
	}
	p.failureMu.Unlock()
	p.cancel(err)
}

func (p *peer) channelError(err error) {
	// Pion's Stop aborts SCTP; its final stream reset can race association closure.
	if closedTransport(err) || errors.Is(err, sctp.ErrUserInitiatedAbort) || errors.Is(err, sctp.ErrResetPacketInStateNotExist) {
		return
	}
	// SCTP Stop forces a DTLS read deadline to unblock local teardown.
	if p.closing.Load() && errors.Is(err, context.DeadlineExceeded) {
		return
	}
	// Pion can dispatch this before shutdown and run it after OnClose or GracefulClose.
	p.fail(errors.New("voice control channel failed"))
	p.report.Print("Voice control channel failed.")
}

func (p *peer) outcome(result error) error {
	p.failureMu.Lock()
	defer p.failureMu.Unlock()
	if result == errProtocolHangup {
		result = nil
	}
	if p.failure != nil && !errors.Is(result, p.failure) {
		result = errors.Join(result, p.failure)
	}
	return result
}

func (p *peer) handleMessage(data []byte) {
	if p.ctx.Err() != nil { return }
	ev, err := decodeEvent(data)
	if err != nil { p.fail(err); return }
	if p.diag != nil { p.diag.observeRealtimeEvent(ev) }
	if ev.kind != "" {
		if ev.status != "" { p.report.Printf("ChatGPT event: %s status=%s.", ev.kind, ev.status)
		} else if ev.failure != "" { p.report.Printf("ChatGPT event: %s category=%s.", ev.kind, ev.failure)
		} else { p.report.Printf("ChatGPT event: %s.", ev.kind) }
	}
	if ev.err != nil {
		p.report.Printf("ChatGPT response error: %v.", ev.err)
		p.fail(ev.err)
	} else if ev.ended {
		p.report.Print("ChatGPT ended the voice session normally.")
		p.cancel(errProtocolHangup)
	}
}

func (p *peer) offer(ctx context.Context) (string, error) {
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return "", errors.New("cannot create voice offer")
	}
	gathered := webrtc.GatheringCompletePromise(p.pc)
	if p.pc.SetLocalDescription(offer) != nil {
		return "", errors.New("cannot set voice offer")
	}
	gatherCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	select {
	case <-gatherCtx.Done():
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
		return "", errors.New("ICE gathering did not complete before timeout")
	case <-p.ctx.Done():
		return "", context.Cause(p.ctx)
	case <-gathered:
	}
	if gatherCtx.Err() != nil {
		return "", errors.New("ICE gathering did not complete before cancellation or timeout")
	}
	description := p.pc.LocalDescription()
	if description == nil || p.pc.ICEGatheringState() != webrtc.ICEGatheringStateComplete {
		return "", errors.New("ICE offer is incomplete")
	}
	return description.SDP, nil
}

func (p *peer) accept(answer string) error {
	if p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}) != nil {
		return errors.New("ChatGPT SDP answer was not accepted by the native peer")
	}
	return nil
}

func signalingClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func exchangeOffer(ctx context.Context, client *http.Client, auth credential, offer string, session []byte) (string, error) {
	return exchangeOfferObserved(ctx, client, auth, offer, session, nil)
}

func exchangeOfferObserved(ctx context.Context, client *http.Client, auth credential, offer string, session []byte, diag *runtimeDiagnostics) (string, error) {
	if !validCredential(auth) {
		return "", errors.New("invalid web credential")
	}
	if len(offer) > maxAnswerBytes || !strings.HasPrefix(offer, "v=0") {
		return "", errors.New("invalid local SDP offer")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("sdp", offer); err != nil {
		return "", errors.New("cannot encode offer")
	}
	if err := form.WriteField("session", string(session)); err != nil {
		return "", errors.New("cannot encode voice session")
	}
	if form.Close() != nil {
		return "", errors.New("cannot finish offer")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, signalingURL, &body)
	if err != nil {
		return "", errors.New("cannot prepare signaling request")
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("User-Agent", "yandex-station-chatgpt-voice/0.1")
	req.Header.Set("Oai-Device-Id", auth.DeviceID)
	req.Header.Set("Oai-Language", "en-US")
	req.Header.Set("Oai-Client-Version", "prod-fb4a8a2a751dfec391053cfd7b01c52699ccf78c")
	req.Header.Set("Oai-Client-Build-Number", "8370486")
	req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := safeClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
		return "", errors.New("voice signaling HTTPS request failed; check network, TLS, and service availability")
	}
	defer response.Body.Close()
	if diag != nil { diag.signalingHTTPStatus.Store(int32(response.StatusCode)) }
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return "", errors.New("ChatGPT authentication rejected (HTTP 401); check the auth import and refresh the web session")
	case http.StatusForbidden:
		return "", errors.New("ChatGPT voice access forbidden (HTTP 403); account entitlement or access policy")
	case http.StatusTooManyRequests:
		return "", errors.New("ChatGPT signaling rate limit exceeded (HTTP 429)")
	case http.StatusPaymentRequired:
		return "", errors.New("ChatGPT signaling quota or billing limit (HTTP 402)")
	case http.StatusOK, http.StatusCreated:
	default:
		return "", fmt.Errorf("ChatGPT voice signaling returned HTTP %d", response.StatusCode)
	}
	answer, err := io.ReadAll(io.LimitReader(response.Body, maxAnswerBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
		return "", errors.New("cannot read voice SDP answer")
	}
	if len(answer) > maxAnswerBytes {
		return "", errors.New("voice SDP answer exceeds 1 MiB")
	}
	sdp := strings.TrimSpace(string(answer))
	if !strings.HasPrefix(sdp, "v=0\r\n") && !strings.HasPrefix(sdp, "v=0\n") {
		return "", errors.New("ChatGPT did not return an SDP answer")
	}
	return sdp + "\r\n", nil
}

func runCall(e *stationEpoch, auth credential, capture *stationCapture, settings sessionSettings) (result error) {
	defer func() {
		switch result {
		case nil, errAlicePriority, context.Canceled:
			return
		case context.DeadlineExceeded:
			if e.ctx.Err() == context.DeadlineExceeded {
				return
			}
		}
		if !errors.Is(result, errStationAudio) {
			result = voiceCallError{result}
		}
	}()
	if !validCredential(auth) {
		return errors.New("invalid web credential")
	}
	if !e.allowed() {
		return context.Cause(e.ctx)
	}
	ctx := e.ctx
	setupStarted := time.Now()
	var workers sync.WaitGroup
	p, err := newPeer(ctx, webrtc.SettingEngine{}, []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302", "stun:stun4.l.google.com:19302"}}}, e.report, e.diag)
	if err != nil {
		return err
	}
	shutdown := sync.OnceValue(func() error {
		e.revoke(context.Canceled)
		return errors.Join(e.output.close(), p.close())
	})
	stopShutdown := context.AfterFunc(ctx, func() { _ = shutdown() })
	defer func() {
		e.revoke(context.Canceled)
		cleanupErr := shutdown()
		stopShutdown()
		workers.Wait()
		result = p.outcome(result)
		if cleanupErr != nil {
			result = errors.Join(result, cleanupErr)
		}
	}()
	workers.Go(func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-p.ctx.Done(): return
			case <-ticker.C: e.report.Print(p.statsSnapshot())
			}
		}
	})
	stopPeerCancel := context.AfterFunc(p.ctx, func() { e.revoke(context.Cause(p.ctx)) })
	defer stopPeerCancel()
	setupCtx, finishSetup := context.WithTimeout(ctx, setupTimeout)
	defer finishSetup()
	stopSetupTimeout := context.AfterFunc(setupCtx, func() {
		if errors.Is(setupCtx.Err(), context.DeadlineExceeded) {
			e.revoke(errors.New("voice connection setup timed out"))
		}
	})
	defer stopSetupTimeout()
	offer, err := p.offer(setupCtx)
	if err != nil {
		return err
	}
	session, err := encodeSession(time.Now(), settings)
	if err != nil {
		return errors.New("cannot encode voice session")
	}
	client := signalingClient()
	defer client.CloseIdleConnections()
	signalingStarted := time.Now()
	e.diag.signalingAttempts.Add(1)
	answer, err := exchangeOfferObserved(setupCtx, client, auth, offer, session, e.diag)
	signalingDuration := time.Since(signalingStarted)
	e.diag.signalingDurationMs.Store(signalingDuration.Milliseconds())
	e.diag.signalingSuccess.Store(err == nil)
	if err != nil {
		e.report.Printf("ChatGPT signaling failed after %s: %v", signalingDuration.Round(time.Millisecond), err)
		return err
	}
	e.report.Printf("ChatGPT signaling accepted in %s.", signalingDuration.Round(time.Millisecond))
	if !e.allowed() {
		return context.Cause(ctx)
	}
	if err = p.accept(answer); err != nil {
		return err
	}
	ready := false
	for !ready {
		select {
		case <-setupCtx.Done():
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return errors.New("voice connection setup timed out")
		case <-p.ready:
			ready = true
		}
	}
	if !e.allowed() {
		return context.Cause(ctx)
	}
	if setupCtx.Err() != nil {
		return errors.New("voice setup ended before media became ready")
	}
	if err = e.output.open(); err != nil {
		return err
	}
	if !e.allowed() {
		return context.Cause(ctx)
	}
	if setupCtx.Err() != nil {
		return errors.New("voice setup ended while opening playback")
	}
	if err = playCue(setupCtx, 880); err != nil {
		return err
	}
	if err = capture.beginStreaming(setupCtx); err != nil {
		return err
	}
	finishSetup()
	e.diag.setupDurationMs.Store(time.Since(setupStarted).Milliseconds())
	e.report.Printf("Voice media ready; setup=%s.", time.Since(setupStarted).Round(time.Millisecond))
	e.report.Print(p.statsSnapshot())
	workers.Go(func() {
		if err := e.output.send(ctx, p.mic, capture.input); err != nil {
			e.revoke(err)
		}
	})
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case track := <-p.tracks:
			workers.Go(func() {
				if err := e.output.receive(ctx, track); err != nil {
					e.revoke(err)
				}
			})
		}
	}
}
