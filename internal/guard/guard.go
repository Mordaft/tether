package guard

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"tether/internal/bus"
)

// Type and constant aliases for ergonomic usage from the guard package.
type (
	GuardVerdict  = bus.GuardVerdict
	VerdictAction = bus.VerdictAction
	ViolationCode = bus.ViolationCode
)

const (
	VerdictAllow        = bus.VerdictAllow
	VerdictNeedsConsent = bus.VerdictNeedsConsent
	VerdictReject       = bus.VerdictReject

	ViolationNone                = bus.ViolationNone
	ViolationEmptyCommand        = bus.ViolationEmptyCommand
	ViolationSyntaxError         = bus.ViolationSyntaxError
	ViolationPathTraversal       = bus.ViolationPathTraversal
	ViolationAbsolutePath        = bus.ViolationAbsolutePath
	ViolationHomeExpansion       = bus.ViolationHomeExpansion
	ViolationCommandSubstitution = bus.ViolationCommandSubstitution
	ViolationCWDEscape           = bus.ViolationCWDEscape
	ViolationEnvEscape           = bus.ViolationEnvEscape
	ViolationConsentDenied       = bus.ViolationConsentDenied
)

// DefaultReadOnlyAllowlist defines commands that are safe to run without interactive
// human confirmation as long as they contain no write redirections or path violations.
var DefaultReadOnlyAllowlist = map[string]bool{
	"ls":       true,
	"dir":      true,
	"pwd":      true,
	"echo":     true,
	"cat":      true,
	"type":     true,
	"head":     true,
	"tail":     true,
	"wc":       true,
	"grep":     true,
	"egrep":    true,
	"fgrep":    true,
	"rg":       true,
	"find":     true,
	"which":    true,
	"where":    true,
	"whoami":   true,
	"uname":    true,
	"date":     true,
	"stat":     true,
	"file":     true,
	"tree":     true,
	"diff":     true,
	"sort":     true,
	"uniq":     true,
	"cut":      true,
	"tr":       true,
	"true":     true,
	"false":    true,
	"cd":       true,
	"basename": true,
	"dirname":  true,
	"realpath": true,
}

// SafeSubcommands defines read-only subcommands for multi-purpose CLI tools.
var SafeSubcommands = map[string]map[string]bool{
	"git": {
		"status":   true,
		"log":      true,
		"diff":     true,
		"show":     true,
		"branch":   true,
		"rev-parse": true,
		"ls-files": true,
		"remote":   true,
		"tag":      true,
	},
	"go": {
		"version": true,
		"env":     true,
		"list":    true,
		"doc":     true,
		"vet":     true,
	},
}

// ForbiddenEnvVars lists environment variables that either expand to paths outside
// workspace_root or alter shell execution behavior in unsafe ways.
var ForbiddenHomeEnvVars = map[string]bool{
	"HOME":        true,
	"USERPROFILE": true,
	"HOMEDRIVE":   true,
	"HOMEPATH":    true,
}

var ForbiddenEscapeEnvVars = map[string]bool{
	"OLDPWD":                true,
	"PWD":                   true,
	"CD":                    true,
	"APPDATA":               true,
	"LOCALAPPDATA":          true,
	"ALLUSERSPROFILE":       true,
	"PROGRAMDATA":           true,
	"PROGRAMFILES":          true,
	"SYSTEMROOT":            true,
	"WINDIR":                true,
	"TEMP":                  true,
	"TMP":                   true,
	"TMPDIR":                true,
	"LD_PRELOAD":            true,
	"LD_LIBRARY_PATH":       true,
	"DYLD_INSERT_LIBRARIES": true,
	"BASH_ENV":              true,
	"ENV":                   true,
	"PROMPT_COMMAND":        true,
	"IFS":                   true,
}

// Guard performs AST-based shell command analysis, relative path enforcement,
// relative CWD depth tracking, and human consent queueing.
type Guard struct {
	eventBus        *bus.EventBus
	readOnlyAllowed map[string]bool
}

// NewGuard creates a new Command Guard instance backed by the provided EventBus.
func NewGuard(eventBus *bus.EventBus) *Guard {
	allowed := make(map[string]bool, len(DefaultReadOnlyAllowlist))
	for k, v := range DefaultReadOnlyAllowlist {
		allowed[k] = v
	}
	return &Guard{
		eventBus:        eventBus,
		readOnlyAllowed: allowed,
	}
}

// Evaluate parses the command into a real shell AST using mvdan.cc/sh/v3/syntax and
// validates all arguments, literals, assignments, variable expansions, and redirections
// starting from currentRelCWD (relative to workspace_root).
func (g *Guard) Evaluate(currentRelCWD string, command string) GuardVerdict {
	return g.evaluateDepth(currentRelCWD, command, 0)
}

func (g *Guard) evaluateDepth(currentRelCWD string, command string, recursionDepth int) GuardVerdict {
	if recursionDepth > 4 {
		return rejectVerdict(ViolationSyntaxError, "nested shell evaluation depth exceeded")
	}

	if strings.TrimSpace(command) == "" {
		return rejectVerdict(ViolationEmptyCommand, "command cannot be empty")
	}

	initialSegs, err := parseInitialRelCWD(currentRelCWD)
	if err != nil {
		return rejectVerdict(ViolationCWDEscape, err.Error())
	}

	parser := syntax.NewParser(
		syntax.Variant(syntax.LangBash),
		syntax.KeepComments(false),
	)
	file, err := parser.Parse(strings.NewReader(command), "")
	if err != nil {
		return rejectVerdict(ViolationSyntaxError, fmt.Sprintf("shell AST syntax error: %v", err))
	}

	state := &walkState{
		guard:        g,
		cwdSegs:      initialSegs,
		commands:     make([]string, 0),
		needsConsent: false,
		depth:        recursionDepth,
	}

	if v := state.walkStmts(file.Stmts); v != nil {
		return *v
	}

	if len(state.commands) == 0 && !state.hasAssignment {
		return rejectVerdict(ViolationEmptyCommand, "command contains no executable statements")
	}

	nextRelCWD := "."
	if len(state.cwdSegs) > 0 {
		nextRelCWD = strings.Join(state.cwdSegs, "/")
	}

	if state.needsConsent {
		reason := "command requires human operator approval"
		if len(state.consentReasons) > 0 {
			reason = strings.Join(state.consentReasons, "; ")
		}
		return GuardVerdict{
			Action:     VerdictNeedsConsent,
			HTTPStatus: http.StatusOK,
			Reason:     reason,
			NextRelCWD: nextRelCWD,
			Commands:   state.commands,
		}
	}

	return GuardVerdict{
		Action:     VerdictAllow,
		HTTPStatus: http.StatusOK,
		Reason:     "command verified safe within workspace_root",
		NextRelCWD: nextRelCWD,
		Commands:   state.commands,
	}
}

// Authorize evaluates the command with the Command Guard and, if the verdict is
// VerdictNeedsConsent, checks session-level consent or blocks on the EventBus FIFO
// queue awaiting explicit human approval in the TUI.
func (g *Guard) Authorize(
	ctx context.Context,
	session *bus.AgentSession,
	sessionID bus.SessionID,
	currentRelCWD string,
	command string,
) (GuardVerdict, error) {
	verdict := g.Evaluate(currentRelCWD, command)
	if verdict.Action == VerdictReject || verdict.Action == VerdictAllow {
		return verdict, nil
	}

	// VerdictNeedsConsent: check if already permitted for this session.
	cmdKey := strings.TrimSpace(command)
	if session != nil && (session.HasSessionConsent("*") || session.HasSessionConsent(cmdKey)) {
		verdict.Action = VerdictAllow
		verdict.Reason = "approved via active session consent policy"
		return verdict, nil
	}

	if g.eventBus == nil {
		return rejectVerdict(ViolationConsentDenied, "consent required but EventBus is not configured"), nil
	}

	var agentID bus.AgentID
	var agentName, workspace string
	if session != nil {
		agentID = session.AgentID
		agentName = session.AgentName
		workspace = session.WorkspaceRoot
	}

	req := bus.ConsentRequest{
		Kind:      bus.ConsentKindCommand,
		AgentID:   agentID,
		AgentName: agentName,
		SessionID: sessionID,
		Workspace: workspace,
		RelCWD:    currentRelCWD,
		Command:   command,
		Reason:    verdict.Reason,
	}

	res, err := g.eventBus.RequestConsent(ctx, req)
	if err != nil {
		return rejectVerdict(ViolationConsentDenied, "command consent cancelled or timed out"), err
	}

	switch res.Decision {
	case bus.DecisionAllowOnce:
		verdict.Action = VerdictAllow
		verdict.HTTPStatus = http.StatusOK
		verdict.Reason = "approved once by human operator"
		return verdict, nil
	case bus.DecisionAllowSession:
		if session != nil {
			session.GrantSessionConsent("*")
		}
		verdict.Action = VerdictAllow
		verdict.HTTPStatus = http.StatusOK
		verdict.Reason = "approved for entire session by human operator"
		return verdict, nil
	default:
		return rejectVerdict(ViolationConsentDenied, "command denied by human operator in TUI"), nil
	}
}

type walkState struct {
	guard          *Guard
	cwdSegs        []string
	commands       []string
	needsConsent   bool
	consentReasons []string
	hasAssignment  bool
	depth          int
}

func (s *walkState) addConsentReason(r string) {
	s.needsConsent = true
	for _, existing := range s.consentReasons {
		if existing == r {
			return
		}
	}
	s.consentReasons = append(s.consentReasons, r)
}

func (s *walkState) walkStmts(stmts []*syntax.Stmt) *GuardVerdict {
	for _, stmt := range stmts {
		if v := s.walkStmt(stmt); v != nil {
			return v
		}
	}
	return nil
}

func (s *walkState) walkStmt(stmt *syntax.Stmt) *GuardVerdict {
	if stmt == nil {
		return nil
	}

	// Inspect all redirections attached to the statement.
	for _, redir := range stmt.Redirs {
		if v := s.inspectRedirect(redir); v != nil {
			return v
		}
	}

	if stmt.Cmd == nil {
		return nil
	}

	return s.walkCommand(stmt.Cmd)
}

func (s *walkState) walkCommand(cmd syntax.Command) *GuardVerdict {
	switch c := cmd.(type) {
	case *syntax.CallExpr:
		return s.inspectCallExpr(c)

	case *syntax.BinaryCmd:
		if c.Op == syntax.Pipe || c.Op == syntax.PipeAll {
			// In POSIX shells, pipeline stages run in subshells so their `cd` does not persist.
			saved := cloneSlice(s.cwdSegs)
			if v := s.walkStmt(c.X); v != nil {
				return v
			}
			s.cwdSegs = cloneSlice(saved)
			if v := s.walkStmt(c.Y); v != nil {
				return v
			}
			s.cwdSegs = saved
			return nil
		}
		if v := s.walkStmt(c.X); v != nil {
			return v
		}
		return s.walkStmt(c.Y)

	case *syntax.Subshell:
		// A subshell runs in a child process; validate its statements, then restore parent CWD.
		saved := cloneSlice(s.cwdSegs)
		if v := s.walkStmts(c.Stmts); v != nil {
			return v
		}
		s.cwdSegs = saved
		return nil

	case *syntax.Block:
		return s.walkStmts(c.Stmts)

	case *syntax.IfClause:
		for cur := c; cur != nil; cur = cur.Else {
			if v := s.walkStmts(cur.Cond); v != nil {
				return v
			}
			if v := s.walkStmts(cur.Then); v != nil {
				return v
			}
		}
		return nil

	case *syntax.WhileClause:
		if v := s.walkStmts(c.Cond); v != nil {
			return v
		}
		return s.walkStmts(c.Do)

	case *syntax.ForClause:
		if c.Loop != nil {
			if wordIter, ok := c.Loop.(*syntax.WordIter); ok {
				for _, w := range wordIter.Items {
					if v := s.inspectWord(w, false); v != nil {
						return v
					}
				}
			}
		}
		return s.walkStmts(c.Do)

	case *syntax.CaseClause:
		if c.Word != nil {
			if v := s.inspectWord(c.Word, false); v != nil {
				return v
			}
		}
		for _, item := range c.Items {
			for _, pat := range item.Patterns {
				if v := s.inspectWord(pat, false); v != nil {
					return v
				}
			}
			if v := s.walkStmts(item.Stmts); v != nil {
				return v
			}
		}
		return nil

	case *syntax.DeclClause:
		s.addConsentReason(fmt.Sprintf("declaration clause %q requires approval", c.Variant.Value))
		for _, assign := range c.Args {
			if v := s.inspectAssign(assign); v != nil {
				return v
			}
		}
		return nil

	case *syntax.FuncDecl:
		s.addConsentReason("shell function declaration requires approval")
		if c.Body != nil {
			return s.walkStmt(c.Body)
		}
		return nil

	case *syntax.ArithmCmd:
		return s.inspectNodeGeneric(c)

	case *syntax.TestClause:
		return s.inspectNodeGeneric(c)

	case *syntax.TimeClause:
		if c.Stmt != nil {
			return s.walkStmt(c.Stmt)
		}
		return nil

	case *syntax.CoprocClause:
		s.addConsentReason("coproc clause requires approval")
		if c.Stmt != nil {
			return s.walkStmt(c.Stmt)
		}
		return nil

	default:
		return s.inspectNodeGeneric(cmd)
	}
}

func (s *walkState) inspectCallExpr(call *syntax.CallExpr) *GuardVerdict {
	for _, assign := range call.Assigns {
		s.hasAssignment = true
		if v := s.inspectAssign(assign); v != nil {
			return v
		}
	}

	if len(call.Args) == 0 {
		return nil
	}

	// Inspect the command word itself (e.g., reject `/bin/sh`, `../script.sh`, `~/run.sh`, `$(cmd)`).
	if v := s.inspectWord(call.Args[0], false); v != nil {
		return v
	}

	cmdName := wordToString(call.Args[0])
	baseCmd := strings.ToLower(filepath.Base(cmdName))
	s.commands = append(s.commands, cmdName)

	// Directory navigation handling (`cd`, `chdir`, `pushd`, `popd`).
	switch baseCmd {
	case "popd":
		v := rejectVerdict(ViolationCWDEscape, "'popd' is forbidden because directory stack may escape workspace_root")
		return &v

	case "cd", "chdir", "pushd":
		return s.handleCD(baseCmd, call.Args[1:])
	}

	// Inspect all remaining arguments strictly (no `..` allowed in non-cd arguments).
	argStrings := make([]string, 0, len(call.Args)-1)
	for _, argWord := range call.Args[1:] {
		if v := s.inspectWord(argWord, false); v != nil {
			return v
		}
		argStrings = append(argStrings, wordToString(argWord))
	}

	// Prevent smuggling forbidden paths or commands through eval / nested shells / source.
	if v := s.inspectNestedExecution(baseCmd, argStrings); v != nil {
		return v
	}

	// Check if command is on the read-only allowlist.
	if !s.isReadOnlyCommand(cmdName, baseCmd, argStrings) {
		s.addConsentReason(fmt.Sprintf("command %q is outside the read-only allowlist", cmdName))
	}

	return nil
}

func (s *walkState) handleCD(cmdName string, args []*syntax.Word) *GuardVerdict {
	if len(args) == 0 {
		// Bare `cd` in POSIX shells jumps to $HOME!
		v := rejectVerdict(ViolationHomeExpansion, fmt.Sprintf("bare %q without target navigates to $HOME outside workspace_root", cmdName))
		return &v
	}

	var target string
	targetFound := false
	for _, arg := range args {
		// First check AST parts for substitutions, home expansions, or env escapes.
		if v := s.inspectWordPartsOnly(arg); v != nil {
			return v
		}
		val := wordToString(arg)
		if val == "-L" || val == "-P" {
			continue
		}
		if val == "-" {
			v := rejectVerdict(ViolationCWDEscape, fmt.Sprintf("'%s -' is forbidden because OLDPWD may escape workspace_root", cmdName))
			return &v
		}
		if strings.HasPrefix(val, "-") {
			v := rejectVerdict(ViolationCWDEscape, fmt.Sprintf("unsupported flag %q for %s", val, cmdName))
			return &v
		}
		if targetFound {
			v := rejectVerdict(ViolationSyntaxError, fmt.Sprintf("too many arguments for %s", cmdName))
			return &v
		}
		target = val
		targetFound = true
	}

	if !targetFound || strings.TrimSpace(target) == "" {
		v := rejectVerdict(ViolationHomeExpansion, fmt.Sprintf("%q without target directory navigates to $HOME", cmdName))
		return &v
	}

	// Validate path rules on target (rejects absolute paths, drive letters, tilde).
	if v := checkForbiddenPathPrefixes(target); v != nil {
		return v
	}

	nextSegs, v := applyCDTransition(s.cwdSegs, target)
	if v != nil {
		return v
	}
	s.cwdSegs = nextSegs
	return nil
}

func (s *walkState) inspectNestedExecution(baseCmd string, args []string) *GuardVerdict {
	switch baseCmd {
	case "eval":
		s.addConsentReason("'eval' requires human operator approval")
		joined := strings.Join(args, " ")
		if strings.TrimSpace(joined) != "" {
			subVerdict := s.guard.evaluateDepth(strings.Join(s.cwdSegs, "/"), joined, s.depth+1)
			if subVerdict.Action == VerdictReject {
				return &subVerdict
			}
		}

	case "source", ".":
		s.addConsentReason(fmt.Sprintf("%q script sourcing requires human operator approval", baseCmd))

	case "sh", "bash", "zsh", "dash", "ksh", "pwsh", "powershell", "powershell.exe", "cmd", "cmd.exe":
		s.addConsentReason(fmt.Sprintf("subshell interpreter %q requires human operator approval", baseCmd))
		for i := 0; i < len(args); i++ {
			a := args[i]
			if a == "-c" || a == "/c" || a == "/C" || a == "-Command" || a == "-command" {
				if i+1 < len(args) {
					subVerdict := s.guard.evaluateDepth(strings.Join(s.cwdSegs, "/"), args[i+1], s.depth+1)
					if subVerdict.Action == VerdictReject {
						return &subVerdict
					}
				}
			}
		}
	}
	return nil
}

func (s *walkState) isReadOnlyCommand(rawCmd, baseCmd string, args []string) bool {
	// Reject relative executable invocations like ./script.sh from automatic allowlist.
	if strings.ContainsAny(rawCmd, `/\`) {
		return false
	}

	if subMap, ok := SafeSubcommands[baseCmd]; ok {
		if len(args) == 0 {
			return false
		}
		// Find first non-flag subcommand token.
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				// Disallow git -C / -c / --git-dir / --work-tree from automatic allowlist
				if a == "-c" || a == "-C" || strings.HasPrefix(a, "--git-dir") || strings.HasPrefix(a, "--work-tree") || strings.HasPrefix(a, "--exec-path") {
					return false
				}
				continue
			}
			return subMap[a]
		}
		return false
	}

	if baseCmd == "find" {
		for _, a := range args {
			if a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir" || a == "-delete" || a == "-fprint" || a == "-fprintf" || a == "-fls" {
				return false
			}
		}
	}

	return s.guard.readOnlyAllowed[baseCmd]
}

func (s *walkState) inspectRedirect(redir *syntax.Redirect) *GuardVerdict {
	if redir == nil {
		return nil
	}

	switch redir.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.RdrAll, syntax.AppAll, syntax.RdrInOut:
		s.addConsentReason(fmt.Sprintf("output redirection (%s) modifies filesystem", redir.Op.String()))
	}

	if redir.Word != nil {
		if v := s.inspectWord(redir.Word, false); v != nil {
			return v
		}
	}
	if redir.Hdoc != nil {
		if v := s.inspectWord(redir.Hdoc, true); v != nil {
			return v
		}
	}
	return nil
}

func (s *walkState) inspectAssign(assign *syntax.Assign) *GuardVerdict {
	if assign == nil {
		return nil
	}
	if assign.Name != nil {
		nameUpper := strings.ToUpper(assign.Name.Value)
		if ForbiddenHomeEnvVars[nameUpper] {
			v := rejectVerdict(ViolationHomeExpansion, fmt.Sprintf("modifying environment variable %s is forbidden", assign.Name.Value))
			return &v
		}
		if ForbiddenEscapeEnvVars[nameUpper] || nameUpper == "PATH" {
			v := rejectVerdict(ViolationEnvEscape, fmt.Sprintf("modifying protected environment variable %s is forbidden", assign.Name.Value))
			return &v
		}
	}
	if assign.Index != nil {
		if v := s.inspectNodeGeneric(assign.Index); v != nil {
			return v
		}
	}
	if assign.Value != nil {
		if v := s.inspectWord(assign.Value, false); v != nil {
			return v
		}
	}
	if assign.Array != nil {
		for _, elem := range assign.Array.Elems {
			if elem.Index != nil {
				if v := s.inspectNodeGeneric(elem.Index); v != nil {
					return v
				}
			}
			if elem.Value != nil {
				if v := s.inspectWord(elem.Value, false); v != nil {
					return v
				}
			}
		}
	}
	return nil
}

func (s *walkState) inspectWordPartsOnly(w *syntax.Word) *GuardVerdict {
	if w == nil {
		return nil
	}
	var verdict *GuardVerdict
	syntax.Walk(w, func(node syntax.Node) bool {
		if verdict != nil || node == nil {
			return false
		}
		switch n := node.(type) {
		case *syntax.CmdSubst:
			v := rejectVerdict(ViolationCommandSubstitution, "command substitution $() or backticks is forbidden")
			verdict = &v
			return false
		case *syntax.ProcSubst:
			v := rejectVerdict(ViolationCommandSubstitution, "process substitution <() or >() is forbidden")
			verdict = &v
			return false
		case *syntax.ParamExp:
			if v := inspectParamExp(n); v != nil {
				verdict = v
				return false
			}
		}
		return true
	})
	return verdict
}

func (s *walkState) inspectWord(w *syntax.Word, isHeredocBody bool) *GuardVerdict {
	if w == nil {
		return nil
	}
	if v := s.inspectWordPartsOnly(w); v != nil {
		return v
	}
	if isHeredocBody {
		return nil
	}

	// Reconstruct the full word string across adjacent literals and quotes so
	// concatenated quote tricks like `".""."/secret` or `"/"etc/passwd` are caught.
	full := wordToString(w)
	if v := validateArgumentPath(full); v != nil {
		return v
	}

	// Also inspect individual literal/quoted fragments and whitespace-separated tokens inside quotes.
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if v := validateArgumentPath(p.Value); v != nil {
				return v
			}
		case *syntax.SglQuoted:
			if v := validateQuotedContent(p.Value); v != nil {
				return v
			}
		case *syntax.DblQuoted:
			subWord := &syntax.Word{Parts: p.Parts}
			if v := validateQuotedContent(wordToString(subWord)); v != nil {
				return v
			}
		}
	}

	return nil
}

func (s *walkState) inspectNodeGeneric(node syntax.Node) *GuardVerdict {
	if node == nil {
		return nil
	}
	var verdict *GuardVerdict
	syntax.Walk(node, func(n syntax.Node) bool {
		if verdict != nil || n == nil {
			return false
		}
		switch x := n.(type) {
		case *syntax.CmdSubst:
			v := rejectVerdict(ViolationCommandSubstitution, "command substitution $() or backticks is forbidden")
			verdict = &v
			return false
		case *syntax.ProcSubst:
			v := rejectVerdict(ViolationCommandSubstitution, "process substitution <() or >() is forbidden")
			verdict = &v
			return false
		case *syntax.ParamExp:
			if v := inspectParamExp(x); v != nil {
				verdict = v
				return false
			}
		case *syntax.Word:
			if v := s.inspectWord(x, false); v != nil {
				verdict = v
				return false
			}
			return false
		}
		return true
	})
	return verdict
}

func inspectParamExp(pe *syntax.ParamExp) *GuardVerdict {
	if pe == nil {
		return nil
	}
	if pe.Excl {
		v := rejectVerdict(ViolationEnvEscape, "indirect parameter expansion ${!var} is forbidden")
		return &v
	}
	if pe.Param != nil {
		nameUpper := strings.ToUpper(pe.Param.Value)
		if ForbiddenHomeEnvVars[nameUpper] {
			v := rejectVerdict(ViolationHomeExpansion, fmt.Sprintf("home environment variable expansion $%s is forbidden", pe.Param.Value))
			return &v
		}
		if ForbiddenEscapeEnvVars[nameUpper] {
			v := rejectVerdict(ViolationEnvEscape, fmt.Sprintf("environment variable expansion $%s is forbidden", pe.Param.Value))
			return &v
		}
	}
	return nil
}

// validateQuotedContent checks both the whole quoted string and any whitespace-delimited
// tokens inside it (catching e.g. `sh -c "cat /etc/passwd"`, `'$(id)'`, or `"--out=../secret"`).
func validateQuotedContent(val string) *GuardVerdict {
	if strings.Contains(val, "$(") || strings.Contains(val, "`") {
		v := rejectVerdict(ViolationCommandSubstitution, "command substitution inside quoted argument is forbidden")
		return &v
	}
	upper := strings.ToUpper(val)
	for homeVar := range ForbiddenHomeEnvVars {
		if strings.Contains(upper, "$"+homeVar) || strings.Contains(upper, "${"+homeVar) || strings.Contains(upper, "%"+homeVar+"%") {
			v := rejectVerdict(ViolationHomeExpansion, fmt.Sprintf("home variable reference %s is forbidden", homeVar))
			return &v
		}
	}
	if v := validateArgumentPath(val); v != nil {
		return v
	}
	for _, field := range strings.Fields(val) {
		cleaned := strings.Trim(field, `"'(),;`)
		if cleaned != "" {
			if v := validateArgumentPath(cleaned); v != nil {
				return v
			}
		}
	}
	return nil
}

// validateArgumentPath inspects a single argument/token (and any `--flag=value` RHS)
// for home expansion (`~`), absolute paths (`/`, `\`, `C:\`, `file://`), and `..` segments.
func validateArgumentPath(token string) *GuardVerdict {
	if token == "" {
		return nil
	}

	upper := strings.ToUpper(token)
	for homeVar := range ForbiddenHomeEnvVars {
		if strings.Contains(upper, "%"+homeVar+"%") {
			v := rejectVerdict(ViolationHomeExpansion, fmt.Sprintf("Windows home variable %%%s%% is forbidden", homeVar))
			return &v
		}
	}

	candidates := []string{token}
	if idx := strings.IndexByte(token, '='); idx != -1 && idx+1 < len(token) {
		candidates = append(candidates, token[idx+1:])
	}

	for _, cand := range candidates {
		if v := checkForbiddenPathPrefixes(cand); v != nil {
			return v
		}
		if containsDotDotSegment(cand) {
			v := rejectVerdict(ViolationPathTraversal, fmt.Sprintf("path traversal segment '..' is forbidden in %q", token))
			return &v
		}
	}
	return nil
}

// checkForbiddenPathPrefixes checks if a candidate string starts with `~`, `/`, `\`,
// a Windows drive letter (`C:`), or a `file:` URI.
func checkForbiddenPathPrefixes(cand string) *GuardVerdict {
	if cand == "" {
		return nil
	}

	// 1. Home directory tilde expansion (`~`, `~/...`, `~user/...`)
	if cand == "~" || strings.HasPrefix(cand, "~/") || strings.HasPrefix(cand, `~\`) || isTildeUserPrefix(cand) {
		v := rejectVerdict(ViolationHomeExpansion, fmt.Sprintf("home directory expansion '~' is forbidden in %q", cand))
		return &v
	}

	// 2. Unix root or absolute path (`/`, `/etc/passwd`, `//server/share`)
	if strings.HasPrefix(cand, "/") {
		v := rejectVerdict(ViolationAbsolutePath, fmt.Sprintf("absolute path %q is forbidden; use workspace-relative paths only", cand))
		return &v
	}

	// 3. Windows root, backslash-prefixed path, or UNC path (`\`, `\Windows`, `\\server\share`)
	if strings.HasPrefix(cand, `\`) {
		v := rejectVerdict(ViolationAbsolutePath, fmt.Sprintf("backslash/UNC root path %q is forbidden; use workspace-relative paths only", cand))
		return &v
	}

	// 4. Windows drive path (`C:`, `C:\...`, `C:/...`, `d:\...`)
	if isWindowsDrivePath(cand) {
		v := rejectVerdict(ViolationAbsolutePath, fmt.Sprintf("Windows drive path %q is forbidden; use workspace-relative paths only", cand))
		return &v
	}

	// 5. Local file URI (`file:///etc/passwd`)
	if strings.HasPrefix(strings.ToLower(cand), "file:") {
		v := rejectVerdict(ViolationAbsolutePath, fmt.Sprintf("file:// URI %q is forbidden", cand))
		return &v
	}

	// 6. Short flag attached to absolute path (e.g. `-I/usr/include`, `-f/etc/passwd`, `-IC:\sdk`)
	if len(cand) >= 3 && cand[0] == '-' && cand[1] != '-' {
		rest := cand[2:]
		if strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, `\`) || isWindowsDrivePath(rest) || strings.HasPrefix(rest, "~") {
			return checkForbiddenPathPrefixes(rest)
		}
	}

	// 7. Colon-separated path list or remote spec (e.g. `host:/etc/passwd`, `a:~/bin`)
	// Exclude http:// and https:// URLs.
	lower := strings.ToLower(cand)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		if idx := strings.IndexByte(cand, ':'); idx > 1 && idx+1 < len(cand) {
			rhs := cand[idx+1:]
			if strings.HasPrefix(rhs, "/") || strings.HasPrefix(rhs, `\`) || strings.HasPrefix(rhs, "~") || isWindowsDrivePath(rhs) {
				return checkForbiddenPathPrefixes(rhs)
			}
		}
	}

	return nil
}

func isTildeUserPrefix(s string) bool {
	if !strings.HasPrefix(s, "~") || len(s) < 2 {
		return false
	}
	slashIdx := strings.IndexAny(s, `/\`)
	userPart := s[1:]
	if slashIdx != -1 {
		userPart = s[1:slashIdx]
	}
	if userPart == "" {
		return true
	}
	for i := 0; i < len(userPart); i++ {
		ch := userPart[i]
		isAlphaNum := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-'
		if !isAlphaNum {
			return false
		}
	}
	return slashIdx != -1 || len(userPart) > 0
}

func isWindowsDrivePath(s string) bool {
	if len(s) < 2 {
		return false
	}
	c := s[0]
	isLetter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	return isLetter && s[1] == ':'
}

func containsDotDotSegment(s string) bool {
	if s == ".." {
		return true
	}
	// Strip short-flag prefix like `-L../lib` -> `../lib`
	if len(s) >= 4 && s[0] == '-' && s[1] != '-' && s[2] == '.' && s[3] == '.' {
		s = s[2:]
	}
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == '/' || r == '\\' || r == ':'
	})
	for _, seg := range fields {
		if seg == ".." {
			return true
		}
	}
	return false
}

// applyCDTransition validates and applies a `cd` target path against current cwdSegs,
// tracking relative depth within workspace_root and rejecting any traversal below depth 0
// or obfuscated mid-path `..` segments.
func applyCDTransition(currentSegs []string, target string) ([]string, *GuardVerdict) {
	rawSegs := strings.FieldsFunc(target, func(r rune) bool {
		return r == '/' || r == '\\'
	})

	stack := cloneSlice(currentSegs)
	seenDownward := false

	for _, seg := range rawSegs {
		if seg == "" || seg == "." {
			continue
		}
		if seg == "..." || strings.Trim(seg, ".") == "" && len(seg) > 2 {
			v := rejectVerdict(ViolationPathTraversal, fmt.Sprintf("ambiguous dot segment %q is forbidden", seg))
			return nil, &v
		}
		if seg == ".." {
			if seenDownward {
				v := rejectVerdict(ViolationPathTraversal, fmt.Sprintf("mid-path parent traversal '..' in %q is forbidden", target))
				return nil, &v
			}
			if len(stack) == 0 {
				v := rejectVerdict(ViolationPathTraversal, fmt.Sprintf("path traversal %q escapes workspace_root (depth < 0)", target))
				return nil, &v
			}
			stack = stack[:len(stack)-1]
			continue
		}
		seenDownward = true
		stack = append(stack, seg)
	}

	return stack, nil
}

func parseInitialRelCWD(relCWD string) ([]string, error) {
	relCWD = strings.TrimSpace(relCWD)
	if relCWD == "" || relCWD == "." {
		return nil, nil
	}
	if v := checkForbiddenPathPrefixes(relCWD); v != nil {
		return nil, fmt.Errorf("invalid initial CWD %q: %s", relCWD, v.Reason)
	}

	rawSegs := strings.FieldsFunc(relCWD, func(r rune) bool {
		return r == '/' || r == '\\'
	})

	var stack []string
	for _, seg := range rawSegs {
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			if len(stack) == 0 {
				return nil, fmt.Errorf("initial relative CWD %q escapes workspace_root", relCWD)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		stack = append(stack, seg)
	}
	return stack, nil
}

func wordToString(w *syntax.Word) string {
	if w == nil {
		return ""
	}
	var sb strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			sb.WriteString(p.Value)
		case *syntax.SglQuoted:
			sb.WriteString(p.Value)
		case *syntax.DblQuoted:
			sb.WriteString(wordToString(&syntax.Word{Parts: p.Parts}))
		case *syntax.ParamExp:
			if p.Param != nil {
				sb.WriteString("$" + p.Param.Value)
			}
		}
	}
	return sb.String()
}

func cloneSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func rejectVerdict(code ViolationCode, reason string) GuardVerdict {
	return GuardVerdict{
		Action:     VerdictReject,
		HTTPStatus: http.StatusForbidden,
		Violation:  code,
		Reason:     reason,
	}
}
