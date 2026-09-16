// Package fix implements the fix-application module of the Test Intelligence
// subsystem. It renders unified diff patch drafts for fix suggestions and
// applies a single suggestion in memory for dry-run verification. The system
// never writes files itself: these helpers only produce a patch draft (or the
// resulting content) so a user can review and approve before applying.
package fix

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Suggestion is a fix suggestion (a patch draft) for one location of one file.
type Suggestion struct {
	File       string `json:"file"`       // path relative to the repository root
	OldText    string `json:"oldText"`    // exact original text to be replaced
	NewText    string `json:"newText"`    // replacement content
	Line       int    `json:"line"`       // starting line number (1-based)
	Confidence string `json:"confidence"` // high | medium | low
}

// hunkContext is the number of unchanged context lines emitted around a change.
const hunkContext = 3

// edit is one resolved line-level change inside a file.
type edit struct {
	oldStart      int      // 1-based line where the change begins (insertion point for inserts)
	oldLines      []string // original lines being replaced (empty for a pure insertion)
	newLines      []string // replacement lines (empty for a pure deletion)
	newTrailingNL bool     // whether the replacement text ends with a newline
}

// hunkResult carries a rendered hunk plus its line-count delta on the new side.
type hunkResult struct {
	text  string
	delta int
}

// GeneratePatch renders a unified diff for the given suggestions. content maps
// each file path to its current complete content, used to compute exact hunk
// line numbers and context. Multiple suggestions for the same file are merged
// into a single file diff with hunks in ascending line order. The result is a
// patch draft only; nothing is written to disk.
func GeneratePatch(content map[string]string, suggs []*Suggestion) (string, error) {
	byFile := make(map[string][]*Suggestion)
	for _, s := range suggs {
		if s == nil || s.File == "" {
			continue
		}
		byFile[s.File] = append(byFile[s.File], s)
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	var b strings.Builder
	for _, f := range files {
		orig, ok := content[f]
		if !ok {
			return "", fmt.Errorf("content for file %q not provided", f)
		}
		list := byFile[f]
		sort.SliceStable(list, func(i, j int) bool {
			a, c := list[i], list[j]
			if a.Line != c.Line {
				return a.Line < c.Line
			}
			if a.OldText != c.OldText {
				return a.OldText < c.OldText
			}
			return a.NewText < c.NewText
		})

		edits := make([]edit, 0, len(list))
		for _, s := range list {
			if s.OldText == s.NewText {
				continue
			}
			e, err := buildEdit(orig, s)
			if err != nil {
				return "", err
			}
			edits = append(edits, e)
		}
		b.WriteString(renderFileDiff(f, orig, edits))
	}
	return b.String(), nil
}

// ApplyDryRun applies one suggestion to content in memory and returns the new
// content. It performs an exact match of OldText and returns an error instead of
// silently replacing when the text is absent or appears more than once.
func ApplyDryRun(content string, s *Suggestion) (string, error) {
	if s == nil {
		return "", errors.New("nil suggestion")
	}
	if s.OldText == "" {
		if s.Line < 1 {
			return "", errors.New("insertion requires a line number")
		}
		off := lineStartOffset(content, s.Line)
		return content[:off] + s.NewText + content[off:], nil
	}
	n := strings.Count(content, s.OldText)
	if n == 0 {
		return "", fmt.Errorf("old text not found in %s", s.File)
	}
	if n > 1 {
		return "", fmt.Errorf("old text in %s is ambiguous: %d occurrences", s.File, n)
	}
	return strings.Replace(content, s.OldText, s.NewText, 1), nil
}

// BackupPlan returns the deduplicated, sorted list of files that should be
// backed up before applying the suggestions. Each file is listed once even when
// several suggestions touch it.
func BackupPlan(suggs []*Suggestion) []string {
	seen := make(map[string]struct{})
	for _, s := range suggs {
		if s != nil && s.File != "" {
			seen[s.File] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// buildEdit resolves a suggestion into a line-level edit using byte-level exact
// matching against the original content.
func buildEdit(orig string, s *Suggestion) (edit, error) {
	var e edit
	startLine, off, err := locate(orig, s)
	if err != nil {
		return e, err
	}
	e.oldStart = startLine
	e.newTrailingNL = strings.HasSuffix(s.NewText, "\n")

	if s.OldText == "" {
		e.newLines = splitFileLines(s.NewText)
		return e, nil
	}

	endOff := off + len(s.OldText)
	oldEnd := lineAt(orig, endOff-1)
	e.oldLines, _ = extractLines(orig, startLine, oldEnd)

	if s.NewText == "" {
		return e, nil
	}
	newFull := orig[:off] + s.NewText + orig[endOff:]
	newEnd := lineAt(newFull, off+len(s.NewText)-1)
	e.newLines, _ = extractLines(newFull, startLine, newEnd)
	return e, nil
}

// renderFileDiff renders the headers and hunks for a single file.
func renderFileDiff(path, orig string, edits []edit) string {
	sort.Slice(edits, func(i, j int) bool { return edits[i].oldStart < edits[j].oldStart })
	oldLines := splitFileLines(orig)
	n := len(oldLines)
	origFinalNL := strings.HasSuffix(orig, "\n")
	newFinalNL := newFileFinalNL(origFinalNL, edits, n)

	var b strings.Builder
	b.WriteString("diff --git a/" + path + " b/" + path + "\n")
	b.WriteString("--- a/" + path + "\n")
	b.WriteString("+++ b/" + path + "\n")

	if n == 0 {
		var added []string
		for _, e := range edits {
			added = append(added, e.newLines...)
		}
		b.WriteString(fmt.Sprintf("@@ -0,0 +1,%d @@\n", len(added)))
		for _, l := range added {
			b.WriteString("+" + l + "\n")
		}
		if len(added) > 0 && !newFinalNL {
			b.WriteString("\\ No newline at end of file\n")
		}
		return b.String()
	}

	offset := 0
	for _, g := range groupEdits(edits) {
		h := renderHunk(oldLines, g, offset, n, origFinalNL, newFinalNL)
		b.WriteString(h.text)
		offset += h.delta
	}
	return b.String()
}

// groupEdits merges edits that sit close enough for their context to overlap,
// so that adjacent hunks are collapsed into a single valid hunk.
func groupEdits(edits []edit) [][]edit {
	var groups [][]edit
	for _, e := range edits {
		if len(groups) == 0 {
			groups = append(groups, []edit{e})
			continue
		}
		last := groups[len(groups)-1]
		if e.oldStart-editEnd(last[len(last)-1]) <= 2*hunkContext {
			groups[len(groups)-1] = append(last, e)
		} else {
			groups = append(groups, []edit{e})
		}
	}
	return groups
}

// editEnd returns the highest original line covered by an edit; a pure
// insertion covers nothing and is anchored just before oldStart.
func editEnd(e edit) int {
	if len(e.oldLines) == 0 {
		return e.oldStart - 1
	}
	return e.oldStart + len(e.oldLines) - 1
}

// renderHunk renders a single hunk with hunkContext lines of context.
func renderHunk(oldLines []string, g []edit, offset, n int, origFinalNL, newFinalNL bool) hunkResult {
	lo := g[0].oldStart
	hi := editEnd(g[0])
	for _, e := range g[1:] {
		if e.oldStart < lo {
			lo = e.oldStart
		}
		if end := editEnd(e); end > hi {
			hi = end
		}
	}
	hLo := lo - hunkContext
	if hLo < 1 {
		hLo = 1
	}
	hHi := hi + hunkContext
	if hHi > n {
		hHi = n
	}

	var body []string
	removed, added := 0, 0
	gi := 0
	for L := hLo; L <= hHi; {
		if gi < len(g) {
			e := g[gi]
			if e.oldStart == L && len(e.oldLines) > 0 {
				for _, ol := range e.oldLines {
					body = append(body, "-"+ol)
					removed++
				}
				for _, nl := range e.newLines {
					body = append(body, "+"+nl)
					added++
				}
				L += len(e.oldLines)
				gi++
				continue
			}
			if e.oldStart == L && len(e.oldLines) == 0 {
				for _, nl := range e.newLines {
					body = append(body, "+"+nl)
					added++
				}
				gi++
			}
		}
		if L <= hHi {
			body = append(body, " "+oldLines[L-1])
			L++
		}
	}

	oldCount := hHi - hLo + 1
	newCount := oldCount - removed + added
	body = addNoNewlineMarkers(body, hHi == n, origFinalNL, newFinalNL)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", hLo, oldCount, hLo+offset, newCount))
	for _, line := range body {
		sb.WriteString(line + "\n")
	}
	return hunkResult{text: sb.String(), delta: newCount - oldCount}
}

// addNoNewlineMarkers inserts the "\ No newline at end of file" marker after
// the last old-side and/or new-side line when the hunk reaches the end of the
// file and that side does not end with a newline.
func addNoNewlineMarkers(body []string, atEOF, origFinalNL, newFinalNL bool) []string {
	if !atEOF {
		return body
	}
	lastOld, lastNew := -1, -1
	for i, line := range body {
		if line == "" {
			continue
		}
		if line[0] == '+' {
			lastNew = i
		} else {
			lastOld = i
		}
	}
	type ins struct {
		idx int
		s   string
	}
	var insns []ins
	if !origFinalNL && lastOld >= 0 {
		insns = append(insns, ins{lastOld + 1, "\\ No newline at end of file"})
	}
	if !newFinalNL && lastNew >= 0 {
		insns = append(insns, ins{lastNew + 1, "\\ No newline at end of file"})
	}
	sort.Slice(insns, func(i, j int) bool { return insns[i].idx > insns[j].idx })
	for _, in := range insns {
		body = append(body[:in.idx], append([]string{in.s}, body[in.idx:]...)...)
	}
	return body
}

// newFileFinalNL derives whether the reconstructed new file ends with a newline
// from the original status and the edit that (if any) touches the file end.
func newFileFinalNL(origFinalNL bool, edits []edit, n int) bool {
	res := origFinalNL
	for _, e := range edits {
		if len(e.oldLines) == 0 {
			if e.oldStart > n {
				res = e.newTrailingNL
			}
			continue
		}
		if e.oldStart+len(e.oldLines)-1 < n {
			continue
		}
		if len(e.newLines) == 0 {
			res = e.oldStart > 1
		} else {
			res = e.newTrailingNL
		}
	}
	return res
}

// locate resolves the 1-based starting line and byte offset of s.OldText inside
// content. An empty OldText is a pure insertion at s.Line.
func locate(content string, s *Suggestion) (line, off int, err error) {
	if s.OldText == "" {
		if s.Line < 1 {
			return 0, 0, errors.New("insertion requires a line number")
		}
		return s.Line, lineStartOffset(content, s.Line), nil
	}
	count := strings.Count(content, s.OldText)
	switch {
	case count == 0:
		return 0, 0, fmt.Errorf("old text not found in %s", s.File)
	case count == 1:
		idx := strings.Index(content, s.OldText)
		return lineAt(content, idx), idx, nil
	default:
		if s.Line < 1 {
			return 0, 0, fmt.Errorf("old text in %s is ambiguous: %d occurrences", s.File, count)
		}
		from := 0
		for {
			rel := strings.Index(content[from:], s.OldText)
			if rel < 0 {
				break
			}
			idx := from + rel
			if lineAt(content, idx) == s.Line {
				return s.Line, idx, nil
			}
			from = idx + 1
		}
		return 0, 0, fmt.Errorf("old text not found at line %d in %s", s.Line, s.File)
	}
}

// lineAt returns the 1-based line number of byte offset off in content.
func lineAt(content string, off int) int {
	return 1 + strings.Count(content[:off], "\n")
}

// lineStartOffset returns the byte offset of the start of the 1-based line.
func lineStartOffset(content string, line int) int {
	if line <= 1 {
		return 0
	}
	pos := 0
	for i := 1; i < line; i++ {
		idx := strings.IndexByte(content[pos:], '\n')
		if idx < 0 {
			return len(content)
		}
		pos += idx + 1
	}
	return pos
}

// splitFileLines splits content into its lines, dropping the line terminator and
// the trailing empty element that a final newline would otherwise produce.
func splitFileLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// extractLines returns the content of lines startLine..endLine (1-based,
// inclusive) without their line terminators.
func extractLines(content string, startLine, endLine int) ([]string, bool) {
	if startLine < 1 || endLine < startLine {
		return nil, false
	}
	all := splitFileLines(content)
	if startLine > len(all) {
		return nil, false
	}
	if endLine > len(all) {
		endLine = len(all)
	}
	lines := append([]string(nil), all[startLine-1:endLine]...)
	terminated := endLine < len(all) || (endLine == len(all) && strings.HasSuffix(content, "\n"))
	return lines, terminated
}
