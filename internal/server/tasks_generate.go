package server

import (
	"context"
	"net/http"
	"time"
)

// taskStepDraft is one LLM-proposed step of a structured task plan.
type taskStepDraft struct {
	Name      string `json:"name"`
	Prompt    string `json:"prompt"`
	Directory string `json:"directory"`
}

// taskPlanDraft is an LLM-proposed structured task plan returned to the client
// for review before it is submitted as concrete tasks via POST /api/tasks.
type taskPlanDraft struct {
	Name  string          `json:"name"`
	Steps []taskStepDraft `json:"steps"`
}

// generateTaskSystem is the system prompt instructing the model to emit a
// single JSON plan object with no surrounding prose.
const generateTaskSystem = `你是一个任务编排器。根据用户对任务的自然语言描述，生成一个结构化的任务计划草稿。

要求：
- 把用户模糊的一句话拆解成清晰的执行步骤（1 到 N 步），每步是一条明确、可执行的指令。
- 如果任务简单到一步即可完成，只返回一个步骤。
- 为整个计划起一个简短的中文名称 name。
- 每个 step 包含三个字段：
  * name: 该步骤的简短名称
  * prompt: 交给 agent 执行的明确、可执行的指令
  * directory: 该步骤的工作目录，用户未指定时为空字符串 ""
- 不要编造用户没提到的目录或细节。

只输出一个 JSON 对象，格式：
{"name":"...","steps":[{"name":"...","prompt":"...","directory":"..."}]}
不要输出任何解释、注释或 markdown 代码块。`

// handleTaskGenerate converts a natural-language task description into a
// structured plan draft using the orchestration LLM. The result is NOT
// persisted; the client must confirm it via POST /api/tasks.
func (s *Server) handleTaskGenerate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireToken(r); !ok {
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
	var req struct {
		Description string `json:"description"`
	}
	if err := readJSON(r, &req); err != nil || req.Description == "" {
		writeErr(w, http.StatusBadRequest, "description is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	var draft taskPlanDraft
	if err := s.llm.CompleteJSON(ctx, generateTaskSystem, req.Description, &draft); err != nil {
		writeErr(w, http.StatusBadGateway, "generate plan failed: "+err.Error())
		return
	}
	if len(draft.Steps) == 0 {
		writeErr(w, http.StatusBadGateway, "model returned no steps")
		return
	}
	for _, st := range draft.Steps {
		if st.Prompt == "" {
			writeErr(w, http.StatusBadGateway, "model returned an empty step prompt")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"draft": draft})
}
