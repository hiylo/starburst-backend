// Package web deterministically extracts Vue (SFC) template field bindings
// (page -> data field path) so the test-intelligence system can build the
// client's "must-display field list" for Web frontends, mirroring the Android
// DataBinding extractor. Bindings are line-anchored and carry source-file:line
// provenance.
//
// The extractor is deliberately heuristic: it reduces each directive's
// expression (interpolation, v-model, :attr / v-bind, v-text/v-html, v-if/v-show,
// v-for) to a dotted identifier chain, dropping operators, literals and method
// calls. Each binding records the directive kind in Slot.
package web

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Binding is one "page -> field" binding extracted from a Vue template.
type Binding struct {
	Page      string `json:"page"`      // component path without extension (e.g. views/users/UserDetail)
	FieldPath string `json:"fieldPath"` // bound data field path (e.g. detail.nickname)
	Slot      string `json:"slot"`      // directive kind: interpolation | v-model | v-bind | v-text | v-if | v-for
	Source    string `json:"source"`    // source file path + line, e.g. views/users/UserDetail.vue:12
}

var (
	reInterpolation = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)
	reVModel        = regexp.MustCompile(`v-model="([^"]*)"`)
	reVBind         = regexp.MustCompile(`:([A-Za-z][A-Za-z0-9-]*)="([^"]*)"`)
	reVBindLong     = regexp.MustCompile(`v-bind:([A-Za-z][A-Za-z0-9-]*)="([^"]*)"`)
	reVText         = regexp.MustCompile(`v-(text|html)="([^"]*)"`)
	reVCond         = regexp.MustCompile(`v-(if|show)="([^"]*)"`)
	reVFor          = regexp.MustCompile(`v-for="([^"]*)"`)

	reStr   = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|` + "`[^`]*`")
	reNum   = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	reChain = regexp.MustCompile(`[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)*`)
)

// keywords are identifier tokens that never represent a data field.
var keywords = map[string]bool{
	"true": true, "false": true, "null": true, "undefined": true, "this": true,
	"in": true, "of": true, "new": true, "typeof": true, "instanceof": true,
	"function": true, "return": true, "if": true, "else": true, "async": true,
	"await": true, "let": true, "const": true, "var": true,
}

// ExtractBindings scans every .vue file under srcDir (recursively), extracts the
// template bindings and returns them sorted by page (and, within a page, by
// source line order). Source paths are reported relative to srcDir.
func ExtractBindings(srcDir string) ([]Binding, error) {
	var out []Binding
	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "node_modules" || info.Name() == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".vue" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(srcDir, path)
		if rerr != nil {
			rel = path
		}
		page := strings.TrimSuffix(filepath.ToSlash(rel), ".vue")
		out = append(out, extractTemplate(page, filepath.ToSlash(rel), string(data))...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Page < out[j].Page })
	return out, nil
}

// extractTemplate extracts all bindings from a single Vue SFC's template block.
func extractTemplate(page, sourcePath, content string) []Binding {
	out := make([]Binding, 0)
	tpl := templateSection(content)
	lines := strings.Split(tpl, "\n")
	// templateSection strips the <template> line, so re-anchor line numbers by
	// counting lines before the template block in the original content.
	base := lineOffset(content, tpl)
	for i, line := range lines {
		seen := map[string]bool{}
		add := func(expr, slot string) {
			fp := reduceExpression(expr)
			if fp == "" {
				return
			}
			if seen[slot+"|"+fp] {
				return
			}
			seen[slot+"|"+fp] = true
			out = append(out, Binding{
				Page:      page,
				FieldPath: fp,
				Slot:      slot,
				Source:    sourcePath + ":" + strconv.Itoa(base+i+1),
			})
		}
		for _, m := range reInterpolation.FindAllStringSubmatch(line, -1) {
			add(m[1], "interpolation")
		}
		for _, m := range reVModel.FindAllStringSubmatch(line, -1) {
			add(m[1], "v-model")
		}
		for _, m := range reVBind.FindAllStringSubmatch(line, -1) {
			add(m[2], "v-bind:"+m[1])
		}
		for _, m := range reVBindLong.FindAllStringSubmatch(line, -1) {
			add(m[2], "v-bind:"+m[1])
		}
		for _, m := range reVText.FindAllStringSubmatch(line, -1) {
			add(m[2], "v-"+m[1])
		}
		for _, m := range reVCond.FindAllStringSubmatch(line, -1) {
			add(m[2], "v-"+m[1])
		}
		for _, m := range reVFor.FindAllStringSubmatch(line, -1) {
			add(vForCollection(m[1]), "v-for")
		}
	}
	return out
}

// vForCollection returns the collection expression of a v-for directive (the
// right-hand side of "in"/"of"), which is the data field the loop reads.
func vForCollection(expr string) string {
	parts := strings.Split(expr, " in ")
	if len(parts) == 2 {
		return parts[1]
	}
	parts = strings.Split(expr, " of ")
	if len(parts) == 2 {
		return parts[1]
	}
	return expr
}

// templateSection returns the contents between the first <template ...> and its
// closing tag, or the whole document when no template tag is present.
func templateSection(content string) string {
	start := strings.Index(content, "<template")
	if start < 0 {
		return content
	}
	gt := strings.Index(content[start:], ">")
	if gt < 0 {
		return content
	}
	start += gt + 1
	end := strings.Index(content[start:], "</template>")
	if end < 0 {
		return content[start:]
	}
	return content[start : start+end]
}

// lineOffset returns the 0-based line index where tpl (a substring of content)
// begins, so extracted line numbers stay anchored to the original file.
func lineOffset(content, tpl string) int {
	idx := strings.Index(content, tpl)
	if idx < 0 {
		return 0
	}
	return strings.Count(content[:idx], "\n")
}

// reduceExpression reduces a Vue template expression to its most significant
// dotted identifier chain (the data field it reads). Operators, string/number
// literals and method calls are dropped; the deepest field path wins.
func reduceExpression(expr string) string {
	s := reStr.ReplaceAllString(expr, " ")
	s = reNum.ReplaceAllString(s, " ")
	matches := reChain.FindAllStringIndex(s, -1)
	best := ""
	bestDots := -1
	for _, m := range matches {
		tok := s[m[0]:m[1]]
		if keywords[tok] {
			continue
		}
		if m[1] < len(s) && s[m[1]] == '(' {
			continue // method call, not a field
		}
		dots := strings.Count(tok, ".")
		if dots > bestDots {
			best = tok
			bestDots = dots
		}
	}
	return best
}
