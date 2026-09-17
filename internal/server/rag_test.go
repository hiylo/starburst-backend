package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiylo/starburst-backend/internal/embed"
	"github.com/hiylo/starburst-backend/internal/llm"
	"github.com/hiylo/starburst-backend/internal/store"
)

func TestSplitDocChunks(t *testing.T) {
	short := splitDocChunks("README.md", "hello world")
	if len(short) != 1 || short[0].Kind != "doc" || short[0].SourceLine != 1 {
		t.Fatalf("short = %+v", short)
	}
	if short[0].SourceFile != "README.md" {
		t.Fatalf("source file = %q", short[0].SourceFile)
	}
	// Long doc must split into multiple chunks on line boundaries.
	var big string
	for i := 0; i < 500; i++ {
		big += "第" + string(rune('a'+i%26)) + "行内容很长很长很长很长很长很长很长\n"
	}
	parts := splitDocChunks("docs/long.md", big)
	if len(parts) < 2 {
		t.Fatalf("long doc should split, got %d chunks", len(parts))
	}
	for i, p := range parts {
		if p.Kind != "doc" || p.SourceFile != "docs/long.md" {
			t.Fatalf("chunk %d = %+v", i, p)
		}
		if p.SourceLine <= 0 {
			t.Fatalf("chunk %d missing line", i)
		}
	}
}

// TestBuildIntelChunksIncludesDocs verifies the knowledge-base builder gathers
// entity, endpoint and Markdown document chunks together.
func TestBuildIntelChunksIncludesDocs(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(root, "README.md"), `# demo
这是一个示例项目，用于验证知识库索引包含文档。`)
	writeTestFile(t, filepath.Join(root, "docs/design.md"), `# 设计
业务说明文档内容。`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserEntity.java"), `package demo;
import javax.persistence.*;
@Entity
@Table(name = "sys_user")
public class UserEntity {
    @Id
    @Column(name = "id", nullable = false)
    private Long id;
}`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	chunks, err := s.buildIntelChunks(context.Background(), proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, c := range chunks {
		kinds[c.Kind]++
	}
	if kinds["entity"] == 0 {
		t.Errorf("no entity chunks, got %v", kinds)
	}
	if kinds["doc"] == 0 {
		t.Errorf("no doc chunks (README/docs should be indexed), got %v", kinds)
	}
	// At least the two documents must be present.
	if kinds["doc"] < 2 {
		t.Errorf("expected >=2 doc chunks, got %d", kinds["doc"])
	}
}

// TestBuildIntelChunksOverview verifies a project with a free-text description
// yields a single "overview" chunk that carries the description plus the
// module/service inventory, so overview questions have a searchable target.
func TestBuildIntelChunksOverview(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(root, "src/main/java/pay/Order.java"), `package pay;
import javax.persistence.*;
@Entity
@Table(name = "t_order")
public class Order {
    @Id
    @Column(name = "id", nullable = false)
    private Long id;
}`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"pay","source":"local","localPath":"`+filepath.ToSlash(root)+`","description":"支付网关项目，负责订单支付与退款。"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	chunks, err := s.buildIntelChunks(context.Background(), proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	var overview *store.RagChunk
	for _, c := range chunks {
		if c.Kind == "overview" {
			overview = c
			break
		}
	}
	if overview == nil {
		kinds := map[string]int{}
		for _, c := range chunks {
			kinds[c.Kind]++
		}
		t.Fatalf("no overview chunk, got kinds=%v", kinds)
	}
	if !strings.Contains(overview.Content, "支付网关") {
		t.Errorf("overview missing description: %q", overview.Content)
	}
	if !strings.Contains(overview.Content, "模块/服务清单") {
		t.Errorf("overview missing module inventory: %q", overview.Content)
	}
}

// TestAskIntelProjectOverviewInjection verifies the project profile is injected
// as a first-class source when the description exists but the overview chunk is
// not among the retrieved fragments, and that the injection is skipped (dedup)
// when the overview chunk itself is retrieved.
func TestAskIntelProjectOverviewInjection(t *testing.T) {
	s := newTestServer(t)

	// 固定 1024 维向量的假嵌入服务（问题与 chunk 都返回同一向量，相似度恒为 1）。
	embSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vec := make([]float32, store.EmbedDim)
		for i := range vec {
			vec[i] = 0.01
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": vec}},
		})
	}))
	t.Cleanup(embSrv.Close)
	s.SetEmbedding(embed.New(embSrv.URL, "", "test-model"))

	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"这是一个支付网关项目。"}}]}`))
	}))
	t.Cleanup(llmSrv.Close)
	s.SetLLM(llm.New(llmSrv.URL, "test-key", "test-model"))

	ctx := context.Background()
	p := &store.IntelProject{Name: "pay", Source: "local", Description: "支付网关项目，负责订单支付与退款。"}
	if err := s.store.CreateIntelProject(ctx, p); err != nil {
		t.Fatal(err)
	}

	vec := func() []float32 {
		v := make([]float32, store.EmbedDim)
		for i := range v {
			v[i] = 0.01
		}
		return v
	}

	t.Run("inject when overview chunk absent", func(t *testing.T) {
		if err := s.store.ReplaceProjectChunks(ctx, p.ID, []*store.RagChunk{
			{Kind: "entity", Title: "t_order 表", Content: "数据表 t_order 字段：id", SourceFile: "Order.java", SourceLine: 1, Embedding: vec()},
		}); err != nil {
			t.Fatal(err)
		}
		resp, err := s.askIntelProject(ctx, p.ID, "这个项目是做什么的", 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if resp["answer"] == "" {
			t.Fatal("expected non-empty answer")
		}
		sources := resp["sources"].([]map[string]any)
		if len(sources) != 2 {
			t.Fatalf("expected 2 sources (画像 + entity), got %d", len(sources))
		}
		if sources[0]["sourceFile"] != "项目画像" {
			t.Fatalf("expected 项目画像 first, got %v", sources[0]["sourceFile"])
		}
	})

	t.Run("dedup when overview chunk retrieved", func(t *testing.T) {
		if err := s.store.ReplaceProjectChunks(ctx, p.ID, []*store.RagChunk{
			{Kind: "overview", Title: "项目概览 pay", Content: "项目 pay 概览", SourceFile: "项目画像", Embedding: vec()},
		}); err != nil {
			t.Fatal(err)
		}
		resp, err := s.askIntelProject(ctx, p.ID, "这个项目有哪些模块", 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		sources := resp["sources"].([]map[string]any)
		if len(sources) != 1 {
			t.Fatalf("expected 1 source (overview chunk only, no duplicate 画像), got %d", len(sources))
		}
		if sources[0]["kind"] != "overview" {
			t.Fatalf("expected overview chunk, got kind=%v", sources[0]["kind"])
		}
	})
}
