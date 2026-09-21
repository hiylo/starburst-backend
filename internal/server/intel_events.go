package server

import (
	"encoding/json"
	"log"

	"github.com/hiylo/starburst-backend/internal/push"
)

// Semantic intel push event types. Declared here so the whole set lives in one
// place; connected clients (web workbench, App) dispatch on the type to render
// the right UI affordance. intel.run.event predates the semantic events and is
// kept for backward compatibility — the semantic types are emitted on top of it.
const (
	intelRunEvent          = "intel.run.event"
	intelGateBlockedEvent  = "intel.gate.blocked"
	intelEnvReadyEvent     = "intel.env.ready"
	intelAuditFindingEvent = "intel.audit.finding"
	intelFixSuggestedEvent = "intel.fix.suggested"
	intelFixAppliedEvent   = "intel.fix.applied"
	intelChatAnswerEvent   = "intel.feature.chat.answer"
)

// pushIntelEvent broadcasts a semantic intel event to all connected clients. It
// is best-effort by design: a marshal failure is logged and the event dropped,
// never fatal to the caller (a broken push must not fail a run/audit/fix).
func (s *Server) pushIntelEvent(typ string, payload map[string]any, severity string) {
	if payload == nil {
		payload = map[string]any{}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		log.Printf("intel push %s marshal: %v", typ, err)
		return
	}
	s.hub.Broadcast(push.Message{Type: typ, Payload: b, Severity: severity})
}
