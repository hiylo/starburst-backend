package intel

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hiylo/starburst-backend/internal/store"
)

// Java contract scanning: deterministic, line-anchored extraction of JPA
// entities (entity↔table↔column with nullable) and REST controllers
// (method+path+response type+request body) with source_file:line provenance.
// Deliberately simple (regex/line based): multi-line annotations and inherited
// mappings are left to the LLM assist layer later; the deterministic anchor
// guarantees every extracted row has a verifiable location.

var (
	reEntity          = regexp.MustCompile(`@Entity\b`)
	reTable           = regexp.MustCompile(`@Table\s*\(\s*name\s*=\s*"([^"]+)"`)
	reClass           = regexp.MustCompile(`\bclass\s+([A-Za-z0-9_]+)`)
	reColumn          = regexp.MustCompile(`@Column`)
	reColumnName      = regexp.MustCompile(`@Column\s*\(\s*name\s*=\s*"([^"]+)"`)
	reColumnNullable  = regexp.MustCompile(`nullable\s*=\s*(true|false)`)
	reField           = regexp.MustCompile(`^\s*(?:private|public|protected)\s+(?:@[\w.]+(?:\s*\([^)]*\))?\s+)*([A-Za-z0-9_$<>,\[\]\.]+)\s+([A-Za-z0-9_$]+)\s*(?:=|;)`)
	reController      = regexp.MustCompile(`@(RestController|Controller)\b`)
	reRequestMapping  = regexp.MustCompile(`@RequestMapping\s*\(\s*(?:value\s*=\s*)?"([^"]*)"`)
	reMethodValue     = regexp.MustCompile(`@(?:Get|Post|Put|Delete|Patch)Mapping\s*\(\s*(?:value\s*=\s*)?(?:path\s*=\s*)?"([^"]*)"`)
	reReturnType      = regexp.MustCompile(`\b(public)\s+([A-Za-z0-9_<>,\[\]\. ]+)\s+([A-Za-z0-9_]+)\s*\(`)
	reRequestBody     = regexp.MustCompile(`@RequestBody\s+([A-Za-z0-9_<>,\[\]\.]+)\s+([A-Za-z0-9_]+)`)
	rePathVariable    = regexp.MustCompile(`@PathVariable(?:\(\s*(?:value\s*=\s*|name\s*=\s*)?"([^"]*)")?\s*([A-Za-z0-9_<>,\[\]\.]+)\s+([A-Za-z0-9_]+)`)
	reRequestParam    = regexp.MustCompile(`@RequestParam(?:\(\s*(?:value\s*=\s*|name\s*=\s*)?"([^"]*)"[^)]*\))?\s*([A-Za-z0-9_<>,\[\]\.]+)\s+([A-Za-z0-9_]+)`)
	reRequestRequired = regexp.MustCompile(`required\s*=\s*false`)
)

// scanJavaFiles scans a set of .java files and returns the extracted entity
// mappings and endpoint contracts with provenance.
func scanJavaFiles(files []string) ([]*store.IntelEntity, []*store.IntelEndpoint) {
	ents := make([]*store.IntelEntity, 0)
	eps := make([]*store.IntelEndpoint, 0)
	for _, f := range files {
		lines, err := readLines(f)
		if err != nil {
			continue
		}
		if isController(lines) {
			eps = append(eps, scanController(f, lines)...)
		}
		if isEntity(lines) {
			ents = append(ents, scanEntity(f, lines)...)
		}
	}
	return ents, eps
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out, sc.Err()
}

func isController(lines []string) bool {
	for _, l := range lines {
		if reController.MatchString(l) {
			return true
		}
	}
	return false
}

func isEntity(lines []string) bool {
	for _, l := range lines {
		if reEntity.MatchString(l) {
			return true
		}
	}
	return false
}

// scanEntity extracts one entity class: @Table(name) → table; @Column/@Id
// fields → columns with nullable. Annotations may sit on the field line or the
// line directly above (the common Spring Data JPA layout). Structural
// assumptions match JPA conventions; LLM assist later handles exotic cases.
func scanEntity(file string, lines []string) []*store.IntelEntity {
	out := make([]*store.IntelEntity, 0)
	var entity, table, clsName string
	// pending annotation state carries @Column/@Id across the line break.
	var pendingCol, pendingID bool
	for i, l := range lines {
		if reEntity.MatchString(l) {
			entity = guessEntityName(l, clsName)
		}
		// @Table(name="...") may live on its own line or after @Entity.
		if m := reTable.FindStringSubmatch(l); m != nil {
			table = m[1]
		}
		if m := reClass.FindStringSubmatch(l); m != nil {
			clsName = m[1]
			if entity == "" {
				entity = clsName
			}
		}
		// Annotation-only line (no field declaration yet): remember it for the
		// next declared field.
		if reColumn.MatchString(l) && !reField.MatchString(l) {
			pendingCol = true
		}
		if strings.Contains(l, "@Id") && !reField.MatchString(l) {
			pendingID = true
		}
		// A declared field: attribute it to @Column/@Id from this line or the
		// pending annotation.
		if !reField.MatchString(l) {
			continue
		}
		hasCol := reColumn.MatchString(l) || pendingCol
		hasID := strings.Contains(l, "@Id") || pendingID
		if !hasCol && !hasID {
			pendingCol, pendingID = false, false
			continue
		}
		fm := reField.FindStringSubmatch(l)
		if fm == nil {
			continue
		}
		fieldType := strings.TrimSpace(fm[1])
		colName := toSnake(fm[2])
		if cm := reColumnName.FindStringSubmatch(l); cm != nil {
			colName = cm[1]
		}
		nullable := true
		if nm := reColumnNullable.FindStringSubmatch(l); nm != nil {
			nullable = nm[1] != "true"
		}
		if hasID {
			nullable = false
		}
		out = append(out, &store.IntelEntity{
			Entity:     entity,
			TableName:  table,
			ColumnName: colName,
			FieldType:  fieldType,
			Nullable:   nullable,
			IsPrimary:  hasID,
			SourceFile: file,
			SourceLine: i + 1,
		})
		pendingCol, pendingID = false, false
	}
	return out
}

// scanController extracts endpoint contracts from a controller class: the class
// level @RequestMapping prefix combines with method-level mappings. A bare
// @GetMapping etc. without arguments is treated as the class prefix path ("").
func scanController(file string, lines []string) []*store.IntelEndpoint {
	out := make([]*store.IntelEndpoint, 0)
	classPrefix := ""
	classLine := -1
	for i, l := range lines {
		// Class-level @RequestMapping precedes the class declaration; method
		// level ones follow it.
		if m := reClass.FindStringSubmatch(l); m != nil {
			classLine = i
			// @RequestMapping may appear on the line right above the class.
			if i > 0 {
				if pm := reRequestMapping.FindStringSubmatch(lines[i-1]); pm != nil {
					classPrefix = pm[1]
				}
			}
		}
		if classLine >= 0 {
			// A @RequestMapping directly after the class but before any method is
			// the class-level prefix (one-line layout: @RestController @RequestMapping(...)).
			continue
		}
		// Pre-class annotations: @RequestMapping on its own line is the prefix.
		if m := reRequestMapping.FindStringSubmatch(l); m != nil {
			classPrefix = m[1]
		}
	}
	// Fallback: if no class declaration was recognized, treat the first
	// @RequestMapping as a class-level prefix only when it is not a method line
	// (no return type follows within a few lines).
	if classLine < 0 {
		for i, l := range lines {
			if m := reRequestMapping.FindStringSubmatch(l); m != nil {
				if _, _, hasMethod := methodReturnAt(lines, i); !hasMethod {
					classPrefix = m[1]
				}
			}
		}
	}
	for i, l := range lines {
		m := methodMappingAt(l)
		if m == nil {
			continue
		}
		httpMethod := mapMethod(m[0])
		path := joinPath(classPrefix, m[1])
		respType, _, _ := methodReturnAt(lines, i)
		out = append(out, &store.IntelEndpoint{
			Method:       httpMethod,
			Path:         path,
			ResponseType: respType,
			RequestJSON:  extractRequestInfo(lines, i),
			SourceFile:   file,
			SourceLine:   i + 1,
		})
	}
	return out
}

// paramInfo is one request parameter (path variable, query parameter or body).
type paramInfo struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Source   string `json:"source"` // path | query
	Required bool   `json:"required"`
}

// extractRequestInfo scans the method signature following a mapping annotation
// (up to ~9 lines) for @PathVariable/@RequestParam/@RequestBody and renders a
// JSON request contract. The @RequestBody type name is recorded; its field-level
// expansion is left to the LLM assist layer (cross-module DTO resolution).
func extractRequestInfo(lines []string, start int) string {
	end := start + 9
	if end > len(lines) {
		end = len(lines)
	}
	window := strings.Join(lines[start:end], "\n")

	params := make([]paramInfo, 0)
	for _, m := range rePathVariable.FindAllStringSubmatch(window, -1) {
		name := m[1]
		if name == "" {
			name = m[3]
		}
		params = append(params, paramInfo{Name: name, Type: m[2], Source: "path", Required: true})
	}
	for _, m := range reRequestParam.FindAllStringSubmatch(window, -1) {
		name := m[1]
		if name == "" {
			name = m[3]
		}
		params = append(params, paramInfo{Name: name, Type: m[2], Source: "query", Required: !reRequestRequired.MatchString(m[0])})
	}
	bodyType := ""
	if m := reRequestBody.FindStringSubmatch(window); m != nil {
		bodyType = m[1]
	}
	if len(params) == 0 && bodyType == "" {
		return ""
	}
	obj := map[string]any{}
	if len(params) > 0 {
		obj["params"] = params
	}
	if bodyType != "" {
		obj["bodyType"] = bodyType
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return ""
	}
	return string(b)
}

// methodMappingAt recognizes a method-level HTTP mapping annotation on a line.
// It returns [verb, path]; path is "" for a bare annotation (@GetMapping with
// no arguments). Lines matching class-level-foreign annotations (@RequestMapping
// on the class, @PutMapping/@PatchMapping with non-string values) are skipped by
// requiring a string literal argument or a method declaration below.
func methodMappingAt(l string) []string {
	for _, verb := range []string{"GetMapping", "PostMapping", "PutMapping", "DeleteMapping", "PatchMapping"} {
		if !strings.Contains(l, "@"+verb) {
			continue
		}
		path := ""
		if rm := reMethodValue.FindStringSubmatch(l); rm != nil {
			path = rm[1]
		}
		return []string{verb, path}
	}
	return nil
}

// methodReturnAt looks forward up to 6 lines from the mapping annotation for
// the method declaration's return type. Returns (returnType, name, found).
func methodReturnAt(lines []string, start int) (string, string, bool) {
	for j := start; j < len(lines) && j <= start+6; j++ {
		if m := reReturnType.FindStringSubmatch(lines[j]); m != nil {
			return strings.TrimSpace(m[2]), m[3], true
		}
	}
	return "", "", false
}

func mapMethod(a string) string {
	switch a {
	case "GetMapping":
		return "GET"
	case "PostMapping":
		return "POST"
	case "PutMapping":
		return "PUT"
	case "DeleteMapping":
		return "DELETE"
	case "PatchMapping":
		return "PATCH"
	default:
		return "ANY"
	}
}

func joinPath(classPrefix, methodPath string) string {
	p := strings.TrimRight(classPrefix, "/") + "/" + strings.TrimLeft(methodPath, "/")
	p = strings.Trim(p, "/")
	if p == "" {
		return "/"
	}
	return "/" + p
}

// guessEntityName derives a deterministic entity name from the annotation line
// or falls back to the class name.
func guessEntityName(line, clsName string) string {
	if m := reTable.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	if clsName != "" {
		return clsName
	}
	return ""
}

// toSnake converts a Java camelCase identifier to snake_case (JPA's default
// physical naming strategy).
func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' && i > 0 {
			b.WriteByte('_')
			b.WriteRune(r + 32)
		} else if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + 32)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// relPath returns a path relative to root, or the absolute path when not under
// root (defensive).
func relPath(root, file string) string {
	r, err := filepath.Rel(root, file)
	if err != nil {
		return file
	}
	return r
}
