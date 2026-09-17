package intel

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestInnermostType(t *testing.T) {
	cases := []struct{ in, want string }{
		{"OperationResponse<FriendPageDto>", "FriendPageDto"},
		{"List<FriendPageDto>", "FriendPageDto"},
		{"FriendPageDto", "FriendPageDto"},
		{"Response<Page<List<UserDto>>>", "UserDto"},
		{"", ""},
	}
	for _, c := range cases {
		if got := innermostType(c.in); got != c.want {
			t.Errorf("innermostType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJavaToJSONType(t *testing.T) {
	cases := []struct{ in, want string }{
		{"String", "string"},
		{"LocalDateTime", "string"},
		{"BigDecimal", "string"},
		{"int", "number"},
		{"Integer", "number"},
		{"boolean", "boolean"},
		{"Boolean", "boolean"},
		{"List<String>", "array"},
		{"String[]", "array"},
		{"Map<String,Object>", "object"},
		{"FriendPageDto", "object"},
	}
	for _, c := range cases {
		if got := javaToJSONType(c.in); got != c.want {
			t.Errorf("javaToJSONType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExtractDtoFields(t *testing.T) {
	lines := []string{
		"public class ActivityDto {",
		"    private Long id;",
		"    private String title;",
		"    private Boolean active;",
		"    private List<String> tags;",
		"    private LocalDateTime createdAt;",
		"}",
	}
	fields := extractDtoFields(lines)
	if len(fields) != 5 {
		t.Fatalf("extractDtoFields returned %d fields, want 5: %+v", len(fields), fields)
	}
	want := map[string]string{
		"id": "number", "title": "string", "active": "boolean",
		"tags": "array", "createdAt": "string",
	}
	for _, f := range fields {
		if want[f.Name] != f.Type {
			t.Errorf("field %q has type %q, want %q", f.Name, f.Type, want[f.Name])
		}
	}
}

func TestResolveResponseFields(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/main/java/com/echo/ctrl/ActivityController.java": `package com.echo.ctrl;
import com.echo.dto.ActivityDto;
@RestController
public class ActivityController {
    @GetMapping("/activity")
    public ActivityDto getActivity() { return null; }
}`,
		"src/main/java/com/echo/dto/ActivityDto.java": `package com.echo.dto;
public class ActivityDto {
    private Long id;
    private String title;
    private Boolean active;
}`,
	})
	idx := map[string]string{
		"com.echo.dto.ActivityDto": filepath.Join(root, "src/main/java/com/echo/dto/ActivityDto.java"),
	}
	ctrlLines := []string{
		"package com.echo.ctrl;",
		"import com.echo.dto.ActivityDto;",
		"public ActivityDto getActivity() { return null; }",
	}
	raw := resolveResponseFields("ActivityDto", ctrlLines, idx)
	if raw == "" {
		t.Fatal("resolveResponseFields returned empty")
	}
	var fields []fieldSpec
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if len(fields) != 3 {
		t.Fatalf("got %d fields, want 3: %+v", len(fields), fields)
	}
}

func TestResolveResponseFieldsPrimitiveOrMissing(t *testing.T) {
	idx := map[string]string{}
	for _, in := range []string{"String", "void", "List<String>", "long"} {
		if got := resolveResponseFields(in, nil, idx); got != "" {
			t.Errorf("resolveResponseFields(%q) = %q, want empty", in, got)
		}
	}
	if got := resolveResponseFields("UnknownDto", nil, map[string]string{"x": "y"}); got != "" {
		t.Errorf("resolveResponseFields(UnknownDto) = %q, want empty", got)
	}
}

func TestResolveResponseFieldsFullyQualified(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/main/java/com/echo/dto/ActivityDto.java": `package com.echo.dto;
public class ActivityDto {
    private Long id;
    private String title;
}`,
	})
	idx := map[string]string{
		"com.echo.dto.ActivityDto": filepath.Join(root, "src/main/java/com/echo/dto/ActivityDto.java"),
	}
	lines := []string{"package com.echo.ctrl;"}
	raw := resolveResponseFields("com.echo.dto.ActivityDto", lines, idx)
	if raw == "" {
		t.Fatal("resolveResponseFields returned empty for fully-qualified return type")
	}
	var fields []fieldSpec
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("got %d fields, want 2: %+v", len(fields), fields)
	}
}
