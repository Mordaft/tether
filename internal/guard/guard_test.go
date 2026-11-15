package guard

import (
	"context"
	"net/http"
	"testing"
	"time"

	"tether/internal/bus"
)

func TestGuard_RejectsPathTraversalAndEscapes(t *testing.T) {
	g := NewGuard(bus.NewEventBus())

	tests := []struct {
		name          string
		relCWD        string
		command       string
		wantViolation ViolationCode
	}{
		// 1. Direct and nested `..` path traversal
		{
			name:          "direct parent cat",
			relCWD:        ".",
			command:       "cat ../secret.txt",
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "multi-level parent ls",
			relCWD:        ".",
			command:       "ls ../../etc/passwd",
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "dot-slash-dot-dot traversal",
			relCWD:        ".",
			command:       "cat ./../secret.txt",
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "subdir then dot-dot escape",
			relCWD:        ".",
			command:       "cat src/../../outside.txt",
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "obfuscated mid-path dot-dot",
			relCWD:        ".",
			command:       "cat src/../README.md",
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "windows backslash dot-dot unquoted",
			relCWD:        ".",
			command:       `type ..\secret.txt`,
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "windows backslash dot-dot double-quoted",
			relCWD:        ".",
			command:       `type "..\secret.txt"`,
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "concatenated quotes dot-dot trick",
			relCWD:        ".",
			command:       `cat ".""."/secret.txt`,
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "flag equals dot-dot",
			relCWD:        ".",
			command:       "tar --file=../escape.tar -cf src",
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "short flag attached dot-dot",
			relCWD:        ".",
			command:       "gcc -L../outside main.c",
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "output redirection dot-dot",
			relCWD:        ".",
			command:       "echo pwned > ../outside.txt",
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "input redirection dot-dot",
			relCWD:        ".",
			command:       "cat < ../../outside.txt",
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "command executable itself uses dot-dot",
			relCWD:        ".",
			command:       "../run_evil.sh",
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "non-cd command with dot-dot even inside subdir",
			relCWD:        "internal/guard",
			command:       "cat ../auth/auth.go",
			wantViolation: ViolationPathTraversal,
		},

		// 2. Absolute paths (Unix, Windows Drive, UNC, file://)
		{
			name:          "unix absolute path arg",
			relCWD:        ".",
			command:       "cat /etc/passwd",
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "unix root ls",
			relCWD:        ".",
			command:       "ls /",
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "unix absolute binary path",
			relCWD:        ".",
			command:       "/bin/sh -i",
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "windows C drive backslash path",
			relCWD:        ".",
			command:       `type C:\Windows\System32\drivers\etc\hosts`,
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "windows drive forward slash path",
			relCWD:        ".",
			command:       "cat C:/Users/alvar/.gitconfig",
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "windows drive letter only",
			relCWD:        ".",
			command:       "cd D:",
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "windows leading backslash root",
			relCWD:        ".",
			command:       `dir \Windows\System32`,
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "windows UNC network share",
			relCWD:        ".",
			command:       `dir "\\fileserver\share\secrets"`,
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "file URI scheme",
			relCWD:        ".",
			command:       "curl file:///etc/passwd",
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "output redirection to unix absolute path",
			relCWD:        ".",
			command:       "echo pwn > /tmp/exploit.sh",
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "dd with absolute path in key=value arg",
			relCWD:        ".",
			command:       "dd if=/dev/zero of=local.bin count=1",
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "short flag attached unix absolute path",
			relCWD:        ".",
			command:       "gcc -I/usr/include main.c",
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "variable assignment with absolute path",
			relCWD:        ".",
			command:       "TARGET=/etc/shadow cat",
			wantViolation: ViolationAbsolutePath,
		},

		// 3. Home expansion & environment variable escapes
		{
			name:          "tilde alone",
			relCWD:        ".",
			command:       "ls ~",
			wantViolation: ViolationHomeExpansion,
		},
		{
			name:          "tilde slash path",
			relCWD:        ".",
			command:       "cat ~/.ssh/id_rsa",
			wantViolation: ViolationHomeExpansion,
		},
		{
			name:          "tilde user path",
			relCWD:        ".",
			command:       "ls ~root/.bashrc",
			wantViolation: ViolationHomeExpansion,
		},
		{
			name:          "bare cd jumps to HOME",
			relCWD:        "internal",
			command:       "cd",
			wantViolation: ViolationHomeExpansion,
		},
		{
			name:          "dollar HOME unquoted",
			relCWD:        ".",
			command:       "cat $HOME/.bashrc",
			wantViolation: ViolationHomeExpansion,
		},
		{
			name:          "dollar braces HOME in double quotes",
			relCWD:        ".",
			command:       `cat "${HOME}/secret.txt"`,
			wantViolation: ViolationHomeExpansion,
		},
		{
			name:          "windows USERPROFILE env var",
			relCWD:        ".",
			command:       `cat "$USERPROFILE/Desktop/keys.txt"`,
			wantViolation: ViolationHomeExpansion,
		},
		{
			name:          "windows percent USERPROFILE syntax",
			relCWD:        ".",
			command:       `type %USERPROFILE%\secret.txt`,
			wantViolation: ViolationHomeExpansion,
		},
		{
			name:          "cd dash OLDPWD escape",
			relCWD:        "internal",
			command:       "cd -",
			wantViolation: ViolationCWDEscape,
		},
		{
			name:          "popd directory stack escape",
			relCWD:        "internal",
			command:       "popd",
			wantViolation: ViolationCWDEscape,
		},
		{
			name:          "OLDPWD env expansion",
			relCWD:        ".",
			command:       "ls $OLDPWD",
			wantViolation: ViolationEnvEscape,
		},
		{
			name:          "LD_PRELOAD env injection",
			relCWD:        ".",
			command:       "LD_PRELOAD=./hook.so ls",
			wantViolation: ViolationEnvEscape,
		},
		{
			name:          "indirect parameter expansion",
			relCWD:        ".",
			command:       "echo ${! hidden_var}",
			wantViolation: ViolationSyntaxError, // or ViolationEnvEscape for valid ${!var}
		},
		{
			name:          "valid indirect parameter expansion rejected",
			relCWD:        ".",
			command:       "echo ${!SECRET_PATH}",
			wantViolation: ViolationEnvEscape,
		},

		// 4. Command substitution & Process substitution & Subshell smuggling
		{
			name:          "dollar paren command substitution",
			relCWD:        ".",
			command:       "echo $(whoami)",
			wantViolation: ViolationCommandSubstitution,
		},
		{
			name:          "nested command substitution in double quotes",
			relCWD:        ".",
			command:       `cat "$(pwd)/secret.txt"`,
			wantViolation: ViolationCommandSubstitution,
		},
		{
			name:          "backtick command substitution",
			relCWD:        ".",
			command:       "echo `id -u`",
			wantViolation: ViolationCommandSubstitution,
		},
		{
			name:          "process substitution",
			relCWD:        ".",
			command:       "diff <(ls dir1) <(ls dir2)",
			wantViolation: ViolationCommandSubstitution,
		},
		{
			name:          "sh -c smuggling absolute path",
			relCWD:        ".",
			command:       `sh -c "cat /etc/passwd"`,
			wantViolation: ViolationAbsolutePath,
		},
		{
			name:          "bash -c smuggling dot-dot escape",
			relCWD:        ".",
			command:       `bash -c "cd .. && ls"`,
			wantViolation: ViolationPathTraversal,
		},
		{
			name:          "eval smuggling dot-dot escape",
			relCWD:        ".",
			command:       `eval "cat ../secret.txt"`,
			wantViolation: ViolationPathTraversal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := g.Evaluate(tc.relCWD, tc.command)
			if v.Action != VerdictReject {
				t.Fatalf("expected VerdictReject for %q, got %s (reason: %s)", tc.command, v.Action, v.Reason)
			}
			if v.HTTPStatus != http.StatusForbidden {
				t.Fatalf("expected HTTP 403 Forbidden for %q, got %d", tc.command, v.HTTPStatus)
			}
			if v.Violation != tc.wantViolation {
				t.Fatalf("expected violation %s for %q, got %s (reason: %s)", tc.wantViolation, tc.command, v.Violation, v.Reason)
			}
		})
	}
}

func TestGuard_CWDDepthTracking(t *testing.T) {
	g := NewGuard(bus.NewEventBus())

	// 1. At root "." (depth 0), `cd ..` must be rejected immediately (403 Forbidden).
	v0 := g.Evaluate(".", "cd ..")
	if v0.Action != VerdictReject || v0.HTTPStatus != http.StatusForbidden {
		t.Fatalf("expected 403 Reject for 'cd ..' at root, got %+v", v0)
	}

	// 2. Navigate down into "internal/guard" (depth 2).
	v1 := g.Evaluate(".", "cd internal/guard")
	if v1.Action != VerdictAllow || v1.NextRelCWD != "internal/guard" {
		t.Fatalf("expected Allow with NextRelCWD='internal/guard', got %+v", v1)
	}

	// 3. From "internal/guard" (depth 2), `cd ..` moves to "internal" (depth 1).
	v2 := g.Evaluate("internal/guard", "cd ..")
	if v2.Action != VerdictAllow || v2.NextRelCWD != "internal" {
		t.Fatalf("expected Allow with NextRelCWD='internal', got %+v", v2)
	}

	// 4. From "internal/guard" (depth 2), `cd ../..` moves back to "." (depth 0).
	v3 := g.Evaluate("internal/guard", "cd ../..")
	if v3.Action != VerdictAllow || v3.NextRelCWD != "." {
		t.Fatalf("expected Allow with NextRelCWD='.', got %+v", v3)
	}

	// 5. From "internal/guard" (depth 2), `cd ../../..` escapes root (depth -1) -> Reject!
	v4 := g.Evaluate("internal/guard", "cd ../../..")
	if v4.Action != VerdictReject || v4.HTTPStatus != http.StatusForbidden || v4.Violation != ViolationPathTraversal {
		t.Fatalf("expected 403 ViolationPathTraversal for 'cd ../../..' from depth 2, got %+v", v4)
	}

	// 6. Chained cd within a single command: `cd a && cd ../..` from "." -> net depth -1 -> Reject!
	v5 := g.Evaluate(".", "cd a && cd ../..")
	if v5.Action != VerdictReject || v5.HTTPStatus != http.StatusForbidden {
		t.Fatalf("expected 403 Reject for 'cd a && cd ../..' from root, got %+v", v5)
	}

	// 7. Chained cd staying inside workspace: `cd internal/guard && cd .. && cd auth` -> NextRelCWD = "internal/auth".
	v6 := g.Evaluate(".", "cd internal/guard && cd .. && cd auth")
	if v6.Action != VerdictAllow || v6.NextRelCWD != "internal/auth" {
		t.Fatalf("expected Allow with NextRelCWD='internal/auth', got %+v", v6)
	}

	// 8. Subshell spoofing attempt: `(cd a) && cd ..` from "." must fail because `(cd a)` does not change parent CWD!
	v7 := g.Evaluate(".", "(cd a) && cd ..")
	if v7.Action != VerdictReject || v7.HTTPStatus != http.StatusForbidden {
		t.Fatalf("expected 403 Reject for subshell CWD spoofing '(cd a) && cd ..', got %+v", v7)
	}
}

func TestGuard_ReadOnlyAllowlistAndConsentQueue(t *testing.T) {
	eb := bus.NewEventBus()
	eventsCh, unsub := eb.Subscribe(16)
	defer unsub()

	g := NewGuard(eb)

	// 1. Safe read-only commands should return VerdictAllow immediately.
	safeCmds := []string{
		"ls -la",
		"ls internal/guard",
		"cat README.md",
		"pwd",
		"git status",
		"git diff --stat",
		"go version",
		"go list ./...",
	}
	for _, cmd := range safeCmds {
		v := g.Evaluate(".", cmd)
		if v.Action != VerdictAllow {
			t.Fatalf("expected VerdictAllow for safe read-only command %q, got %+v", cmd, v)
		}
	}

	// 2. Read-only command with file write redirection requires human consent.
	vRedir := g.Evaluate(".", "echo hello > output.txt")
	if vRedir.Action != VerdictNeedsConsent {
		t.Fatalf("expected VerdictNeedsConsent for write redirection, got %+v", vRedir)
	}

	// 3. Mutating / non-allowlisted commands require human consent.
	consentCmds := []string{
		"rm -rf dist",
		"mkdir -p build/bin",
		"go test ./...",
		"git commit -m 'initial'",
		"./scripts/deploy.sh",
	}
	for _, cmd := range consentCmds {
		v := g.Evaluate(".", cmd)
		if v.Action != VerdictNeedsConsent {
			t.Fatalf("expected VerdictNeedsConsent for %q, got %+v", cmd, v)
		}
	}

	// 4. Test Authorize blocking on EventBus and "Allow Session" policy.
	now := time.Now()
	agentSess := bus.NewAgentSession("tok-1", "coder-01", "Coder", "/workspace", "raw-tok", now, now.Add(30*time.Minute))

	go func() {
		for evt := range eventsCh {
			if evt.Type == bus.EventConsentQueued && evt.ConsentRequest.Kind == bus.ConsentKindCommand {
				// Human selects [S]esión (Permitir en esta sesión)
				_ = eb.ResolveConsent(evt.ConsentRequest.ID, bus.DecisionAllowSession, "approved for session")
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	verdict, err := g.Authorize(ctx, agentSess, "sess-1", ".", "go test ./...")
	if err != nil {
		t.Fatalf("Authorize failed: %v", err)
	}
	if !verdict.Allowed() {
		t.Fatalf("expected VerdictAllow after human approval, got %+v", verdict)
	}

	// Subsequent safe non-allowlisted command in the same session should be auto-approved without blocking!
	verdict2, err := g.Authorize(ctx, agentSess, "sess-1", ".", "go build ./...")
	if err != nil {
		t.Fatalf("second Authorize failed: %v", err)
	}
	if !verdict2.Allowed() {
		t.Fatalf("expected auto-approval on second command after DecisionAllowSession, got %+v", verdict2)
	}

	// Even with session consent granted, path traversal MUST still be rejected with 403 Forbidden!
	verdictEvil, err := g.Authorize(ctx, agentSess, "sess-1", ".", "cat ../secrets.env")
	if err != nil {
		t.Fatalf("Authorize returned unexpected error: %v", err)
	}
	if verdictEvil.Action != VerdictReject || verdictEvil.HTTPStatus != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for path traversal even with session consent, got %+v", verdictEvil)
	}
}
