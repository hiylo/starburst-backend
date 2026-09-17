package intel

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

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
	reInterface       = regexp.MustCompile(`\binterface\s+([A-Za-z0-9_]+)`)
	reImplements      = regexp.MustCompile(`\bimplements\s+([A-Za-z0-9_]+(?:\s*,\s*[A-Za-z0-9_]+)*)`)
	reImport          = regexp.MustCompile(`^import\s+(?:static\s+)?([\w.]+)\s*;`)
	rePackage         = regexp.MustCompile(`^package\s+([\w.]+)\s*;`)
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
// mappings and endpoint contracts with provenance. idx is the repo-wide
// FQCN→file index used to resolve controller interface (Feign) contracts.
func scanJavaFiles(files []string, idx map[string]string) ([]*store.IntelEntity, []*store.IntelEndpoint) {
	ents := make([]*store.IntelEntity, 0)
	eps := make([]*store.IntelEndpoint, 0)
	for _, f := range files {
		lines, err := readLines(f)
		if err != nil {
			continue
		}
		if isController(lines) {
			eps = append(eps, scanController(f, lines, idx)...)
		}
		if isEntity(lines) {
			ents = append(ents, scanEntity(f, lines)...)
		}
	}
	return ents, dedupeEndpoints(eps)
}

// dedupeEndpoints collapses endpoints sharing a (method, path) key, keeping the
// first occurrence (the direct controller mapping, which precedes any
// interface-inherited duplicate). Cross-module same-path endpoints are not
// deduplicated: they are distinct services (e.g. a BFF proxy vs the provider).
func dedupeEndpoints(eps []*store.IntelEndpoint) []*store.IntelEndpoint {
	seen := make(map[string]bool)
	out := make([]*store.IntelEndpoint, 0, len(eps))
	for _, ep := range eps {
		key := ep.Method + " " + ep.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ep)
	}
	return out
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
// When the controller implements a Feign provider interface, the mappings
// declared on that interface are resolved (via idx) and added as endpoints with
// the interface file as provenance.
func scanController(file string, lines []string, idx map[string]string) []*store.IntelEndpoint {
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
			FieldsJSON:   resolveResponseFields(respType, lines, idx),
			SourceFile:   file,
			SourceLine:   i + 1,
		})
	}
	// Resolve endpoints inherited from implemented Feign provider interfaces.
	out = append(out, resolveInterfaceEndpoints(lines, idx)...)
	return out
}

// resolveInterfaceEndpoints finds the interfaces a controller implements and,
// for each one that declares REST mappings, extracts those mappings (attributed
// to the interface file, the source of truth for the path). This covers the
// common "controller implements Feign contract interface" layout where the
// @GetMapping etc. live on the interface rather than the controller.
func resolveInterfaceEndpoints(lines []string, idx map[string]string) []*store.IntelEndpoint {
	if len(idx) == 0 {
		return nil
	}
	pkg := packageOf(lines)
	imports := importsOf(lines)
	out := make([]*store.IntelEndpoint, 0)
	seen := make(map[string]bool)
	for _, l := range lines {
		m := reImplements.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		for _, name := range strings.Split(m[1], ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			fqcn := imports[name]
			if fqcn == "" {
				if pkg != "" {
					fqcn = pkg + "." + name
				} else {
					fqcn = name
				}
			}
			ifaceFile, ok := idx[fqcn]
			if !ok || seen[ifaceFile] {
				continue
			}
			seen[ifaceFile] = true
			ifaceLines, err := readLines(ifaceFile)
			if err != nil {
				continue
			}
			for i, il := range ifaceLines {
				mm := methodMappingAt(il)
				if mm == nil {
					continue
				}
				path := joinPath("", mm[1])
				respType, _, _ := methodReturnAt(ifaceLines, i)
				out = append(out, &store.IntelEndpoint{
					Method:       mapMethod(mm[0]),
					Path:         path,
					ResponseType: respType,
					RequestJSON:  extractRequestInfo(ifaceLines, i),
					FieldsJSON:   resolveResponseFields(respType, ifaceLines, idx),
					SourceFile:   ifaceFile,
					SourceLine:   i + 1,
				})
			}
		}
	}
	return out
}

// packageOf extracts the package declaration of a Java file.
func packageOf(lines []string) string {
	for _, l := range lines {
		if m := rePackage.FindStringSubmatch(l); m != nil {
			return m[1]
		}
	}
	return ""
}

// importsOf maps imported simple names to their fully-qualified names.
func importsOf(lines []string) map[string]string {
	out := make(map[string]string)
	for _, l := range lines {
		if m := reImport.FindStringSubmatch(l); m != nil {
			fqcn := m[1]
			out[fqcn[strings.LastIndexByte(fqcn, '.')+1:]] = fqcn
		}
	}
	return out
}

// classIndexCache memoizes the repo-wide FQCN→file index per root so a
// monorepo scan builds it once instead of once per module.
var (
	classIndexMu    sync.Mutex
	classIndexByKey = map[string]map[string]string{}
)

// classIndexFor returns the FQCN→absolute-path index for a repository root,
// building and caching it on first use.
func classIndexFor(root string) map[string]string {
	classIndexMu.Lock()
	defer classIndexMu.Unlock()
	if idx, ok := classIndexByKey[root]; ok {
		return idx
	}
	idx := buildClassIndex(root)
	classIndexByKey[root] = idx
	return idx
}

// buildClassIndex walks root for .java files and maps each top-level class /
// interface fully-qualified name to its absolute file path.
func buildClassIndex(root string) map[string]string {
	idx := make(map[string]string)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "target" || name == "node_modules" ||
				name == ".gradle" || name == "build_artifacts" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".java") {
			return nil
		}
		pkg, cls := readPackageAndClass(path)
		if cls != "" {
			fqcn := cls
			if pkg != "" {
				fqcn = pkg + "." + cls
			}
			idx[fqcn] = path
		}
		return nil
	})
	return idx
}

// readPackageAndClass reads the package and the first top-level class or
// interface name from a Java file.
func readPackageAndClass(path string) (string, string) {
	lines, err := readLines(path)
	if err != nil {
		return "", ""
	}
	pkg := ""
	for _, l := range lines {
		if m := rePackage.FindStringSubmatch(l); m != nil {
			pkg = m[1]
			continue
		}
		if m := reClass.FindStringSubmatch(l); m != nil {
			return pkg, m[1]
		}
		if m := reInterface.FindStringSubmatch(l); m != nil {
			return pkg, m[1]
		}
	}
	return pkg, ""
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

// fieldSpec is one response-field contract (name + JSON type + required flag)
// produced from a DTO class.
type fieldSpec struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

// resolveResponseFields resolves an endpoint's return type to the fields of the
// DTO it wraps (via the repo-wide class index), rendering them as a JSON array
// of {name,type,required}. It returns "" for primitive/void/list-of-primitive
// responses or when the DTO cannot be located.
func resolveResponseFields(returnType string, lines []string, idx map[string]string) string {
	dto := innermostType(returnType)
	if dto == "" || isPrimitiveOrVoid(dto) || len(idx) == 0 {
		return ""
	}
	pkg := packageOf(lines)
	imports := importsOf(lines)
	var fqcn string
	if strings.Contains(dto, ".") {
		fqcn = dto
	} else if imp := imports[dto]; imp != "" {
		fqcn = imp
	} else if pkg != "" {
		fqcn = pkg + "." + dto
	} else {
		fqcn = dto
	}
	file, ok := idx[fqcn]
	if !ok {
		return ""
	}
	dtoLines, err := readLines(file)
	if err != nil {
		return ""
	}
	fields := extractDtoFields(dtoLines)
	if len(fields) == 0 {
		return ""
	}
	b, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	return string(b)
}

// innermostType extracts the innermost generic type argument of a return type
// (OperationResponse<FriendPageDto> / List<FriendPageDto> → FriendPageDto), or
// the bare type when there is no generic.
func innermostType(t string) string {
	t = strings.TrimSpace(t)
	if i := strings.LastIndexByte(t, '<'); i >= 0 {
		inner := t[i+1:]
		if j := strings.IndexByte(inner, '>'); j >= 0 {
			inner = inner[:j]
		}
		return strings.TrimSpace(inner)
	}
	return t
}

// isPrimitiveOrVoid reports whether a Java type name has no meaningful field
// contract (void, wrappers, strings, collections, dates).
func isPrimitiveOrVoid(t string) bool {
	switch t {
	case "void", "Void", "String", "Integer", "Long", "int", "long", "boolean", "Boolean",
		"double", "Double", "float", "Float", "short", "Short", "byte", "Byte", "char",
		"Character", "BigDecimal", "BigInteger", "Date", "LocalDate", "LocalDateTime",
		"LocalTime", "Object", "Map", "List", "Set", "Collection", "UUID":
		return true
	}
	return false
}

// extractDtoFields extracts the declared fields of a DTO class, mapping each
// Java type to a JSON type. required defaults to false (nullability annotations
// are not yet parsed; the LLM assist layer can refine it later).
func extractDtoFields(lines []string) []fieldSpec {
	out := make([]fieldSpec, 0)
	for _, l := range lines {
		fm := reField.FindStringSubmatch(l)
		if fm == nil {
			continue
		}
		out = append(out, fieldSpec{
			Name: fm[2],
			Type: javaToJSONType(fm[1]),
		})
	}
	return out
}

// javaToJSONType maps a Java type to one of the canonical JSON types used by
// the contract checker (string|number|boolean|array|object).
func javaToJSONType(t string) string {
	t = strings.TrimSpace(t)
	switch t {
	case "String", "char", "Character", "CharSequence", "Date", "LocalDate",
		"LocalDateTime", "LocalTime", "UUID", "BigDecimal":
		return "string"
	case "int", "long", "short", "byte", "double", "float", "Integer", "Long",
		"Short", "Byte", "Double", "Float", "BigInteger", "Number":
		return "number"
	case "boolean", "Boolean":
		return "boolean"
	}
	if strings.HasPrefix(t, "List<") || strings.HasPrefix(t, "Set<") ||
		strings.HasPrefix(t, "Collection<") || strings.HasSuffix(t, "[]") {
		return "array"
	}
	if strings.HasPrefix(t, "Map<") {
		return "object"
	}
	return "object"
}
