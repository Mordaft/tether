package env

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ShellInfo describes a shell interpreter discovered on the host system.
type ShellInfo struct {
	Name string   `json:"name"`
	Path string   `json:"path"`
	Args []string `json:"args,omitempty"`
}

// SystemInfo captures the host OS, CPU architecture, and available command interpreters.
type SystemInfo struct {
	OS           string      `json:"os"`
	Arch         string      `json:"arch"`
	DefaultShell string      `json:"default_shell"`
	Shells       []ShellInfo `json:"shells"`
}

// Detect inspect the current host environment (GOOS, GOARCH) and probes PATH and
// standard installation directories for available shells.
func Detect() SystemInfo {
	info := SystemInfo{
		OS:     runtime.GOOS,
		Arch:   runtime.GOARCH,
		Shells: make([]ShellInfo, 0),
	}

	seenPaths := make(map[string]bool)
	addShell := func(name, binPath string, args ...string) {
		if binPath == "" {
			return
		}
		clean := filepath.Clean(binPath)
		key := strings.ToLower(clean)
		if seenPaths[key] {
			return
		}
		seenPaths[key] = true
		info.Shells = append(info.Shells, ShellInfo{
			Name: name,
			Path: clean,
			Args: args,
		})
	}

	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("pwsh.exe"); err == nil {
			addShell("pwsh", p, "-NoLogo")
		} else if p, err := exec.LookPath("pwsh"); err == nil {
			addShell("pwsh", p, "-NoLogo")
		}

		if p, err := exec.LookPath("powershell.exe"); err == nil {
			addShell("powershell.exe", p, "-NoLogo")
		}

		if p, err := exec.LookPath("cmd.exe"); err == nil {
			addShell("cmd.exe", p)
		}

		if gitBash := detectWindowsGitBash(); gitBash != "" {
			addShell("git-bash", gitBash, "--login", "-i")
		}

		if p, err := exec.LookPath("bash.exe"); err == nil {
			// Avoid the Windows System32 WSL launcher stub if Git Bash or another real bash is used
			if !strings.Contains(strings.ToLower(p), `\windows\system32\bash.exe`) {
				addShell("bash", p)
			}
		}
	} else {
		candidates := []string{"zsh", "bash", "sh", "pwsh"}
		for _, c := range candidates {
			if p, err := exec.LookPath(c); err == nil {
				addShell(c, p)
			}
		}
	}

	// Determine DefaultShell
	if runtime.GOOS != "windows" {
		if envShell := os.Getenv("SHELL"); envShell != "" {
			base := filepath.Base(envShell)
			for _, s := range info.Shells {
				if s.Name == base {
					info.DefaultShell = s.Name
					break
				}
			}
		}
	}
	if info.DefaultShell == "" && len(info.Shells) > 0 {
		info.DefaultShell = info.Shells[0].Name
	}

	return info
}

func detectWindowsGitBash() string {
	candidates := []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files\Git\usr\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
	}
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		candidates = append(candidates, filepath.Join(localAppData, "Programs", "Git", "bin", "bash.exe"))
	}
	if progFiles := os.Getenv("ProgramFiles"); progFiles != "" {
		candidates = append(candidates, filepath.Join(progFiles, "Git", "bin", "bash.exe"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// ResolveShell matches a requested shell identifier (or empty string for default)
// against the detected host shells.
func (s SystemInfo) ResolveShell(requested string) (ShellInfo, error) {
	if len(s.Shells) == 0 {
		return ShellInfo{}, errors.New("no supported shells detected on host")
	}

	req := strings.TrimSpace(strings.ToLower(requested))
	if req == "" {
		for _, sh := range s.Shells {
			if strings.EqualFold(sh.Name, s.DefaultShell) {
				return sh, nil
			}
		}
		return s.Shells[0], nil
	}

	// Normalize aliases (e.g. "powershell" <-> "powershell.exe", "cmd" <-> "cmd.exe", "bash" <-> "git-bash")
	for _, sh := range s.Shells {
		nameLower := strings.ToLower(sh.Name)
		baseLower := strings.ToLower(filepath.Base(sh.Path))
		baseNoExt := strings.TrimSuffix(baseLower, ".exe")
		reqNoExt := strings.TrimSuffix(req, ".exe")

		if req == nameLower || req == baseLower || reqNoExt == baseNoExt || reqNoExt == strings.TrimSuffix(nameLower, ".exe") {
			return sh, nil
		}
		if (req == "bash" || req == "sh") && nameLower == "git-bash" {
			return sh, nil
		}
	}

	return ShellInfo{}, fmt.Errorf("requested shell %q is not installed or available on host", requested)
}

// ResolveSafeSubpath joins workspaceRoot and relPath and verifies that the resulting
// canonical path remains strictly inside workspaceRoot.
func ResolveSafeSubpath(workspaceRoot string, relPath string) (string, error) {
	wsAbs, err := filepath.Abs(filepath.Clean(workspaceRoot))
	if err != nil {
		return "", fmt.Errorf("resolving workspace root: %w", err)
	}

	if strings.TrimSpace(relPath) == "" || relPath == "." {
		return wsAbs, nil
	}

	if filepath.IsAbs(relPath) || strings.HasPrefix(relPath, "/") || strings.HasPrefix(relPath, `\`) {
		return "", fmt.Errorf("path %q must be relative to workspace root", relPath)
	}

	targetAbs, err := filepath.Abs(filepath.Join(wsAbs, filepath.FromSlash(relPath)))
	if err != nil {
		return "", fmt.Errorf("resolving target path: %w", err)
	}

	relCheck, err := filepath.Rel(wsAbs, targetAbs)
	if err != nil {
		return "", fmt.Errorf("verifying workspace containment: %w", err)
	}

	if relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace root %q", relPath, wsAbs)
	}

	return targetAbs, nil
}
