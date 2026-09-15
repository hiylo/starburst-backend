package server

import (
	"testing"
	"time"

	"github.com/hiylo/starburst-backend/internal/opencode"
)

func TestParseSessionEventV1Properties(t *testing.T) {
	ev := opencode.SSEEvent{Data: []byte(`{"type":"session.created","properties":{"sessionID":"ses_1","version":1}}`)}
	se, ok := parseSessionEvent(ev)
	if !ok {
		t.Fatal("parse failed")
	}
	if se.SessionID != "ses_1" || se.EventType != "session.created" {
		t.Fatalf("got session=%q type=%q", se.SessionID, se.EventType)
	}
	if string(se.Payload) != string(ev.Data) {
		t.Fatal("payload must keep the raw event JSON")
	}
	if se.CreatedAt.IsZero() || time.Since(se.CreatedAt) > time.Minute {
		t.Fatalf("createdAt not set to now: %v", se.CreatedAt)
	}
}

func TestParseSessionEventV2DataAndCamelCase(t *testing.T) {
	ev := opencode.SSEEvent{Data: []byte(`{"type":"next.text.delta","data":{"sessionId":"ses_2"}}`)}
	se, ok := parseSessionEvent(ev)
	if !ok {
		t.Fatal("parse failed")
	}
	if se.SessionID != "ses_2" || se.EventType != "next.text.delta" {
		t.Fatalf("got session=%q type=%q", se.SessionID, se.EventType)
	}
}

// TestParseSessionEventV18Wrapped regresses the real opencode (>=1.18) global
// event payload, where the event is wrapped in a top-level "payload" object:
// {"payload":{"type":"message.part.delta","properties":{"sessionID":"ses_.."}},
//  "project":"..","directory":".."}. The host name stays in the raw Data.
func TestParseSessionEventV18Wrapped(t *testing.T) {
	ev := opencode.SSEEvent{Data: []byte(`{"directory":"/w","project":"p","payload":{"id":"evt_x","type":"message.part.delta","properties":{"sessionID":"ses_wrapped","delta":"a"}}}`)}
	se, ok := parseSessionEvent(ev)
	if !ok {
		t.Fatal("parse failed")
	}
	if se.SessionID != "ses_wrapped" || se.EventType != "message.part.delta" {
		t.Fatalf("got session=%q type=%q", se.SessionID, se.EventType)
	}
	if string(se.Payload) != string(ev.Data) {
		t.Fatal("payload must keep the raw event JSON")
	}
}

func TestParseSessionEventFallbacks(t *testing.T) {
	// No type -> unknown; no session -> empty.
	se, ok := parseSessionEvent(opencode.SSEEvent{Data: []byte(`{"properties":{}}`)})
	if !ok {
		t.Fatal("parse failed")
	}
	if se.EventType != "unknown" || se.SessionID != "" {
		t.Fatalf("got type=%q session=%q", se.EventType, se.SessionID)
	}

	// Top-level sessionID wins.
	se, _ = parseSessionEvent(opencode.SSEEvent{Data: []byte(`{"type":"x","sessionID":"ses_3"}`)})
	if se.SessionID != "ses_3" {
		t.Fatalf("top-level sessionID not read: %q", se.SessionID)
	}

	// Not JSON at all -> rejected.
	if _, ok := parseSessionEvent(opencode.SSEEvent{Data: []byte("not json")}); ok {
		t.Fatal("expected reject on invalid JSON")
	}
}

func TestIsHighFrequencyEvent(t *testing.T) {
	for _, hf := range []string{"message.part.updated", "next.text.delta", "next.tool.input.delta", "next.tool.progress", "heartbeat"} {
		if !isHighFrequencyEvent(hf) {
			t.Fatalf("%q should be high frequency", hf)
		}
	}
	for _, lf := range []string{"session.created", "session.idle", "next.tool.success", "message.updated"} {
		if isHighFrequencyEvent(lf) {
			t.Fatalf("%q should NOT be high frequency", lf)
		}
	}
}

func TestParseEventSince(t *testing.T) {
	if _, ok := parseEventSince(""); ok {
		t.Fatal("empty since should fail")
	}
	rfc, ok := parseEventSince("2026-09-14T10:00:00+08:00")
	if !ok {
		t.Fatal("RFC3339 rejected")
	}
	if rfc.UTC().Hour() != 2 {
		t.Fatalf("RFC3339 tz not normalized to UTC: %v", rfc)
	}
	ms, ok := parseEventSince("1750000000000")
	if !ok {
		t.Fatal("unix ms rejected")
	}
	if ms.UnixMilli() != 1750000000000 {
		t.Fatalf("unix ms parsed wrong: %v", ms)
	}
	if _, ok := parseEventSince("bogus"); ok {
		t.Fatal("garbage accepted")
	}
}
