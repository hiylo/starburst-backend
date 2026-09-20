package android

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hiylo/starburst-backend/internal/store"
)

// Kotlin contract scanning for Android modules: deterministic, line-anchored
// extraction of entity↔table↔column mappings (Room @Entity/@Table + data class
// constructor fields) and Retrofit interface endpoints (@GET/@POST/... path
// annotations directly above a method). Mirrors the Java scanner's "every row
// carries source_file:line provenance" guarantee; multi-line annotations and
// inherited/generic mappings are left to the LLM assist layer later.

var (
	// reKtEntity matches the bare @Entity marker.
	reKtEntity = regexp.MustCompile(`@Entity\b`)
	// reKtEntityTable matches Room-style @Entity(tableName = "...").
	reKtEntityTable = regexp.MustCompile(`@Entity\s*\(\s*tableName\s*=\s*"([^"]+)"`)
	// reKtTable matches JPA-style @Table(name = "...").
	reKtTable = regexp.MustCompile(`@Table\s*\(\s*name\s*=\s*"([^"]+)"`)
	// reKtDataClass matches a `data class Xxx` declaration.
	reKtDataClass = regexp.MustCompile(`\bdata\s+class\s+([A-Za-z0-9_$]+)`)
	// reKtClass matches a plain `class Xxx` declaration.
	reKtClass = regexp.MustCompile(`\bclass\s+([A-Za-z0-9_$]+)`)
	// reKtInterface matches a `interface Xxx` declaration.
	reKtInterface = regexp.MustCompile(`\binterface\s+([A-Za-z0-9_$]+)`)
	// reKtField matches a val/var property declaration `name: Type[?]`. The type
	// capture runs up to = , ) or newline so multi-field data class constructor
	// lines split cleanly; a trailing `?` marks nullability.
	reKtField = regexp.MustCompile(`\b(val|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*:\s*([^,=\n()]+)`)
	// reRetrofitAt matches a Retrofit HTTP method annotation with a string path
	// argument, e.g. @GET("api/users"). Bare annotations without a path are
	// skipped because they do not match.
	reRetrofitAt = regexp.MustCompile(`@(GET|POST|PUT|DELETE|PATCH)\(\s*"([^"]*)"`)
	// reKtFun matches a Kotlin function declaration.
	reKtFun = regexp.MustCompile(`\bfun\s+[A-Za-z0-9_$]+\s*\(`)
)

// ktEntityCtx is the entity currently being scanned: its owning class name,
// table name and the brace depth at which the class was declared.
type ktEntityCtx struct {
	name   string
	table  string
	entity bool
	base   int
	line   int
	opened bool // true once the class body contour rose above base (has a {})
}

// ktField is one property extracted from an entity class.
type ktField struct {
	name     string
	typ      string
	nullable bool
}

// ScanKotlin scans dir for .kt files and returns the extracted entity mappings
// and Retrofit endpoint contracts with provenance. .git, build, .gradle and
// node_modules directories are skipped. Endpoints are deduplicated by
// (Method, Path); entity/endpoint SourceFile paths are relative to dir.
func ScanKotlin(dir string) ([]*store.IntelEntity, []*store.IntelEndpoint, error) {
	files := make([]string, 0)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipKtDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(info.Name(), ".kt") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	ents := make([]*store.IntelEntity, 0)
	eps := make([]*store.IntelEndpoint, 0)
	for _, f := range files {
		e, p := scanKotlinFile(dir, f)
		ents = append(ents, e...)
		eps = append(eps, p...)
	}
	return ents, dedupeKotlinEndpoints(eps), nil
}

// skipKtDir reports whether a directory name is generated/vendored output that
// must not be scanned for Kotlin sources.
func skipKtDir(name string) bool {
	switch name {
	case ".git", "build", ".gradle", "node_modules":
		return true
	}
	return false
}

// scanKotlinFile extracts entities and endpoints from a single .kt file.
// Source paths are rendered relative to dir (slash-normalized).
func scanKotlinFile(dir, file string) ([]*store.IntelEntity, []*store.IntelEndpoint) {
	lines, err := readKotlinLines(file)
	if err != nil {
		return nil, nil
	}
	rel, err := filepath.Rel(dir, file)
	if err != nil {
		rel = file
	}
	rel = filepath.ToSlash(rel)

	ents := make([]*store.IntelEntity, 0)
	eps := make([]*store.IntelEndpoint, 0)

	cur := 0 // brace depth at the start of the current line
	var ctx *ktEntityCtx
	var ifaceLine = -1
	var ifaceBase = -1
	var ifaceOpened bool
	// Pending annotation state carried to the next class/field declaration.
	pendingEntity := false
	pendingTable := ""
	pendingID := false

	for i, l := range lines {
		// Class-level annotations that precede the next class declaration.
		if reKtEntity.MatchString(l) {
			pendingEntity = true
		}
		if m := reKtEntityTable.FindStringSubmatch(l); m != nil {
			pendingTable = m[1]
		}
		if m := reKtTable.FindStringSubmatch(l); m != nil {
			pendingTable = m[1]
		}

		// Field-level annotations (@Id/@PrimaryKey) that precede the next field.
		fieldID := strings.Contains(l, "@Id") || strings.Contains(l, "@PrimaryKey")
		if fieldID && !reKtField.MatchString(l) {
			pendingID = true
		}
		hasID := fieldID || pendingID

		// Class / interface declaration.
		if m := reKtDataClass.FindStringSubmatch(l); m != nil {
			// data classes are entities by default (spec: unannotated data class
			// name becomes the entity name).
			ctx = &ktEntityCtx{name: m[1], table: pendingTable, entity: true, base: cur, line: i}
			pendingEntity, pendingTable = false, ""
			for _, f := range fieldsIn(l) {
				ents = append(ents, kotlinEntity(ctx, f, rel, i+1, hasID))
			}
			pendingID = false
		} else if m := reKtClass.FindStringSubmatch(l); m != nil {
			isEntity := pendingEntity || pendingTable != "" || reKtEntity.MatchString(l)
			ctx = &ktEntityCtx{name: m[1], table: pendingTable, entity: isEntity, base: cur, line: i}
			pendingEntity, pendingTable = false, ""
			if isEntity {
				for _, f := range fieldsIn(l) {
					ents = append(ents, kotlinEntity(ctx, f, rel, i+1, hasID))
				}
			}
			pendingID = false
		} else if m := reKtInterface.FindStringSubmatch(l); m != nil {
			ifaceLine, ifaceBase, ifaceOpened = i, cur, false
		}

		// Retrofit endpoint: the HTTP method annotation sits on the line directly
		// above (or on the same line as) a method declaration, inside an interface.
		if am := reRetrofitAt.FindStringSubmatch(l); am != nil {
			if ifaceLine >= 0 && i > ifaceLine && ifaceOpened && cur <= ifaceBase {
				ifaceLine, ifaceBase, ifaceOpened = -1, -1, false
			}
			if ifaceLine >= 0 && funFollows(lines, i) {
				eps = append(eps, &store.IntelEndpoint{
					Method:     am[1],
					Path:       am[2],
					SourceFile: rel,
					SourceLine: i + 1,
				})
			}
		}

		// Fields of the current entity (declared after the class header).
		if ctx != nil && ctx.entity && i > ctx.line && cur < ctx.base+1 {
			for _, f := range fieldsIn(l) {
				ents = append(ents, kotlinEntity(ctx, f, rel, i+1, hasID))
			}
			if reKtField.MatchString(l) {
				pendingID = false
			}
		}

		// Close contexts once a brace-owning class/interface returns to base
		// depth. Brace-less declarations (single-line data classes) stay open so
		// their constructor fields on following lines are still attributed.
		if ctx != nil && ctx.opened && i > ctx.line && cur <= ctx.base {
			ctx = nil
		}
		if ifaceLine >= 0 && ifaceOpened && i > ifaceLine && cur <= ifaceBase {
			ifaceLine, ifaceBase, ifaceOpened = -1, -1, false
		}

		cur += braceDelta(l)

		// Mark brace-owning contexts as opened once depth rises above base.
		if ctx != nil && cur > ctx.base {
			ctx.opened = true
		}
		if ifaceLine >= 0 && cur > ifaceBase {
			ifaceOpened = true
		}
	}
	return ents, eps
}

// readKotlinLines reads a file into its lines (buffered like the Java scanner).
func readKotlinLines(path string) ([]string, error) {
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

// kotlinEntity renders one IntelEntity from an entity context and a field.
func kotlinEntity(ctx *ktEntityCtx, f ktField, rel string, line int, hasID bool) *store.IntelEntity {
	return &store.IntelEntity{
		Entity:     ctx.name,
		TableName:  ctx.table,
		ColumnName: f.name,
		FieldType:  f.typ,
		Nullable:   f.nullable,
		IsPrimary:  hasID,
		SourceFile: rel,
		SourceLine: line,
	}
}

// fieldsIn extracts every val/var property declaration on a line.
func fieldsIn(l string) []ktField {
	out := make([]ktField, 0)
	for _, m := range reKtField.FindAllStringSubmatch(l, -1) {
		raw := strings.TrimSpace(m[3])
		nullable := strings.HasSuffix(raw, "?")
		typ := strings.TrimSpace(strings.TrimSuffix(raw, "?"))
		if typ == "" {
			continue
		}
		out = append(out, ktField{name: m[2], typ: typ, nullable: nullable})
	}
	return out
}

// funFollows reports whether a function declaration sits on the line directly
// after i (the annotation line) or on the annotation line itself.
func funFollows(lines []string, i int) bool {
	if i+1 < len(lines) && reKtFun.MatchString(lines[i+1]) {
		return true
	}
	return reKtFun.MatchString(lines[i])
}

// braceDelta counts the net brace-depth change a line introduces (strings and
// comments are not lexed; close enough for line-anchored scanning).
func braceDelta(l string) int {
	d := 0
	for _, r := range l {
		switch r {
		case '{':
			d++
		case '}':
			d--
		}
	}
	return d
}

// dedupeKotlinEndpoints collapses endpoints sharing a (Method, Path) key,
// keeping the first occurrence.
func dedupeKotlinEndpoints(eps []*store.IntelEndpoint) []*store.IntelEndpoint {
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
