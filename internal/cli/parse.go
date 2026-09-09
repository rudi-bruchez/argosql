package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rudi-bruchez/argosql/internal/model"
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
		spec, ok := superset[name]
		if !ok {
			req.Explicit = explicit
			return req, nil, flagError(name, "unknown flag")
		}

		var val string
		if spec.Kind == FlagBool {
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
	if consumed < len(bare) {
		return req, nil, &model.PublicError{
			Code:    2,
			Kind:    "extra_arguments",
			Message: fmt.Sprintf("unexpected extra arguments: %s", strings.Join(bare[consumed:], " ")),
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
