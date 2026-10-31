package voice

import (
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testEnvelope(t *testing.T, inner string) []byte {
	t.Helper()
	data, err := json.Marshal(struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}{"data_message", inner})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWebsiteProtocol(t *testing.T) {
	session, err := encodeSession(time.Date(2026, 10, 22, 12, 0, 0, 0, time.FixedZone("Asia/Kolkata", 19800)), sessionSettings{})
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err = json.Unmarshal(session, &values); err != nil {
		t.Fatal(err)
	}
	if values["timezone_offset_min"] != float64(-330) || values["timezone"] != "Asia/Kolkata" {
		t.Fatal("wrong local timezone", values)
	}
	id := values["voice_session_id"].(string)
	if id != strings.ToUpper(id) || len(id) != 36 || values["voice_status_request_id"] != id {
		t.Fatal("session IDs do not match")
	}
	for key, want := range map[string]any{"voice": "orbit", "voice_mode": "wingman", "backend_model": "gpt-6-thinking", "backend_thinking_effort": "extended", "deprecate_celsius": false, "requested_default_model": "", "model_slug": "", "model_slug_advanced": "", "enable_message_streaming": true} {
		if values[key] != want {
			t.Fatalf("%s=%v", key, values[key])
		}
	}
}

func TestDisplayEventsIgnored(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		kind string
	}{
		{`{"type":"chat_message_delta","delta":{"v":{"message":{"content":{"parts":[{"text":"private words"}]}}}}}`, "unrecognized:chat_message_delta"},
		{`{"type":"state_update","payload":{"new_state":"speaking"}}`, "unrecognized:state_update"},
		{`{"type":"state_update","payload":{"new_state":{"future":true}}}`, "unrecognized:state_update"},
	} {
		ev, err := decodeEvent(testEnvelope(t, tc.raw))
		if err != nil || ev.kind != tc.kind || ev.ended || ev.err != nil || ev.status != "" || ev.failure != "" {
			t.Fatalf("display event was not safely classified: %+v %v", ev, err)
		}
	}
	if _, err := decodeEvent(make([]byte, maxMessageBytes+1)); err == nil {
		t.Fatal("unbounded event accepted")
	}
}

func TestUnknownEventDataShapes(t *testing.T) {
	for _, data := range []string{`{"extra":true}`, `[1,"extra"]`, `"extra"`, `42`, `true`, `null`} {
		t.Run(data, func(t *testing.T) {
			raw := `{"type":"future_event","data":` + data + `}`
			for _, input := range [][]byte{[]byte(raw), testEnvelope(t, raw), testEnvelope(t, `{"payload":`+raw+`}`)} {
				ev, err := decodeEvent(input)
				if err != nil || ev.kind != "unrecognized:future_event" || ev.ended || ev.err != nil || ev.status != "" || ev.failure != "" {
					t.Fatalf("unknown event was not safely classified: %+v, %v", ev, err)
				}
			}
		})
	}
}

func TestEventDiagnosticSummaryOnlyIncludesSafeMetadata(t *testing.T) {
	raw := testEnvelope(t, `{"type":"error","error":{"code":"rate_limit_exceeded","type":"server_error","message":"private conversation text"}}`)
	summary := eventDiagnosticSummary(raw)
	for _, want := range []string{"type=error", "error_type=server_error", "error_code=rate_limit_exceeded"} {
		if !strings.Contains(summary, want) { t.Fatalf("summary %q missing %q", summary, want) }
	}
	if strings.Contains(summary, "private") || strings.Contains(summary, "conversation") {
		t.Fatalf("summary leaked payload text: %q", summary)
	}
	state := eventDiagnosticSummary(testEnvelope(t, `{"type":"state_update","payload":{"new_state":"speaking"}}`))
	if !strings.Contains(state, "type=state_update") { t.Fatalf("state event missing: %q", state) }
}

func TestMalformedDataMessage(t *testing.T) {
	for _, raw := range []string{
		`{"type":"data_message"}`,
		`{"type":"data_message","data":null}`,
		`{"type":"data_message","data":{"extra":true}}`,
		`{"type":"data_message","data":[1,"extra"]}`,
		`{"type":"data_message","data":42}`,
		`{"type":"data_message","data":true}`,
		`{"type":"data_message","data":""}`,
		`{"type":"data_message","data":"invalid JSON"}`,
	} {
		if _, err := decodeEvent([]byte(raw)); err == nil {
			t.Errorf("malformed data_message accepted: %s", raw)
		}
	}
}

func TestRemoteEnd(t *testing.T) {
	for _, raw := range []string{
		`{"type":"usage_update","audio_s":0}`,
		`{"type":"usage_update","audio_s":-1}`,
		`{"type":"usage_update","limits":{"audio":{"remaining_seconds":0}}}`,
		`{"type":"usage_update","limits":{"audio":{"remaining_seconds":-1}}}`,
		`{"type":"usage_update","rate_limit_message":{"limit_reached":true}}`,
		`{"type":"usage_update","rate_limit_message":{"exceed_limit_message":"limit exceeded"}}`,
		`{"type":"usage_update","rate_limit_message":{"exceeded":true}}`,
		`{"type":"error","payload":{"message":"private server details"}}`,
	} {
		ev, err := decodeEvent(testEnvelope(t, raw))
		if err != nil || ev.err == nil {
			t.Fatalf("end event missed: %s %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"type":"tool_update","payload":{"update_type":"hang_up"}}`,
		`{"type":"usage_update","payload":{"instructions":{"hang_up":true}}}`,
	} {
		ev, err := decodeEvent(testEnvelope(t, raw))
		if err != nil || !ev.ended || ev.err != nil {
			t.Fatal("remote hangup was not normal completion", ev, err)
		}
	}
}

func TestRealtimeLifecycleEventsAreClassified(t *testing.T) {
	cases := []struct { raw, kind, status string }{
		{`{"type":"input_audio_buffer.speech_started"}`, "input_audio_buffer.speech_started", ""},
		{`{"type":"conversation.item.input_audio_transcription.completed"}`, "conversation.item.input_audio_transcription.completed", ""},
		{`{"type":"response.created","response":{"status":"in_progress"}}`, "response.created", "in_progress"},
		{`{"type":"response.audio.delta","delta":"opaque"}`, "response.audio.delta", ""},
		{`{"type":"response.done","response":{"status":"completed"}}`, "response.done", "completed"},
		{`{"type":"response.done","response":{"status":"failed"}}`, "response.done", "failed"},
	}
	for _, tc := range cases {
		ev, err := decodeEvent(testEnvelope(t, tc.raw))
		if err != nil || ev.kind != tc.kind || ev.status != tc.status {
			t.Fatalf("event %s classified as %+v, err=%v", tc.raw, ev, err)
		}
		if tc.status == "failed" && (ev.err == nil || ev.failure != "response_failed") {
			t.Fatalf("failed response not marked: %+v", ev)
		}
		if tc.status != "failed" && ev.err != nil {
			t.Fatalf("successful event became error: %+v", ev)
		}
	}
}

func TestRealtimeServerErrorsAreSanitizedAndClassified(t *testing.T) {
	for _, tc := range []struct { raw, category string }{
		{`{"type":"error","error":{"code":"rate_limit_exceeded","message":"secret"}}`, "rate_limit"},
		{`{"type":"error","error":{"type":"quota_error","message":"secret"}}`, "quota"},
		{`{"type":"error","error":{"code":"invalid_authentication","message":"secret"}}`, "authentication"},
		{`{"type":"error","message":"secret"}`, "server_error"},
	} {
		ev, err := decodeEvent(testEnvelope(t, tc.raw))
		if err != nil || ev.failure != tc.category || ev.err == nil || strings.Contains(ev.err.Error(), "secret") {
			t.Fatalf("unsafe or incorrect error classification: %+v %v", ev, err)
		}
	}
}

func TestPositiveRemainingQuotaIsNotTerminal(t *testing.T) {
	for _, raw := range []string{
		`{"type":"usage_update","audio_s":1}`,
		`{"type":"usage_update","audio_s":5}`,
		`{"type":"usage_update","limits":{"audio":{"remaining_seconds":1}}}`,
		`{"type":"usage_update","limits":{"audio":{"remaining_seconds":5}}}`,
	} {
		ev, err := decodeEvent(testEnvelope(t, raw))
		if err != nil || ev.kind != "unrecognized:usage_update" || ev.ended || ev.err != nil || ev.status != "" || ev.failure != "" {
			t.Errorf("positive remaining quota was not safely classified: %s %+v %v", raw, ev, err)
		}
	}
}

func TestRunSettingsPayload(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		args                 []string
		voice, model, effort string
		omit                 bool
	}{
		{"missing", nil, "orbit", "gpt-6-thinking", "extended", false},
		{"empty", []string{"--voice=", "--backend-model=", "--backend-thinking-effort="}, "orbit", "gpt-6-thinking", "extended", false},
		{"empty voice", []string{"--voice=", "--backend-model=future", "--backend-thinking-effort=other"}, "orbit", "future", "other", false},
		{"empty model", []string{"--voice=future", "--backend-model=", "--backend-thinking-effort=other"}, "future", "gpt-6-thinking", "other", false},
		{"empty effort", []string{"--voice=future", "--backend-model=other", "--backend-thinking-effort="}, "future", "other", "extended", false},
		{"whitespace", []string{"--voice= \t ", "--backend-model=\n", "--backend-thinking-effort= "}, " \t ", "\n", " ", false},
		{"literal", []string{"--voice=  голос\t\n$'\"`;&|<>#", "--backend-model=-future/model", "--backend-thinking-effort=任意"}, "  голос\t\n$'\"`;&|<>#", "-future/model", "任意", false},
		{"disabled", []string{"--voice=disabled", "--backend-model=disabled", "--backend-thinking-effort=disabled"}, "disabled", "disabled", "", true},
		{"case", []string{"--backend-thinking-effort=Disabled"}, "orbit", "gpt-6-thinking", "Disabled", false},
		{"leading space", []string{"--backend-thinking-effort= disabled"}, "orbit", "gpt-6-thinking", " disabled", false},
		{"trailing space", []string{"--backend-thinking-effort=disabled "}, "orbit", "gpt-6-thinking", "disabled ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, err := parseRunSettings(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			before := settings
			data, err := encodeSession(time.Date(2026, 10, 22, 12, 0, 0, 0, time.FixedZone("Asia/Kolkata", 19800)), settings)
			if err != nil {
				t.Fatal(err)
			}
			if settings != before {
				t.Fatal("encoding changed the startup snapshot")
			}
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			id, ok := got["voice_session_id"].(string)
			if !ok || len(id) != 36 || strings.ToUpper(id) != id {
				t.Fatal("invalid session ID")
			}
			want := map[string]any{
				"backend_reasoning_effort": "instant", "language_code": "auto",
				"requested_default_model": "", "voice": tc.voice, "voice_session_id": id,
				"voice_status_request_id": id, "timezone_offset_min": float64(-330), "timezone": "Asia/Kolkata",
				"voice_mode": "wingman", "model_slug": "", "model_slug_advanced": "",
				"client_tools": []any{}, "history_and_training_disabled": false,
				"conversation_mode":        map[string]any{"kind": "primary_assistant"},
				"enable_message_streaming": true, "backend_model": tc.model, "deprecate_celsius": false,
			}
			if !tc.omit {
				want["backend_thinking_effort"] = tc.effort
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("payload got %#v, want %#v", got, want)
			}
		})
	}
}
