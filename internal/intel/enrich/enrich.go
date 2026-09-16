// Package enrich prepares the LLM document-analysis pass that turns raw project
// docs plus endpoint contracts into business summaries and (suggested) gateway
// routes. It is deterministic in what it feeds the model: the gathered docs and
// endpoint list are plain data, and only the descriptive text is model output.
package enrich

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// EndpointHint is the compact endpoint description fed to the model.
type EndpointHint struct {
	Method       string `json:"method"`
	Path         string `json:"path"`
	ResponseType string `json:"responseType"`
}

// EndpointSummary is the model's one-line business summary for an endpoint.
type EndpointSummary struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Summary string `json:"summary"`
}

// RouteHint is a model-suggested gateway route (path patterns → service).
type RouteHint struct {
	Service string   `json:"service"`
	Paths   []string `json:"paths"`
}

// Result is the model output contract.
type Result struct {
	Endpoints []EndpointSummary `json:"endpoints"`
	Routes    []RouteHint       `json:"routes"`
}

// SystemPrompt instructs the model to return only JSON in the Result shape.
func SystemPrompt() string {
	return `你是软件架构分析助手。根据项目文档和接口清单，为每个接口生成一句中文业务描述，
并尽可能推断对外网关路由。只输出 JSON，不要输出任何解释、Markdown 或代码块。
JSON 结构：
{
  "endpoints": [{"method":"GET","path":"/activity/list","summary":"分页查询活动列表"}],
  "routes": [{"service":"activity-provider","paths":["/activity/**"]}]
}
要求：
1. endpoints 与输入接口一一对应，summary 简洁（不超过 30 字），概括业务含义；
2. routes 仅当文档中有依据时才推断，service 用服务名，paths 用网关路径前缀；
3. 不要臆造输入中不存在的接口或路由。`
}

// UserPrompt builds the model input: the gathered docs and the endpoint list.
func UserPrompt(docs string, endpoints []EndpointHint) string {
	var b strings.Builder
	b.WriteString("## 项目文档\n")
	b.WriteString(docs)
	b.WriteString("\n\n## 接口清单\n")
	for _, e := range endpoints {
		b.WriteString("- ")
		b.WriteString(e.Method)
		b.WriteString(" ")
		b.WriteString(e.Path)
		if e.ResponseType != "" {
			b.WriteString(" -> ")
			b.WriteString(e.ResponseType)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n请输出 JSON。")
	return b.String()
}

// Docs gathers documentation text (Markdown files at the root and under docs/)
// up to maxBytes total, so the model prompt stays bounded regardless of repo
// size. Files are visited in sorted order for deterministic prompts.
func Docs(root string, maxBytes int) string {
	const perFile = 8000
	var files []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "target" ||
				name == ".gradle" || name == "build_artifacts" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			rel, _ := filepath.Rel(root, path)
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)

	var b strings.Builder
	remaining := maxBytes
	for _, rel := range files {
		if remaining <= 0 {
			break
		}
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		text := string(data)
		if len(text) > perFile {
			text = text[:perFile]
		}
		if len(text) > remaining {
			text = text[:remaining]
		}
		b.WriteString("### ")
		b.WriteString(rel)
		b.WriteString("\n")
		b.WriteString(text)
		b.WriteString("\n\n")
		remaining -= len(text)
	}
	return strings.TrimSpace(b.String())
}
