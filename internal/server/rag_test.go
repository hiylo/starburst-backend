package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
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
