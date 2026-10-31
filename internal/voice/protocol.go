package voice

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"uuid"
)

const maxMessageBytes = 256 << 10

type event struct {
	ended   bool
	err     error
	kind    string
	status  string
	failure string
}

type sessionSettings struct {
	Voice                 string
	BackendModel          string
	BackendThinkingEffort string
}

func encodeSession(now time.Time, settings sessionSettings) ([]byte, error) {
	_, east := now.Zone()
	zone := now.Location().String()
	if zone == "Local" {
		if tz := os.Getenv("TZ"); tz != "" {
			zone = strings.TrimPrefix(tz, ":")
		}
		if zone == "Local" {
			if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
				if _, name, ok := strings.Cut(target, "/zoneinfo/"); ok {
					zone = name
				}
			}
		}
		if zone == "Local" {
			zone, _ = now.Zone()
		}
	}
	id := strings.ToUpper(uuid.New().String())
	payload := map[string]any{
		"backend_reasoning_effort": "instant", "language_code": "auto",
		"requested_default_model": "", "voice": cmp.Or(settings.Voice, "orbit"), "voice_session_id": id,
		"voice_status_request_id": id, "timezone_offset_min": -east / 60, "timezone": zone,
		"voice_mode": "wingman", "model_slug": "", "model_slug_advanced": "",
		"client_tools": []string{}, "history_and_training_disabled": false,
		"conversation_mode":        map[string]string{"kind": "primary_assistant"},
		"enable_message_streaming": true, "backend_model": cmp.Or(settings.BackendModel, "gpt-6-thinking"),
		"deprecate_celsius": false,
	}
	if effort := cmp.Or(settings.BackendThinkingEffort, "extended"); effort != "disabled" {
		payload["backend_thinking_effort"] = effort
	}
	return json.Marshal(payload)
}

type wireEvent struct {
	Type    string         `json:"type"`
	Data    jsontext.Value `json:"data"`
	Payload jsontext.Value `json:"payload"`
}

func decodeEvent(data []byte) (event, error) {
	if len(data) > maxMessageBytes {
		return event{}, errors.New("voice event exceeds 256 KiB")
	}
	var wire wireEvent
	if json.Unmarshal(data, &wire) != nil {
		return event{}, errors.New("invalid voice event JSON")
	}
	if wire.Type == "data_message" {
		var inner string
		if json.Unmarshal(wire.Data, &inner) != nil {
			return event{}, errors.New("invalid nested voice event JSON")
		}
		data = []byte(inner)
		wire = wireEvent{}
		if json.Unmarshal(data, &wire) != nil {
			return event{}, errors.New("invalid nested voice event JSON")
		}
	}
	if len(wire.Payload) > 0 {
		var nested wireEvent
		if json.Unmarshal(wire.Payload, &nested) == nil && nested.Type != "" {
			data = wire.Payload
			wire = nested
		}
	}
	payload := wire.Payload
	if len(payload) == 0 {
		payload = data
	}
	ev := event{}
	if wire.Type == "response.created" || wire.Type == "response.done" {
		var direct struct { Status string `json:"status"` }
		var wrapped struct { Response struct { Status string `json:"status"` } `json:"response"` }
		_ = json.Unmarshal(payload, &direct)
		_ = json.Unmarshal(payload, &wrapped)
		ev.status = direct.Status
		if ev.status == "" { ev.status = wrapped.Response.Status }
		if wire.Type == "response.done" && strings.EqualFold(ev.status, "failed") {
			ev.failure = "response_failed"
			ev.err = errors.New("ChatGPT response failed")
		}
	}
	switch wire.Type {
	case "input_audio_buffer.speech_started", "input_audio_buffer.speech_stopped",
		"input_audio_buffer.committed", "conversation.item.input_audio_transcription.completed",
		"response.created", "response.output_item.added", "response.content_part.added",
		"response.audio.delta", "response.audio.done", "response.audio_transcript.delta",
		"response.audio_transcript.done", "response.text.delta", "response.text.done",
		"response.done", "rate_limits.updated":
		ev.kind = wire.Type
		return ev, nil
	case "usage_update":
		var p struct {
			Instructions struct {
				HangUp bool `json:"hang_up"`
			} `json:"instructions"`
			Rate struct {
				Exceed   jsontext.Value `json:"exceed_limit_message"`
				Exceeded bool           `json:"exceeded"`
				Reached  bool           `json:"limit_reached"`
			} `json:"rate_limit_message"`
			Audio  *float64 `json:"audio_s"`
			Limits struct {
				Audio struct {
					Remaining *float64 `json:"remaining_seconds"`
				} `json:"audio"`
			} `json:"limits"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return event{}, errors.New("invalid usage event")
		}
		remaining := p.Audio
		if remaining == nil {
			remaining = p.Limits.Audio.Remaining
		}
		exceeded := len(p.Rate.Exceed) > 0 && string(p.Rate.Exceed) != "null" && string(p.Rate.Exceed) != "false" && string(p.Rate.Exceed) != `""`
		if p.Instructions.HangUp {
			ev.kind = wire.Type
			ev.ended = true
			return ev, nil
		}
		if exceeded || p.Rate.Exceeded || p.Rate.Reached || (remaining != nil && *remaining <= 0) {
			ev.kind = wire.Type
			ev.failure = "usage_limit"
			ev.err = errors.New("ChatGPT ended this call because of its usage limit")
			return ev, nil
		}
	case "tool_update":
		var p struct {
			Update string `json:"update_type"`
		}
		if json.Unmarshal(payload, &p) == nil && (strings.EqualFold(p.Update, "hangup") || strings.EqualFold(p.Update, "hang_up")) {
			ev.ended = true
		}
		return ev, nil
	case "error":
		var p struct {
			Error struct { Type string `json:"type"`; Code string `json:"code"` } `json:"error"`
			Code string `json:"code"`
		}
		_ = json.Unmarshal(payload, &p)
		code, kind := strings.ToLower(p.Error.Code), strings.ToLower(p.Error.Type)
		if code == "" { code = strings.ToLower(p.Code) }
		switch {
		case strings.Contains(code, "rate_limit"), strings.Contains(kind, "rate_limit"):
			ev.failure = "rate_limit"
		case strings.Contains(code, "quota"), strings.Contains(kind, "quota"):
			ev.failure = "quota"
		case strings.Contains(code, "auth"), strings.Contains(kind, "auth"), strings.Contains(code, "token"):
			ev.failure = "authentication"
		default:
			ev.failure = "server_error"
		}
		ev.kind = wire.Type
		ev.err = errors.New("ChatGPT reported a voice session error")
		return ev, nil
	}
	if wire.Type != "" { ev.kind = "unrecognized:" + wire.Type }
	return ev, nil
}
