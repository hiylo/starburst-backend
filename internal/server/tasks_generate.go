package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hiylo/startburst-backend/internal/llm"
)

// taskStepDraft is one LLM-proposed step of a structured task plan.
type taskStepDraft struct {
	Name      string `json:"name"`
	Prompt    string `json:"prompt"`
	Directory string `json:"directory"`
}

// taskScheduleDraft is the LLM-detected scheduling intent for a plan.
// Type is one of immediate / delay / at / cron.
type taskScheduleDraft struct {
	Type    string `json:"type"`
	Minutes int    `json:"minutes"`
	Cron    string `json:"cron"`
	At      string `json:"at"`
}

// taskPlanDraft is an LLM-proposed structured task plan returned to the client
// for review before it is submitted as concrete tasks via POST /api/tasks.
type taskPlanDraft struct {
	Name      string             `json:"name"`
	Directory string             `json:"directory"`
	Steps     []taskStepDraft    `json:"steps"`
	Schedule  *taskScheduleDraft `json:"schedule"`
}

// generateTaskSystem instructs the model to emit a single rich JSON plan object.
const generateTaskSystem = `你是一个任务编排器。根据用户对任务的自然语言描述，生成一个结构化的任务计划草案。

要求：
- 把用户模糊的一句话拆解成清晰的执行步骤（1 到 N 步），每步明确可执行；简单任务只返回一步。
- 为整个计划起一个简短的中文名称 name。
- 为计划设定默认工作目录 directory（用户未指定则为空字符串 ""）。
- 每个 step 包含三个字段：name（步骤简短名称）、prompt（交给 agent 的明确可执行指令）、directory（该步骤工作目录，未指定为 ""）。
- 识别用户的调度意图，输出 schedule：
  * 立即执行 → {"type":"immediate","minutes":0,"cron":"","at":""}
  * 延时执行（如"30分钟后"、"1小时后"）→ {"type":"delay","minutes":30,"cron":"","at":""}
  * 周期执行（如"每天早上8点"、"每小时"）→ {"type":"cron","minutes":0,"cron":"0 0 8 * * *","at":""}（6 字段秒级：秒 分 时 日 月 周）
  * 指定时间（如"今晚8点"；若无法给出绝对日期，转成延时分钟数用 delay）→ {"type":"at","minutes":0,"cron":"","at":"YYYY-MM-DD HH:MM"}
  无法识别调度则 {"type":"immediate","minutes":0,"cron":"","at":""}。
- 不要编造用户没提到的目录或细节。

只输出一个 JSON 对象，结构：
{"name":"...","directory":"...","steps":[{"name":"...","prompt":"...","directory":"..."}],"schedule":{"type":"immediate","minutes":0,"cron":"","at":""}}
不要输出任何解释、注释或 markdown 代码块。`

// refineTaskSystem instructs the model to revise an existing draft per a
// follow-up instruction, keeping the same JSON structure. It has a single %s
// placeholder for the existing draft JSON.
const refineTaskSystem = `你是一个任务编排器。用户之前生成了一个任务计划草案，现在给出了修改意见。请根据修改意见修改现有草案，返回修改后的完整草案。

现有草案（JSON）：
%s

保持输出 JSON 结构与现有草案一致（name / directory / steps[] / schedule），只修改用户要求的部分，其余字段保持不变。

只输出一个 JSON 对象，不要输出任何解释、注释或 markdown 代码块。`

// validateTaskDraft rejects obviously unusable drafts.
func validateTaskDraft(d *taskPlanDraft) error {
	if d == nil {
		return fmt.Errorf("empty draft")
	}
	if len(d.Steps) == 0 {
		return fmt.Errorf("no steps")
	}
	for _, st := range d.Steps {
		if st.Prompt == "" {
			return fmt.Errorf("empty step prompt")
		}
	}
	if d.Schedule != nil {
		switch d.Schedule.Type {
		case "", "immediate", "delay", "at", "cron":
		default:
			return fmt.Errorf("invalid schedule type %q", d.Schedule.Type)
		}
	}
	return nil
}

// handleTaskGenerate converts a natural-language task description into a
// structured plan draft using the orchestration LLM. It supports two modes:
//   - fresh: {description} generates a new draft;
//   - refine: {draft, instruction} revises an existing draft.
//
// The result is NOT persisted; the client must confirm it via POST /api/tasks.
// With ?stream=1 the endpoint streams the model output as SSE and emits a final
// "draft" event.
func (s *Server) handleTaskGenerate(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.requireToken(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.llm == nil || !s.llm.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "orchestration LLM is not configured")
		return
	}
	// Rate-limit per token so a runaway client cannot burn unbounded LLM cost.
	if !s.genLimit.allow(tok.ID) {
		writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	var req struct {
		Description string         `json:"description"`
		Draft       *taskPlanDraft `json:"draft"`
		Instruction string         `json:"instruction"`
	}
	if !readBody(w, r, &req) {
		return
	}

	var system, user string
	switch {
	case req.Draft == nil && req.Description != "":
		system = generateTaskSystem
		user = req.Description
	case req.Draft != nil && req.Instruction != "":
		db, err := json.Marshal(req.Draft)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid draft")
			return
		}
		system = fmt.Sprintf(refineTaskSystem, string(db))
		user = req.Instruction
	default:
		writeErr(w, http.StatusBadRequest, "provide description (fresh) or draft+instruction (refine)")
		return
	}

	if r.URL.Query().Get("stream") == "1" {
		s.streamTaskGenerate(w, r, system, user)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	var draft taskPlanDraft
	if err := s.llm.CompleteJSON(ctx, system, user, &draft); err != nil {
		writeErr(w, http.StatusBadGateway, "generate plan failed: "+err.Error())
		return
	}
	if err := validateTaskDraft(&draft); err != nil {
		writeErr(w, http.StatusBadGateway, "model returned invalid draft: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"draft": draft})
}

// streamTaskGenerate streams the LLM output as SSE: repeated "delta" events
// carry raw text, then a final "draft" event carries the parsed plan, or an
// "error" event on failure.
func (s *Server) streamTaskGenerate(w http.ResponseWriter, r *http.Request, system, user string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	var sb strings.Builder
	text, err := s.llm.CompleteJSONStream(ctx, system, user, func(delta string) error {
		sb.WriteString(delta)
		return sseWrite(w, flusher, map[string]any{"type": "delta", "text": delta})
	})
	if err != nil {
		_ = sseWrite(w, flusher, map[string]any{"type": "error", "message": err.Error()})
		return
	}

	var draft taskPlanDraft
	if err := llm.DecodeJSON(text, &draft); err != nil {
		_ = sseWrite(w, flusher, map[string]any{"type": "error", "message": "解析失败: " + err.Error()})
		return
	}
	if err := validateTaskDraft(&draft); err != nil {
		_ = sseWrite(w, flusher, map[string]any{"type": "error", "message": "解析失败: " + err.Error()})
		return
	}
	_ = sseWrite(w, flusher, map[string]any{"type": "draft", "draft": draft})
}

// sseWrite marshals payload into one SSE "data:" event and flushes it.
func sseWrite(w http.ResponseWriter, flusher http.Flusher, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
