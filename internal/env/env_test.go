package env

import (
	"runtime"
	"testing"
)

func TestDetectAndResolveShell(t *testing.T) {
	info := Detect()
	if info.OS != runtime.GOOS {
		t.Fatalf("expected OS %s, got %s", runtime.GOOS, info.OS)
	}
	if info.Arch != runtime.GOARCH {
		t.Fatalf("expected Arch %s, got %s", runtime.GOARCH, info.Arch)
	}
	if len(info.Shells) == 0 {
		t.Fatalf("expected at least 1 detected shell on host")
	}
	if info.DefaultShell == "" {
		t.Fatalf("expected non-empty DefaultShell")
	}

	def, err := info.ResolveShell("")
	if err != nil {
		t.Fatalf("ResolveShell('') failed: %v", err)
	}
	if def.Path == "" {
		t.Fatalf("expected non-empty path for default shell")
	}

	if _, err := info.ResolveShell("nonexistent-shell-xyz"); err == nil {
		t.Fatalf("expected error resolving nonexistent shell")
	}
}

func TestResolveSafeSubpath(t *testing.T) {
	root := t.TempDir()

	p1, err := ResolveSafeSubpath(root, ".")
	if err != nil || p1 != root {
		t.Fatalf("expected %q, got %q (err=%v)", root, p1, err)
	}

	if _, err := ResolveSafeSubpath(root, "../outside"); err == nil {
		t.Fatalf("expected error for '../outside' escape")
	}

	if _, err := ResolveSafeSubpath(root, "/etc/passwd"); err == nil {
		t.Fatalf("expected error for '/etc/passwd' absolute path")
	}
}
