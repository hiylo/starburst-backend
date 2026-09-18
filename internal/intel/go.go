package intel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/hiylo/starburst-backend/internal/store"
)

// scanGoFiles extracts a deterministic contract view of a Go module: exported
// functions and exported receiver methods become pseudo "endpoints" carrying
// file:line provenance, and struct types become entities (type → fields, with
// json/db tags as columns). Standard-library HTTP registrations
// (http.HandleFunc / mux.HandleFunc("...")) are captured as concrete routes
// with method "GET" when pathable. This gives non-HTTP Go projects a
// retrievable knowledge surface (RAG 盲区根因) and HTTP Go services real
// endpoint contracts.
func scanGoFiles(files []string) ([]*store.IntelEntity, []*store.IntelEndpoint) {
	entities := make([]*store.IntelEntity, 0, 8)
	endpoints := make([]*store.IntelEndpoint, 0, 8)
	typeSeen := make(map[string]bool)
	routeSeen := make(map[string]bool)

	for _, file := range files {
		base := filepath.Base(file)
		if strings.HasSuffix(base, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, data, 0)
		if err != nil {
			// 解析失败的文件跳过；AST 失败不回退正则，保证处理是确定性的。
			continue
		}

		// 结构体类型 → 实体。复用已登记的类型名，避免同名不同文件重复。
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || !ast.IsExported(ts.Name.Name) || typeSeen[ts.Name.Name] {
					continue
				}
				typeSeen[ts.Name.Name] = true
				line := fset.Position(ts.Pos()).Line
				fields := make([]string, 0, len(st.Fields.List))
				for _, fd := range st.Fields.List {
					if len(fd.Names) == 0 {
						continue
					}
					for _, n := range fd.Names {
						col := goFieldColumn(n.Name, fd.Tag)
						fields = append(fields, col)
						entities = append(entities, &store.IntelEntity{
							Entity:       ts.Name.Name,
							TableName:    strings.ToLower(ts.Name.Name),
							ColumnName:   col,
							FieldType:    exprString(fd.Type),
							IsPrimary:    n.Name == "ID" || n.Name == "Id" || n.Name == "id",
							SourceFile:   file,
							SourceLine:   fset.Position(n.Pos()).Line,
						})
					}
				}
				_ = fields
				_ = line
			}
		}

		// 路由注册 → 具体端点：mux.HandleFunc("/path", fn) / http.HandleFunc。
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			se, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || se.Sel.Name != "HandleFunc" {
				return true
			}
			if len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			path := strings.Trim(strings.TrimSpace(lit.Value), `"`)
			key := path
			if routeSeen[key] {
				return true
			}
			routeSeen[key] = true
			endpoints = append(endpoints, &store.IntelEndpoint{
				Method:       "GET",
				Path:         path,
				ResponseType: "http.HandlerFunc",
				SourceFile:   file,
				SourceLine:   fset.Position(call.Pos()).Line,
			})
			return true
		})

		// 导出函数 / 导出 receiver 方法 → 伪端点（纯函数项目 RAG 可检索）。
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			line := fset.Position(fn.Pos()).Line
			sig := funcSignature(fn)
			if fn.Recv == nil {
				if !ast.IsExported(name) {
					continue
				}
				endpoints = append(endpoints, &store.IntelEndpoint{
					Method:       "FUNC",
					Path:         "/func/" + name,
					ResponseType: sig,
					SourceFile:   file,
					SourceLine:   line,
				})
				continue
			}
			recvType := goRecvType(fn.Recv)
			if recvType == "" || !ast.IsExported(recvType) {
				continue
			}
			endpoints = append(endpoints, &store.IntelEndpoint{
				Method:       "METHOD",
				Path:         "/method/" + strings.ToLower(recvType) + "/" + name,
				ResponseType: sig,
				SourceFile:   file,
				SourceLine:   line,
			})
		}
	}
	return entities, endpoints
}

// goRecvType returns the base type name of a method receiver (strips * and
// generic brackets).
func goRecvType(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	t := exprString(recv.List[0].Type)
	t = strings.TrimPrefix(t, "*")
	if i := strings.IndexByte(t, '['); i > 0 {
		t = t[:i]
	}
	return t
}

// funcSignature renders a function/method signature deterministically
// (name(args…) results…), e.g. "Add(a, b int) int".
func funcSignature(fn *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString(fn.Name.Name)
	b.WriteString("(")
	b.WriteString(paramsString(fn.Type.Params))
	b.WriteString(")")
	if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
		b.WriteString(" ")
		b.WriteString(paramsString(fn.Type.Results))
	}
	if doc := fn.Doc; doc != nil {
		if txt := strings.TrimSpace(doc.Text()); txt != "" {
			b.WriteString(" // " + firstLine(txt))
		}
	}
	return b.String()
}

func paramsString(fl *ast.FieldList) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fl.List))
	for _, f := range fl.List {
		names := ""
		if len(f.Names) > 0 {
			ns := make([]string, 0, len(f.Names))
			for _, n := range f.Names {
				ns = append(ns, n.Name)
			}
			names = strings.Join(ns, ", ") + " "
		}
		parts = append(parts, names+exprString(f.Type))
	}
	return strings.Join(parts, ", ")
}

// exprString renders an AST expression back to source form.
func exprString(e ast.Expr) string {
	if e == nil {
		return ""
	}
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		el := exprString(t.Elt)
		if t.Len == nil {
			return "[]" + el
		}
		return "[" + exprString(t.Len) + "]" + el
	case *ast.MapType:
		return "map[" + exprString(t.Key) + "]" + exprString(t.Value)
	case *ast.FuncType:
		return "func(" + paramsString(t.Params) + ")"
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.BasicLit:
		return t.Value
	case *ast.ChanType:
		return "chan " + exprString(t.Value)
	case *ast.ParenExpr:
		return "(" + exprString(t.X) + ")"
	default:
		return "<expr>"
	}
}

// goFieldColumn derives a struct field's column-ish name from its struct tag
// json:"..." (preferred), else db:"...", else the lowercased field name.
func goFieldColumn(name string, tag *ast.BasicLit) string {
	if tag != nil {
		raw := strings.Trim(tag.Value, "`")
		for _, key := range []string{"json", "db"} {
			val := tagValue(raw, key)
			if val != "" && val != "-" {
				if i := strings.IndexByte(val, ','); i > 0 {
					val = val[:i]
				}
				return val
			}
		}
	}
	return strings.ToLower(name)
}

// tagValue extracts the value of a struct-tag key.
func tagValue(tag, key string) string {
	needle := key + ":"
	i := strings.Index(tag, needle)
	if i < 0 {
		return ""
	}
	rest := tag[i+len(needle):]
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	if j := strings.Index(rest, `"`); j >= 0 {
		return rest[:j]
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}