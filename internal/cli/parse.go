package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rudi-bruchez/argosql/internal/diagnostics"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// Parse turns args (the program's arguments, not including argv[0]) into
// a Request and the Command it selects, against a fresh Registry.
//
// Flags are recognized wherever they appear - before the command path,
// after it, or after the command's own positional arguments - and a
// recognized flag's value (everything but a FlagBool) is always
// consumed as the very next token before Parse ever looks at that token
// as a candidate command-path word. This is what keeps "--ctx qs info"
// from reading "qs" as the start of some future "qs ..." command: "qs"
// is consumed as --ctx's value, and the command is "info". Command
// selection itself never comes close to an ambiguous prefix match
// either: see Registry.matchCommand, which only ever accepts an exact,
// whole-word name.
//
// Parse returns a Request even when it also returns an error: Format is
// always resolved from whatever flags were tokenized before the
// failure, defaulting to "tsv", so a caller that wants to report an
// argument error as JSON (design spec: "JSON errors use the same
// envelope") can do so without re-parsing anything.
func Parse(args []string) (Request, *Command, error) {
	reg := NewRegistry()
	superset := reg.flagSuperset()

	raw := map[string]string{}
	explicit := map[string]bool{}
	var bare []string

	req := Request{Format: "tsv"}

	i := 0
	for i < len(args) {
		tok := args[i]
		if !strings.HasPrefix(tok, "--") {
			bare = append(bare, tok)
			i++
			continue
		}
		name := strings.TrimPrefix(tok, "--")
		if name == "" {
			req.Explicit = explicit
			return req, nil, &model.PublicError{Code: 2, Kind: "flag", Message: "empty flag name"}
		}

		// Fix 1's A7 (pending-fixes.md P1): "--flag=value" is cut on the
		// FIRST "=" - not the last, so a value that itself contains "="
		// (e.g. --object dbo.a=b, which arrives as its own separate
		// token anyway, or a hypothetical --flag=a=b) keeps everything
		// after that first sign - before name is ever looked up in the
		// registry. The previous form looked up "top=5" itself and
		// reported "unknown flag", which is false: --top exists, only
		// the message was wrong, and an agent reading it would have
		// dropped the flag instead of fixing its syntax.
		var val string
		hasInlineValue := false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			val = name[eq+1:]
			name = name[:eq]
			hasInlineValue = true
		}

		spec, ok := superset[name]
		if !ok {
			req.Explicit = explicit
			return req, nil, flagError(name, "unknown flag")
		}

		if hasInlineValue {
			// "--no-truncate=false" must value false, not the
			// FlagBool branch's usual forced "true": Validate still
			// parses val as a bool below, so "--x=notabool" is still
			// rejected the same way a separated "--x notabool" is.
			i++
		} else if spec.Kind == FlagBool {
			val = "true"
			i++
		} else {
			if i+1 >= len(args) {
				req.Explicit = explicit
				return req, nil, flagError(name, "missing value")
			}
			val = args[i+1]
			i += 2
		}

		if prev, seen := raw[name]; seen && prev != val {
			req.Explicit = explicit
			return req, nil, flagError(name, fmt.Sprintf("given twice with conflicting values %q and %q", prev, val))
		}
		raw[name] = val
		explicit[name] = true
		req.Format = resolveFormat(raw, explicit)
	}
	req.Explicit = explicit

	cmd, consumed, err := reg.matchCommand(bare)
	if err != nil {
		return req, nil, err
	}

	// A command with no declared Positional keeps matchCommand's
	// ordinary contract: any bare token left over after its own words
	// is an error. "qs query" is the one command that takes exactly
	// one more - its query_id - parsed and range-checked here, before
	// any connection opens, the same "syntactic check before
	// connection" ordering already applied to --object below and to
	// the window flags (design spec: "Query and plan IDs are positive
	// signed 64-bit integers... Syntactically invalid IDs ... yield
	// code 2").
	extra := bare[consumed:]
	if cmd.Positional == "" {
		if len(extra) > 0 {
			return req, nil, &model.PublicError{
				Code:    2,
				Kind:    "extra_arguments",
				Message: fmt.Sprintf("unexpected extra arguments: %s", strings.Join(extra, " ")),
			}
		}
	} else {
		if len(extra) != 1 {
			return req, nil, &model.PublicError{
				Code:    2,
				Kind:    "invalid_argument",
				Message: fmt.Sprintf("%s requires exactly one %s argument", cmd.Name, cmd.Positional),
			}
		}
		// PositionalIsObject: "obj table"/"obj code"/"idx list"/"size
		// table"'s own <schema.name> argument - validated exactly like
		// --object already is below (the same syntactic, pre-connection
		// check sqlserver.Resolve itself performs first), never parsed
		// as an integer.
		if cmd.PositionalIsObject {
			if err := sqlserver.ValidateQualifiedName(extra[0]); err != nil {
				return req, nil, err
			}
			req.Object = extra[0]
		} else {
			id, perr := strconv.ParseInt(extra[0], 10, 64)
			if perr != nil || id <= 0 {
				return req, nil, &model.PublicError{
					Code:    2,
					Kind:    "invalid_argument",
					Message: fmt.Sprintf("%s: %q is not a valid positive %s", cmd.Name, extra[0], cmd.Positional),
				}
			}
			req.QueryID = id
		}
	}

	allowed := map[string]Flag{}
	for _, f := range globalFlags() {
		allowed[f.Name] = f
	}
	for _, f := range cmd.Flags {
		allowed[f.Name] = f
	}

	for name := range explicit {
		if _, ok := allowed[name]; !ok {
			return req, nil, flagError(name, fmt.Sprintf("is not valid for command %q", cmd.Name))
		}
	}

	allowedFlags := make([]Flag, 0, len(allowed))
	for _, f := range allowed {
		allowedFlags = append(allowedFlags, f)
	}
	if err := checkExclusions(allowedFlags, explicit); err != nil {
		return req, nil, err
	}

	req.Command = cmd.Name
	for name, f := range allowed {
		var v any
		if explicit[name] {
			parsed, err := f.Validate(raw[name])
			if err != nil {
				return req, nil, err
			}
			v = parsed
		} else if f.Default != nil {
			v = f.Default
		} else {
			v = zeroForKind(f.Kind)
		}
		if err := assignRequestField(&req, name, v); err != nil {
			return req, nil, err
		}
	}
	// --json is sugar for --format json on the commands that declare it
	// (help); it never has its own Request field, and it always wins
	// over --format regardless of which of the two was typed first.
	if explicit["json"] {
		req.Format = "json"
	}

	// "qs top --by executions --aggregate avg" is rejected here, before
	// any connection is opened (design spec: "For executions, total is
	// sum(count_executions) and --aggregate avg is rejected with code
	// 2") - a cross-field rule Flag's own Enum cannot express, since
	// each flag is validated independently of the other's value.
	if cmd.Name == "qs top" && req.By == "executions" && req.Aggregate == "avg" {
		return req, nil, &model.PublicError{Code: 2, Kind: "flag", Message: "--aggregate avg is not valid with --by executions"}
	}

	// "plan <query_id> --plan-id <id> [--summary]" (design spec): --plan-id
	// is the one non-positional argument this command cannot do without,
	// and Flag itself has no "required" concept (see Command.Positional's
	// own doc comment on why --ctx's own requiredness, the only other
	// case like this, is checked directly here rather than through a new
	// Flag field for a single use). Checked before any connection opens,
	// the same "syntactic check before connection" ordering already
	// applied above and below to every other pre-connection rejection in
	// this function.
	if cmd.Name == "plan" && !explicit["plan-id"] {
		return req, nil, &model.PublicError{Code: 2, Kind: "flag", Message: "--plan-id is required"}
	}

	// An explicitly given --since or --until with an EMPTY value is a
	// malformed timestamp, never "not given": ParseWindow only ever
	// sees the two strings, not whether they were typed at all, so
	// "--since '' --until ''" (for example, an unset environment
	// variable expanded blank in an operations script) would otherwise
	// look identical to neither flag being given, and silently fall
	// back to the relative --hours window instead of reporting the
	// actual mistake. Checked here, against req.Explicit, which is the
	// one place that distinguishes "empty" from "absent" - never both
	// given (the valid case), only the explicitly-empty one.
	if explicit["since"] && req.Since == "" {
		return req, nil, &model.PublicError{Code: 2, Kind: "invalid_window", Message: "--since: empty value is not a valid RFC3339 timestamp"}
	}
	if explicit["until"] && req.Until == "" {
		return req, nil, &model.PublicError{Code: 2, Kind: "invalid_window", Message: "--until: empty value is not a valid RFC3339 timestamp"}
	}

	// --object's two-part syntax (schema.object, bracket-quoting rules
	// included) is checked here, before any connection opens - the same
	// ordering already applied above to --aggregate/--by and below to
	// the window. This is ONLY the syntactic check sqlserver.Resolve
	// itself performs first, with no catalog access; the TYPE of the
	// resolved object (procedure vs. table vs. anything else) still has
	// to wait for a real connection, since only the catalog can answer
	// that.
	if _, ok := allowed["object"]; ok && req.Object != "" {
		if err := sqlserver.ValidateQualifiedName(req.Object); err != nil {
			return req, nil, err
		}
	}

	// --table's two-part syntax is checked here for exactly the same
	// reason as --object above, right next to it: "idx missing" is the
	// one command that declares "table" rather than "object" for its
	// own optional schema.name filter (Command.Flags below), so the
	// same pre-connection syntax check applies to it under its own flag
	// name.
	if _, ok := allowed["table"]; ok && req.Table != "" {
		if err := sqlserver.ValidateQualifiedName(req.Table); err != nil {
			return req, nil, err
		}
	}

	// ParseWindow resolves --hours/--since/--until into req.Window here,
	// before any connection opens, so every one of the design spec's
	// code-2 window rejections (mixed half-given since/until, since >=
	// until, malformed RFC3339, an --hours overflow) reports "bad
	// argument" rather than a connection failure a user or an agent
	// would go debug a healthy network for. Guarded by the command
	// actually declaring "hours": calling this unconditionally would
	// resolve (and potentially reject) a window for "info" or "qs
	// status", which declare no such flags at all and carry zero
	// meaning for one. time.Now() is read exactly once here, matching
	// the design spec's "resolve relative time once in UTC for the
	// entire command" - never read again later, closer to the
	// connection, which would let the two diverge.
	if _, ok := allowed["hours"]; ok {
		win, err := diagnostics.ParseWindow(time.Now(), req.Hours, req.Since, req.Until)
		if err != nil {
			return req, nil, err
		}
		req.Window = win
	}

	if !cmd.Offline && req.ContextName == "" {
		return req, nil, &model.PublicError{Code: 2, Kind: "flag", Message: "--ctx is required"}
	}

	return req, cmd, nil
}

// resolveFormat is Parse's running best guess at the eventual output
// format, recomputed after every flag token so that an error returned
// mid-parse still carries a usable Format: --json (when explicitly
// given) always wins over --format, matching the real, fully-validated
// assignment Parse performs on its success path.
func resolveFormat(raw map[string]string, explicit map[string]bool) string {
	format := "tsv"
	if v, ok := raw["format"]; ok {
		format = v
	}
	if explicit["json"] {
		format = "json"
	}
	return format
}

// assignRequestField copies v, already validated and typed by Flag.Kind,
// into the Request field flag name maps to. "json" has no field of its
// own - see Parse's sugar handling right after this loop - so it is
// accepted here as a deliberate no-op rather than falling into default.
func assignRequestField(req *Request, name string, v any) error {
	switch name {
	case "ctx":
		req.ContextName = v.(string)
	case "db":
		req.Database = v.(string)
	case "config":
		req.ConfigPath = v.(string)
	case "format":
		req.Format = v.(string)
	case "timeout":
		req.TimeoutSeconds = int(v.(int64))
	case "preview":
		req.PreviewRows = int(v.(int64))
	case "truncate":
		req.TruncateRunes = int(v.(int64))
	case "no-truncate":
		req.NoTruncate = v.(bool)
	case "out-dir":
		req.OutDir = v.(string)
	case "object":
		req.Object = v.(string)
	case "table":
		req.Table = v.(string)
	case "min-executions":
		req.MinExecutions = v.(int64)
	case "by":
		req.By = v.(string)
	case "aggregate":
		req.Aggregate = v.(string)
	case "hours":
		req.Hours = int(v.(int64))
	case "since":
		req.Since = v.(string)
	case "until":
		req.Until = v.(string)
	case "top":
		req.Top = int(v.(int64))
	case "include-internal":
		req.IncludeInternal = v.(bool)
	case "plan-id":
		req.PlanID = v.(int64)
	case "summary":
		req.Summary = v.(bool)
	case "json":
		// handled by Parse directly after this loop.
	default:
		return fmt.Errorf("cli: internal: flag %q has no Request field mapping", name)
	}
	return nil
}

// defaultConfigPath resolves the config file Run uses when the caller
// never passed --config: <os.UserConfigDir()>/argosql/config.yaml. It is
// never called for an offline command (help): os.UserConfigDir fails
// outright on an environment with no configuration directory defined,
// and help must keep working there. Run calls this itself, once, only
// for a command that is actually about to load a profile - this
// function does not belong to Parse's own return path, which is why
// Request.ConfigPath is left empty by Parse whenever --config was not
// given.
func defaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", &model.PublicError{Code: 2, Kind: "config_path", Message: "cannot resolve user config directory"}
	}
	return filepath.Join(dir, "argosql", "config.yaml"), nil
}

// defaultOutDir resolves the artifact directory Run uses when the caller
// never passed --out-dir: <os.UserCacheDir()>/argosql. Same deferral as
// defaultConfigPath, for the same reason: os.UserCacheDir can fail on an
// environment with no cache directory defined, and that must never stop
// help from working.
func defaultOutDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", &model.PublicError{Code: 2, Kind: "out_dir_path", Message: "cannot resolve user cache directory"}
	}
	return filepath.Join(dir, "argosql"), nil
}
