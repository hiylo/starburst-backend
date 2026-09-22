package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// 上传落盘端点：把 App/Web 上传的文件写进 OpenCode 会话工作目录的 uploads/ 子目录，
// 使其成为「工作区里的真实文件」——文本文件可被 OpenCode 的 edit 工具直接修改，
// 二进制文档（ppt/docx/xlsx）可被 internal/doc 解析成骨架后再重渲染
// （docs/DOCUMENTS.md §7）。没有后端时 App 仍走直连（base64 内联附件），本端点是
// 可选增强通道，不替代直连。
//
//	POST /api/opencode/upload   multipart/form-data
//	  file      上传的字节（必填）
//	  directory 目标会话工作目录（绝对路径，必填，走 validateWorkDirectory 校验）
//	  name      文件名覆盖（可选，缺省用上传文件名）
//	→ {ok, name, path:"uploads/<name>", absolutePath, size}
//
// path 是相对工作目录的路径，客户端可据此在消息里引用（@-mention / file part），
// OpenCode 即可在工作区内打开、编辑该文件。

// maxUploadRequestBytes 是 multipart 请求体上限（含表单开销），文件本体上限见下。
const maxUploadRequestBytes = 16 << 20

// maxUploadFileBytes 是单个上传文件的上限（与 App 端 10MB 附件限制一致）。
const maxUploadFileBytes = 10 << 20

// handleUploadFile writes an uploaded file into <directory>/uploads/.
func (s *Server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequestBytes)
	if err := r.ParseMultipartForm(maxUploadRequestBytes); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "upload too large")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid multipart form")
		return
	}

	dir := strings.TrimSpace(r.FormValue("directory"))
	if dir == "" {
		writeErr(w, http.StatusBadRequest, "directory is required")
		return
	}
	if err := validateWorkDirectory(dir); err != nil {
		writeErr(w, http.StatusForbidden, "directory out of scope: "+err.Error())
		return
	}
	if !filepath.IsAbs(dir) {
		writeErr(w, http.StatusBadRequest, "directory must be an absolute path")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "file is required")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxUploadFileBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read upload failed: "+err.Error())
		return
	}
	if len(data) > maxUploadFileBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "file exceeds 10 MiB")
		return
	}

	name := sanitizeUploadName(r.FormValue("name"))
	if name == "" {
		name = sanitizeUploadName(header.Filename)
	}
	if name == "" {
		writeErr(w, http.StatusBadRequest, "file name is required")
		return
	}

	absPath, relPath, err := writeWorkspaceUpload(dir, name, data)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "write upload failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"name":         filepath.Base(absPath),
		"path":         relPath,
		"absolutePath": absPath,
		"size":         len(data),
	})
}

// sanitizeUploadName reduces a client-supplied name to a single safe path element
// (no separators, no traversal, no control characters).
func sanitizeUploadName(name string) string {
	name = strings.TrimSpace(name)
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r < 0x20:
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ".")
	if name == "" || name == "." || name == ".." {
		return ""
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

// writeWorkspaceUpload writes data to <dir>/uploads/<name>, keeping the target
// inside dir (symlink-resolved) and picking a non-clobbering name if it exists.
// It returns the absolute path and the workspace-relative path (forward slashes).
func writeWorkspaceUpload(dir, name string, data []byte) (absPath, relPath string, err error) {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		// 目录可能尚未创建；退回清理后的绝对路径。
		realDir = filepath.Clean(dir)
	}
	uploadsDir := filepath.Join(realDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0o755); err != nil {
		return "", "", err
	}
	// 双保险：确保最终目标确实落在 uploadsDir 内（防软链接 / 拼接逃逸）。
	target := filepath.Join(uploadsDir, name)
	rel, err := filepath.Rel(uploadsDir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("unsafe upload path")
	}
	target = uniqueUploadPath(target)
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return "", "", err
	}
	return target, filepath.ToSlash(filepath.Join("uploads", filepath.Base(target))), nil
}

// uniqueUploadPath appends -1, -2 … before the extension until the path is free,
// so re-uploading the same file never clobbers the previous one.
func uniqueUploadPath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; i < 10000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return path
}
