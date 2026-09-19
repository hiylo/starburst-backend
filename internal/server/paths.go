package server

// 客户端给出的目录入参会被服务端「走进去」：intel 的 localPath 之后被 walk +
// ReadFile 并回传内容，任务/规则/工作流的 directory 则是 OpenCode agent（kind=git
// 的规则还有本机 `git -C`）的工作目录。这里给两类入参共用一套最小约束。
//
// 刻意不做白名单根目录：被测/要改的仓库本就散落在用户磁盘上，白名单需要额外的
// 部署配置，且这层是编排器而非沙箱——挡不住的方向不该靠路径规则挡。这里只挡住
// 两类明确越界：系统目录（含软链接逃逸），以及 `..` 越出当前目录的相对写法。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// systemPathRoots 及其子树不该成为任何「由后端切进去操作」的目录。不含 /run
// /tmp /var /home /usr：这些目录下确实有人放仓库（如 systemd 用户会话的临时目录）。
var systemPathRoots = []string{"/boot", "/dev", "/etc", "/proc", "/root", "/sys"}

// resolveSystemScopedPath 归一化路径、按软链接解析后的真实位置做系统目录判定。
// 解析失败（路径不存在）时保持原值，是否再 Stat 由调用方决定。返回值是**解析后**
// 的位置，仅用于判定；落库仍应使用调用方给出的原始路径。
func resolveSystemScopedPath(raw string) (string, error) {
	p := filepath.Clean(strings.TrimSpace(raw))
	if p == "" {
		return "", errors.New("路径为空")
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = filepath.Clean(real)
	}
	if !filepath.IsAbs(p) {
		if p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("路径不能越出当前目录: %s", p)
		}
		return p, nil
	}
	if p == "/" {
		return "", errors.New("路径不能是文件系统根目录")
	}
	for _, root := range systemPathRoots {
		if p == root || strings.HasPrefix(p, root+string(filepath.Separator)) {
			return "", fmt.Errorf("路径不能位于系统目录 %s 下: %s", root, p)
		}
	}
	return p, nil
}

// validateWorkDirectory 校验任务/规则/工作流的 directory：允许留空（用默认目录）
// 与相对路径（由上游解析），但系统目录与 `..` 越界一律拒绝。报错原样回给调用方。
// 顶层单段目录（`/w`、`/data` 之类）这里不拦：挂载盘当工作目录是常见用法。
func validateWorkDirectory(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	_, err := resolveSystemScopedPath(raw)
	return err
}

// validateIntelLocalPath 额外要求绝对路径 + 真实存在的目录 + 不是整棵顶层目录：
// 本地源码目录一旦确定，整棵子树都会被扫描并把内容回传，`/home`、`/tmp` 这种
// 一段式路径不可能是某个仓库。
func validateIntelLocalPath(raw string) error {
	p := filepath.Clean(strings.TrimSpace(raw))
	if !filepath.IsAbs(p) {
		return fmt.Errorf("源码目录必须是绝对路径: %s", p)
	}
	resolved, err := resolveSystemScopedPath(p)
	if err != nil {
		return err
	}
	if !strings.Contains(strings.TrimPrefix(resolved, "/"), string(filepath.Separator)) {
		return fmt.Errorf("源码目录不能是系统一级目录: %s", resolved)
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.IsDir() {
		return fmt.Errorf("源码目录不存在或不是目录: %s", p)
	}
	return nil
}
