// Package ios deterministically extracts SwiftUI view data bindings (page ->
// data field path) so the test-intelligence system can build the client's
// "must-display field list" for iPhone/iPad apps, mirroring the Android and Web
// extractors. Bindings are line-anchored and carry source-file:line provenance.
//
// SwiftUI has no dedicated binding syntax, so the extractor targets display
// constructors: Text(...) arguments, string interpolations and url parameters
// of image views. Each expression is reduced to its deepest dotted identifier
// chain (model field being displayed); a leading self./viewModel./vm. root is
// stripped so the same backend field dedupes regardless of access path.
package ios

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Binding is one "page -> field" binding extracted from a SwiftUI view.
type Binding struct {
	Page      string `json:"page"`      // component path without extension (e.g. EchoApp/Views/Home/PostCardView)
	FieldPath string `json:"fieldPath"` // bound data field path (e.g. post.coverURL)
	Slot      string `json:"slot"`      // binding kind: text | interpolation | image
	Source    string `json:"source"`    // source file path + line, e.g. EchoApp/Views/Home/PostCardView.swift:23
}

var (
	reText      = regexp.MustCompile(`Text\(([^()]*)\)`)
	reImageURL  = regexp.MustCompile(`[A-Za-z]*Image\(url:\s*([^()]*)\)`)
	reStrTriple = regexp.MustCompile(`"""[\s\S]*?"""`)
	reStr       = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	reNum       = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	reChain     = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`)
)

// keywords are identifier tokens that never represent a data field.
var keywords = map[string]bool{
	"self": true, "let": true, "var": true, "nil": true, "true": true, "false": true,
	"if": true, "else": true, "return": true, "for": true, "while": true, "in": true,
	"as": true, "try": true, "await": true, "throw": true, "case": true, "switch": true,
	"some": true, "any": true, "guard": true, "where": true, "is": true, "default": true,
	"func": true, "class": true, "struct": true, "enum": true, "protocol": true,
	"extension": true, "import": true, "typealias": true, "do": true, "catch": true,
	"repeat": true, "static": true, "private": true, "public": true, "internal": true,
	"final": true, "required": true, "init": true, "deinit": true, "operator": true,
}

// viewRoots are leading identifier chains stripped from a field path (the
// access root, not the backend field being displayed).
var viewRoots = []string{"self.", "viewmodel.", "vm.", "viewmodel."}

// ExtractBindings scans every Swift file under srcDir (recursively), extracts
// the display bindings and returns them sorted by page (and, within a page, by
// source line order). Source paths are reported relative to srcDir.
func ExtractBindings(srcDir string) ([]Binding, error) {
	var out []Binding
	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == "DerivedData" || name == ".build" || name == "Pods" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".swift" {
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
		page := strings.TrimSuffix(filepath.ToSlash(rel), ".swift")
		out = append(out, extractFile(page, filepath.ToSlash(rel), string(data))...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Page < out[j].Page })
	return out, nil
}

// extractFile extracts all display bindings from a single Swift file.
func extractFile(page, sourcePath, content string) []Binding {
	out := []Binding{}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		slot := ""
		added := map[string]bool{}
		add := func(expr, s string) {
			fps := fieldPaths(expr)
			slot = s
			for _, fp := range fps {
				if fp == "" || added[fp] {
					continue
				}
				added[fp] = true
				out = append(out, Binding{
					Page:      page,
					FieldPath: fp,
					Slot:      slot,
					Source:    sourcePath + ":" + strconv.Itoa(i+1),
				})
			}
		}
		for _, m := range reText.FindAllStringSubmatch(line, -1) {
			interpolate(m[1], add)
			add(m[1], "text")
		}
		for _, m := range reImageURL.FindAllStringSubmatch(line, -1) {
			add(m[1], "image")
		}
		interpolate(line, add)
	}
	return out
}

// interpolate extracts string-interpolation expressions from s and feeds each
// to add.
func interpolate(s string, add func(expr, slot string)) {
	for _, expr := range findInterpolations(s) {
		add(expr, "interpolation")
	}
}

// findInterpolations returns the expressions inside Swift string interpolation
// markers "\(...)", tracking parenthesis depth so nested calls survive.
func findInterpolations(s string) []string {
	var out []string
	i := 0
	for i+1 < len(s) {
		if s[i] == '\\' && s[i+1] == '(' {
			depth := 0
			j := i + 1
			for ; j < len(s); j++ {
				switch s[j] {
				case '(':
					depth++
				case ')':
					depth--
					if depth == 0 {
						out = append(out, s[i+2:j])
						break
					}
				}
			}
			i = j + 1
		} else {
			i++
		}
	}
	return out
}

// fieldPaths reduces an expression to its display field paths: for each string
// then dotted identifier chain, strip the access root and return the deepest
// paths found.
func fieldPaths(expr string) []string {
	s := reStrTriple.ReplaceAllString(expr, " ")
	s = reStr.ReplaceAllString(s, " ")
	s = reNum.ReplaceAllString(s, " ")
	// Normalize Swift optional/force-unwrap syntax so chains survive "?.cover".
	s = strings.ReplaceAll(s, "?.", ".")
	s = strings.ReplaceAll(s, "!", "")
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
		tok = stripViewRoot(tok)
		dots := strings.Count(tok, ".")
		if dots > bestDots {
			best = tok
			bestDots = dots
		}
	}
	if best == "" {
		return nil
	}
	return []string{best}
}

// stripViewRoot removes a leading access root (self./viewModel./vm.) so the
// same backend field path is reported regardless of how the view reaches it.
func stripViewRoot(tok string) string {
	low := strings.ToLower(tok)
	for _, root := range viewRoots {
		if strings.HasPrefix(low, root) {
			return tok[len(root):]
		}
	}
	return tok
}
