# Diagnostics implementation plan

## Scope

Add an on-device diagnostics subsystem to Yandex Station ChatGPT Voice. Home Assistant, MQTT, remote monitoring, automatic repair, audio recording, and firmware changes are out of scope.

All changes are developed on `feature/diagnostics-core`. The upstream/default `main` branch must remain untouched. Diagnostics must not change native Alice priority, create an additional ChatGPT voice session, or interfere with the active audio IPC stream.

## Current code map (baseline: main at 0739f0855f0a6d54b875f5286ae196e37a724838)

- `cmd/yandex-station-chatgpt-voice/main.go`: process entry point; forwards CLI arguments to `voice.Run`.
- `internal/voice/cli.go`: CLI currently supports run, auth import, and help.
- `internal/voice/station.go`: Station lifecycle, native Alice priority gate, per-call epoch, wake-word loop, and processed-audio capture queue.
- `internal/voice/station_ipc.go`: protobuf IPC framing and decoding; native Alice state IPC at `127.0.0.1:9875`; processed audio IPC at `127.0.0.1:9850`; `readCapture` uses a 3-second audio-stall deadline.
- `internal/voice/station_native.go`: Yandex wake-word model overlay and native spotter initialization/feed/cleanup.
- `internal/voice/audio.go`: PCM-to-Opus sending, incoming Opus/RTP decode, ALSA playback through malgo, playback queue and drop counter.
- `internal/voice/call.go`: ChatGPT signaling request, credential use, Pion PeerConnection/DataChannel lifecycle, remote audio track handling.
- `internal/voice/protocol.go`: voice-session payload and control-event decoding.
- `tools/station.py`: host-side ADB management, installation, process identity verification, and current status checks.
- `Makefile`: existing status/logs/device-management targets and host test/check targets.

## Important constraints discovered in the current implementation

1. The application does not open a physical microphone directly. It receives firmware-processed PCM from the Station over the existing native IPC connection at 127.0.0.1:9850.
2. The IPC audio reader is part of the live wake-word/audio pipeline. A second reader or a diagnostic audio request can compete with the application and is prohibited while the client is running.
3. A successful PCM stream proves that processed audio reaches the application; it does not independently prove the physical microphone hardware is healthy.
4. The wake-word detector uses the stock Yandex model and a native spotter. Initialization and feed failures can be reported, but absence of wake-word detections alone is not proof of a detector fault.
5. Playback uses ALSA through malgo. The existing playback drop counter is reported when a Station epoch finishes; playback device errors revoke the epoch.
6. ChatGPT signaling uses the website voice endpoint and the account web credential. A credential-file existence check is not proof that the credential is currently accepted.
7. The application already handles HTTP 401/403, signaling errors, ICE gathering timeout, WebRTC connection failure, DataChannel closure, media decode errors, and playback failures. Diagnostics should preserve these error paths and expose structured events rather than replace them.
8. WebRTC statistics must be read from the existing PeerConnection. No second PeerConnection or test voice session may be created by normal diagnostics.

## Design principles

- Diagnostics are observational: no automatic restart, re-authentication, configuration edits, or repair.
- Never record audio, transcripts, tokens, authorization headers, or complete SDP.
- Use existing connections and callbacks; do not add competing consumers to the audio IPC stream.
- Keep hot-path instrumentation lightweight: atomic counters and bounded in-memory event storage; no per-frame logging.
- Diagnostics must not block PCM capture, wake-word processing, RTP callbacks, or ALSA callbacks.
- All background workers must be context-bound and shut down cleanly.
- A diagnostic failure must never crash or stop the voice client.
- Distinguish OK, WARN, FAIL, and SKIP. SKIP means a check could not safely or meaningfully run; it is not a failure.
- Keep user-facing explanations actionable while retaining safe technical detail for developers.

## Planned implementation stages

### Stage 1 — safe baseline diagnostics

Add a read-only `diagnose` action to the existing host-side Station management tool and expose it as `make diagnose`. It will inspect only safe state already available through ADB: root ADB, installation files and permissions, verified client process identity, runtime dependencies/model files, and local IPC listener presence where that can be checked without opening the streams.

The first stage must not connect to or read from 9850/9875, invoke the native spotter, initialize ALSA, or contact ChatGPT. It must explicitly mark live PCM, physical microphone, wake-word recognition, ChatGPT authorization, WebRTC media, and speaker output as SKIP when they cannot be safely verified without interacting with the live application.

Add a report mode that can save the same sanitized report locally. Preserve the existing `make status` and `make logs` behavior.

### Stage 2 — runtime audio and wake-word telemetry

Implementation started on `feature/diagnostics-core`: a per-epoch atomic diagnostics state observes the existing `pushPCM` path and native spotter feed. It records PCM chunks/bytes/samples, last PCM age, RMS/peak, elapsed silence, detector/streaming frame counts, capture stalls/read errors/queue overflows, spotter initialization/feed errors, and wake detections. A context-bound monitor emits a sanitized snapshot every five seconds while an epoch is active. PCM is analyzed in place and never retained; no second IPC reader is created. Host tests cover PCM metrics and silence handling. Hardware verification and the full test gate remain pending.

### Stage 3 — call, signaling, WebRTC, and media telemetry

Instrument existing HTTP signaling and PeerConnection/DataChannel callbacks. Record request duration/status class, setup duration, state transitions, RTP/media counters and available RTT/jitter/loss metrics. Read statistics from the existing peer only. Never log credentials, SDP, payloads, or conversation content.

### Stage 4 — bounded event history and reports

Add a bounded event ring (initial target 200 events), periodic snapshots, and a sanitized report that can be requested without changing call state. Persist only selected diagnostic events if needed; enforce size/rotation limits.

### Stage 5 — explicit extended tests

Only by explicit user request, add isolated tests such as a short speaker test tone. No test may run automatically during normal startup or quick diagnostics. Clearly distinguish software playback-path success from physical speaker verification.

## Initial command contract

- `make diagnose`: quick, read-only baseline report; no new voice session and no audio-stream reads.
- `make diagnose-full`: reserved for later stages; must remain unavailable or clearly report not implemented until safe runtime checks exist.
- `make report`: write a sanitized report file locally; never include credentials or conversation content.
- `make status` and `make logs`: preserve existing behavior.

## Acceptance gates

Each stage must:
- pass formatting, Python helper tests, Go race tests, and `go vet` where supported by the project toolchain;
- include focused tests for new behavior and error cases;
- leave the default `main` branch unchanged;
- be reviewed for concurrency, shutdown, privacy, and impact on native Alice/audio behavior;
- be committed independently so it can be reverted without affecting other stages.

A feature is not considered verified on hardware solely because host-side tests pass. Hardware-dependent checks must be labeled as unverified until exercised on a Station.
