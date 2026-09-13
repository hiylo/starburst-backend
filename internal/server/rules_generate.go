package server

import (
	"context"
	"net/http"
	"time"
)

// ruleDraft is an LLM-proposed automation rule, returned to the client for
// review before it is persisted via POST /api/rules.
type ruleDraft struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Schedule  string `json:"schedule"`
	Directory string `json:"directory"`
	Prompt    string `json:"prompt"`
	Enabled   bool   `json:"enabled"`
}

// generateRuleSystem is the system prompt instructing the model to emit a
// single JSON rule object with no surrounding prose.
const generateRuleSystem = `你是一个自动化规则生成器。根据用户用自然语言描述的自动化意图，生成一条自动化规则。

字段说明：
- name: 简短的中文规则名称
- kind: 触发类型，只能是 "cron"（定时）、"git"（git 仓库变更）、"http"（webhook 触发）之一
- schedule:
  * cron 类型为 6 字段秒级 cron 表达式 "秒 分 时 日 月 周"，例如每天 8 点触发 = "0 0 8 * * *"，每 30 分钟 = "0 */30 * * * *"
  * git 类型为要监听的仓库目录路径
  * http 类型为 webhook 的目标路径
- directory: 任务执行时的工作目录（可为空字符串）
- prompt: 触发后交给 agent 执行的明确指令，要具体可执行
- enabled: true

只输出一个 JSON 对象，不要输出任何解释、注释或 markdown 代码块。`

// handleRuleGenerate converts a natural-language automation description into
// a draft rule using the orchestration LLM. The result is NOT persisted; the
// client must confirm it via POST /api/rules.
func (s *Server) handleRuleGenerate(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
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

	var draft ruleDraft
	if err := s.llm.CompleteJSON(ctx, generateRuleSystem, req.Description, &draft); err != nil {
		writeErr(w, http.StatusBadGateway, "generate rule failed: "+err.Error())
		return
	}

	// Validate the model's output; reject obviously unusable drafts.
	switch draft.Kind {
	case "cron", "git", "http":
	default:
		writeErr(w, http.StatusBadGateway, "model returned invalid kind")
		return
	}
	if draft.Prompt == "" {
		writeErr(w, http.StatusBadGateway, "model returned empty prompt")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"draft": draft})
}
