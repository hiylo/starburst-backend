// Package android deterministically extracts Android layout field bindings
// (page -> data field path) from DataBinding expressions so the test
// intelligence system can verify the "must-display field list" (M6): when the
// backend returns null for one of these bound fields, the client necessarily
// breaks and the failure is reported as CLIENT_MISSING_FIELD.
//
// The extractor is deliberately simple and line-anchored: it scans DataBinding
// expressions (@{...}) inside layout XML, resolves the owning widget (element
// tag), and reduces the expression to a dotted field-access path. Resource
// references (@string/xxx) and ViewBinding-only markers are ignored; method
// calls and array/map indexes are dropped from the path. Every binding carries
// source-file:line provenance for auditability.
package android

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Binding is one "page -> field" binding extracted from a layout.
type Binding struct {
	Page      string `json:"page"`      // layout file name without extension (e.g. activity_main)
	FieldPath string `json:"fieldPath"` // bound data field path (e.g. user.name, bannerList.title)
	Widget    string `json:"widget"`    // widget type (TextView/ImageView/EditText/RecyclerView)
	Source    string `json:"source"`    // source file path + line, e.g. res/layout/activity_main.xml:23
}

var (
	// reBind matches a DataBinding expression @{...} (no nested braces).
	reBind = regexp.MustCompile(`@\{([^{}]*)\}`)
	// reOpenTag matches an opening element tag <Tag (not </ or <!).
	reOpenTag = regexp.MustCompile(`<\s*([A-Za-z_][A-Za-z0-9_.:-]*)`)
	// reCloseTag matches a closing element tag </Tag.
	reCloseTag = regexp.MustCompile(`</\s*([A-Za-z_][A-Za-z0-9_.:-]*)`)
	// reSelfClose matches the trailing /> of a self-closing element.
	reSelfClose = regexp.MustCompile(`/>`)
	// reIdent matches a single identifier token.
	reIdent = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
)

// viewModelPrefixes are DataBinding root prefixes that reference the bound
// model itself; they are stripped so only the real field path remains.
var viewModelPrefixes = map[string]bool{"viewmodel": true, "bean": true}

// ExtractLayout extracts all data bindings from a single layout XML document.
// layoutName is the layout file name without extension and becomes Binding.Page.
func ExtractLayout(layoutName string, data []byte) []Binding {
	return extractLayout(strings.TrimSuffix(layoutName, ".xml"),
		"res/layout/"+layoutName+".xml", data)
}

// ExtractBindings scans every XML file under res/layout (or resDir itself when
// no layout subdirectory exists), aggregates the bindings and returns them
// sorted by page (and, within a page, by source line order). Source paths are
// reported relative to resDir.
func ExtractBindings(resDir string) ([]Binding, error) {
	layoutDir := resDir
	if fi, err := os.Stat(filepath.Join(resDir, "layout")); err == nil && fi.IsDir() {
		layoutDir = filepath.Join(resDir, "layout")
	}

	var out []Binding
	err := filepath.Walk(layoutDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".xml" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(resDir, path)
		if rerr != nil {
			rel = path
		}
		page := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		out = append(out, extractLayout(page, filepath.ToSlash(rel), data)...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Group by page; within a page the entries were appended in line order, so a
	// stable sort keeps the per-file source order intact.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Page < out[j].Page })
	return out, nil
}

// extractLayout is the shared extractor. sourcePath is the relative path (plus
// .xml) rendered into each binding's Source, and page is the layout name used
// for Binding.Page.
func extractLayout(page, sourcePath string, data []byte) []Binding {
	out := make([]Binding, 0)
	stack := make([]string, 0)
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		for _, ev := range scanLine(line) {
			switch ev.kind {
			case evOpen:
				stack = append(stack, ev.value)
			case evClose:
				stack = popMatching(stack, ev.value)
			case evSelf:
				stack = popTop(stack)
			case evBind:
				fp := fieldPath(ev.value)
				if fp == "" {
					continue
				}
				out = append(out, Binding{
					Page:      page,
					FieldPath: fp,
					Widget:    widgetTag(top(stack)),
					Source:    sourcePath + ":" + strconv.Itoa(i+1),
				})
			}
		}
	}
	return out
}

// eventKind discriminates the per-line tokens produced by scanLine.
type eventKind int

const (
	evOpen eventKind = iota
	evClose
	evSelf
	evBind
)

// event is one token on a line, positioned by its byte offset so that element
// push/pop and binding resolution happen in source order.
type event struct {
	pos   int
	kind  eventKind
	value string
}

// scanLine tokenizes a single line into ordered events: element open/close,
// self-close and binding expressions.
func scanLine(line string) []event {
	events := make([]event, 0)
	for _, m := range reBind.FindAllStringSubmatchIndex(line, -1) {
		events = append(events, event{pos: m[0], kind: evBind, value: line[m[2]:m[3]]})
	}
	for _, m := range reCloseTag.FindAllStringSubmatchIndex(line, -1) {
		events = append(events, event{pos: m[0], kind: evClose, value: line[m[2]:m[3]]})
	}
	for _, m := range reSelfClose.FindAllStringIndex(line, -1) {
		events = append(events, event{pos: m[0], kind: evSelf})
	}
	for _, m := range reOpenTag.FindAllStringSubmatchIndex(line, -1) {
		events = append(events, event{pos: m[0], kind: evOpen, value: line[m[2]:m[3]]})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].pos < events[j].pos })
	return events
}

// fieldPath reduces a DataBinding expression to a dotted field-access path:
// identifier chain only, dropping method calls, array/map indexes, string and
// numeric literals, and stripping a leading viewModel/bean root prefix.
func fieldPath(expr string) string {
	segs := make([]string, 0)
	for _, part := range strings.Split(expr, ".") {
		part = strings.TrimSpace(part)
		if part == "" || strings.Contains(part, "(") {
			continue
		}
		if idx := strings.Index(part, "["); idx >= 0 {
			part = strings.TrimSpace(part[:idx])
		}
		ident := reIdent.FindString(part)
		if ident == "" || isDigits(ident) {
			continue
		}
		segs = append(segs, ident)
	}
	if len(segs) > 0 && viewModelPrefixes[strings.ToLower(segs[0])] {
		segs = segs[1:]
	}
	return strings.Join(segs, ".")
}

// widgetTag normalizes an element tag to its plain widget type: strips an XML
// namespace prefix and a fully-qualified class package.
func widgetTag(tag string) string {
	if tag == "" {
		return ""
	}
	if i := strings.Index(tag, ":"); i >= 0 {
		tag = tag[i+1:]
	}
	if i := strings.LastIndex(tag, "."); i >= 0 {
		tag = tag[i+1:]
	}
	return tag
}

// top returns the innermost open element, or "" when the stack is empty.
func top(stack []string) string {
	if len(stack) == 0 {
		return ""
	}
	return stack[len(stack)-1]
}

// popTop removes the innermost open element (used for self-closing tags).
func popTop(stack []string) []string {
	if len(stack) == 0 {
		return stack
	}
	return stack[:len(stack)-1]
}

// popMatching removes the named element and everything nested above it.
func popMatching(stack []string, name string) []string {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == name {
			return stack[:i]
		}
	}
	return stack
}

// isDigits reports whether s is non-empty and contains only ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
