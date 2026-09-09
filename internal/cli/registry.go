// Package cli is the program's entry point: the command registry, the
// argument parser built on it, and Run, the one function cmd/asq calls.
// No package outside cmd/asq may import it.
package cli

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// Flag kinds Validate understands. A flag's Kind decides how its raw
// command-line string is parsed and which of Flag's other fields apply:
// Enum only ever matters for FlagString, Min/Max only for FlagInt64.
const (
	FlagString = "string"
	FlagBool   = "bool"
	FlagInt64  = "int64"
)

// Flag describes one command-line flag as the registry declares it:
// enough for the parser to recognize it, validate its value, and for
// help to render it, without any of that logic living in the command
// itself.
//
// Flag{Name,Kind string; Default any; Min,Max int64} - the shape named
// by the implementation plan - cannot express either an enum (--format
// tsv|json, --by cpu|duration|...) or a mutual exclusion (--no-truncate
// vs --truncate, --hours vs --since/--until). Enum and ExclusiveWith
// exist so every command that needs either rule declares it here, once,
// rather than re-checking it by hand in its own Execute.
//
// Min and Max apply only when both are not simultaneously zero: a flag
// with no declared bound (Min==0 and Max==0, such as --ctx or --db) is
// never range-checked. A flag whose true valid range is exactly [0, 0]
// does not exist in this registry, so this convention never collides
// with a real bound.
type Flag struct {
	Name          string
	Kind          string
	Default       any
	Min, Max      int64
	Enum          []string
	ExclusiveWith []string
}

// flagError builds the code-2 error Validate and the parser return for a
// malformed or out-of-range flag value.
func flagError(name, message string) error {
	return &model.PublicError{Code: 2, Kind: "flag", Message: fmt.Sprintf("--%s: %s", name, message)}
}

// Validate parses raw according to f.Kind, checking it against Enum (for
// FlagString) or against Min/Max (for FlagInt64, when a bound is
// declared - see Flag's doc comment). It does not know about
// ExclusiveWith: that rule depends on which other flags were explicitly
// given on this invocation, which only the parser can see once every
// flag is tokenized - see checkExclusions.
func (f Flag) Validate(raw string) (any, error) {
	switch f.Kind {
	case FlagBool:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, flagError(f.Name, fmt.Sprintf("%q is not a valid boolean", raw))
		}
		return v, nil
	case FlagInt64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, flagError(f.Name, fmt.Sprintf("%q is not a valid integer", raw))
		}
		if f.Min != 0 || f.Max != 0 {
			if n < f.Min || n > f.Max {
				return nil, flagError(f.Name, fmt.Sprintf("%d is out of range [%d, %d]", n, f.Min, f.Max))
			}
		}
		return n, nil
	case FlagString:
		if len(f.Enum) > 0 && !slices.Contains(f.Enum, raw) {
			return nil, flagError(f.Name, fmt.Sprintf("%q is not one of %s", raw, strings.Join(f.Enum, "|")))
		}
		return raw, nil
	default:
		return nil, flagError(f.Name, fmt.Sprintf("internal: unknown flag kind %q", f.Kind))
	}
}

// zeroForKind is the Request value an unset flag falls back to when the
// registry itself declares no Default (ctx, db, config and out-dir: the
// empty string is exactly "not given" for every one of them).
func zeroForKind(kind string) any {
	switch kind {
	case FlagBool:
		return false
	case FlagInt64:
		return int64(0)
	default:
		return ""
	}
}

// Request carries one parsed invocation: the resolved command name,
// connection/output flags, every command-specific flag any registered
// command may need, and Explicit, which remembers which flag names the
// user actually typed - not merely which ones ended up with a non-zero
// value. --no-truncate must be rejected alongside an explicit --truncate
// even when that --truncate repeats its own default (200): a check that
// compared values instead of presence would miss exactly that case, the
// single most likely way a user triggers this conflict.
type Request struct {
	Command, ContextName, Database, ConfigPath, Format, OutDir string
	Object, Table, By, Aggregate, Since, Until                 string
	QueryID, PlanID, MinExecutions                             int64
	TimeoutSeconds, PreviewRows, TruncateRunes, Hours, Top     int
	NoTruncate, IncludeInternal, Summary                       bool
	Explicit                                                   map[string]bool
}

// Command is one registered command: its table contracts, the flags it
// accepts beyond the global set, how to run it, and what help must be
// able to render about it - examples, output units, required
// permissions and supported engine versions (design spec line 41). A
// registry that did not carry these itself would force help to
// reinvent them by hand for every command, the opposite of a single
// source of truth.
//
// Offline is true only for help: Run must never resolve a config path,
// an out-dir, or open a connection to run it, so that help keeps
// working on a machine with neither configured.
type Command struct {
	Name    string
	Tables  []model.TableSpec
	Flags   []Flag
	Execute func(ctx context.Context, s *sqlserver.Session, req Request, dst model.Sink) error

	Summary     string
	Examples    []string
	Units       []string
	Permissions []string
	Versions    []string
	Offline     bool
}

// Registry is the ordered list of every command this build of asq
// knows about. Task 9a's registry holds exactly three: help, info and
// qs status; later tasks append to it, never replace matchCommand's
// exact-name matching with anything resembling a prefix or an
// abbreviation.
type Registry []Command

// globalFlags are accepted by every connecting command (everything but
// help): --ctx, --db, --config, --format, --timeout, --preview,
// --truncate, --no-truncate, --out-dir. --ctx's requiredness is not
// expressed as a Flag field - it is a property of the command, not of
// the flag - see Command.Offline and Parse's check right after matching
// the command.
func globalFlags() []Flag {
	return []Flag{
		{Name: "ctx", Kind: FlagString},
		{Name: "db", Kind: FlagString},
		{Name: "config", Kind: FlagString},
		{Name: "format", Kind: FlagString, Enum: []string{"tsv", "json"}, Default: "tsv"},
		{Name: "timeout", Kind: FlagInt64, Default: int64(30), Min: 1, Max: 300},
		{Name: "preview", Kind: FlagInt64, Default: int64(10), Min: 0, Max: 10000},
		{Name: "truncate", Kind: FlagInt64, Default: int64(200), Min: 1, Max: 10000, ExclusiveWith: []string{"no-truncate"}},
		{Name: "no-truncate", Kind: FlagBool, Default: false, ExclusiveWith: []string{"truncate"}},
		{Name: "out-dir", Kind: FlagString},
	}
}

// NewRegistry builds the registry this build of asq serves: help, info
// and qs status. help's Execute closes over the finished registry
// through reg, a pointer to the slice NewRegistry is about to return:
// by the time help actually runs, reg has been fully populated by the
// composite literal below, even though help's own entry is built first.
func NewRegistry() Registry {
	reg := make(Registry, 3)
	reg[0] = helpCommand(&reg)
	reg[1] = infoCommand()
	reg[2] = qsStatusCommand()
	return reg
}

// helpCommandsTableSpec holds one row per registered command: its
// identity and the help-only fields that do not vary in number
// (summary, examples, units, permissions, versions, each flattened to
// a single "; "-joined string, because model.Cell carries scalars
// only). Flags live in helpFlagsTableSpec instead, one row per flag:
// see its own doc comment for why a command's flags cannot join this
// table as just another flattened string column.
var helpCommandsTableSpec = model.TableSpec{
	Name: "commands",
	Columns: []model.Column{
		{Name: "name", SQLType: "NVARCHAR"},
		{Name: "summary", SQLType: "NVARCHAR"},
		{Name: "examples", SQLType: "NVARCHAR"},
		{Name: "units", SQLType: "NVARCHAR"},
		{Name: "permissions", SQLType: "NVARCHAR"},
		{Name: "versions", SQLType: "NVARCHAR"},
	},
}

// helpFlagsTableSpec holds one row per flag accepted by any registered
// command (global flags repeated for every connecting command, plus
// each command's own). A flag's name, kind, default, bounds and enum
// each land in their own column, rather than one "; "-joined cell per
// command the way helpCommandsTableSpec's other list fields do: a
// command that accepts nine global flags plus several of its own -
// "qs top" will, once task 10 adds it - pushes a single flattened cell
// well past the project's 200-code-point preview cell limit, which
// truncates help's own inventory by default; the design spec (line 45)
// requires that inventory to carry "parameters, defaults, and
// examples" in full, so it cannot be the one output this project lets
// its own truncation guarantee quietly break. See
// TestHelpOfflineNoCellTruncated.
var helpFlagsTableSpec = model.TableSpec{
	Name: "flags",
	Columns: []model.Column{
		{Name: "command", SQLType: "NVARCHAR"},
		{Name: "flag", SQLType: "NVARCHAR"},
		{Name: "kind", SQLType: "NVARCHAR"},
		{Name: "default", SQLType: "NVARCHAR"},
		{Name: "min", SQLType: "INT"},
		{Name: "max", SQLType: "INT"},
		{Name: "enum", SQLType: "NVARCHAR"},
	},
}

// helpCommand builds the help command. reg is a pointer to the
// in-progress registry slice built by NewRegistry; Execute dereferences
// it only when actually called, by which point NewRegistry has finished
// populating every entry, help's own included.
func helpCommand(reg *Registry) Command {
	return Command{
		Name:     "help",
		Offline:  true,
		Summary:  "List every registered command and every flag it accepts. Works with no configuration file and no connection.",
		Examples: []string{"asq help", "asq help --json"},
		Versions: []string{"2019", "2022"},
		Tables:   []model.TableSpec{helpCommandsTableSpec, helpFlagsTableSpec},
		Flags:    []Flag{{Name: "json", Kind: FlagBool, Default: false}},
		Execute: func(_ context.Context, _ *sqlserver.Session, _ Request, dst model.Sink) error {
			return runHelp(*reg, dst)
		},
	}
}

// runHelp fills dst with one row per command (the commands table) and
// one row per flag any command accepts (the flags table). It never
// touches a file or a connection: everything it reads comes from the
// registry already held in memory.
func runHelp(reg Registry, dst model.Sink) error {
	if err := dst.Begin(helpCommandsTableSpec); err != nil {
		return err
	}
	for _, c := range reg {
		row := []model.Cell{
			c.Name,
			c.Summary,
			strings.Join(c.Examples, "; "),
			strings.Join(c.Units, "; "),
			strings.Join(c.Permissions, "; "),
			strings.Join(c.Versions, "; "),
		}
		if err := dst.Row(row); err != nil {
			return err
		}
	}
	if err := dst.End(true, true); err != nil {
		return err
	}

	if err := dst.Begin(helpFlagsTableSpec); err != nil {
		return err
	}
	for _, c := range reg {
		for _, f := range flagsFor(c) {
			var min, max model.Cell
			if f.Min != 0 || f.Max != 0 {
				min, max = f.Min, f.Max
			}
			row := []model.Cell{
				c.Name,
				f.Name,
				f.Kind,
				f.Default,
				min,
				max,
				strings.Join(f.Enum, "|"),
			}
			if err := dst.Row(row); err != nil {
				return err
			}
		}
	}
	return dst.End(true, true)
}

// flagsFor lists every flag c accepts: global flags first (unless c is
// offline, which accepts none of them), then c's own.
func flagsFor(c Command) []Flag {
	var flags []Flag
	if !c.Offline {
		flags = append(flags, globalFlags()...)
	}
	return append(flags, c.Flags...)
}

// notImplemented is the placeholder Execute for every command task 9b
// has not replaced yet: a stable, typed error (exit code 5, kind
// not_implemented) rather than a panic or a silent empty result, so a
// caller driving this build before 9b lands gets a clear, machine
// readable signal instead of a misleading success.
func notImplemented(name string) error {
	return &model.PublicError{Code: 5, Kind: "not_implemented", Message: fmt.Sprintf("%s is not implemented yet", name)}
}

// infoCommand registers "info". Its Tables and Execute are scaffolding
// for task 9b: the columns below follow the design spec's description
// (resolved identity, product version, edition, compatibility level,
// current principal) but are not binding on 9b's own SQL, which may
// adjust them once it writes the actual query.
func infoCommand() Command {
	return Command{
		Name:        "info",
		Summary:     "Resolved server/database identity, product version, edition, compatibility level, and current principal.",
		Examples:    []string{"asq --ctx client --db AppDB info"},
		Units:       []string{"compatibility_level: integer"},
		Permissions: []string{"CONNECT"},
		Versions:    []string{"2019", "2022"},
		Tables: []model.TableSpec{{
			Name: "identity",
			Columns: []model.Column{
				{Name: "server", SQLType: "NVARCHAR"},
				{Name: "database", SQLType: "NVARCHAR"},
				{Name: "principal", SQLType: "NVARCHAR"},
				{Name: "product_version", SQLType: "NVARCHAR"},
				{Name: "edition", SQLType: "NVARCHAR"},
				{Name: "compatibility_level", SQLType: "INT"},
			},
		}},
		Execute: func(context.Context, *sqlserver.Session, Request, model.Sink) error {
			return notImplemented("info")
		},
	}
}

// qsStatusCommand registers "qs status". Tables follow the design
// spec's declared order (status, coverage) and field list; like info's,
// they are 9a's scaffolding for 9b's actual query and may be adjusted
// there.
func qsStatusCommand() Command {
	return Command{
		Name:        "qs status",
		Summary:     "Desired/actual Query Store state, decoded read-only reason, capture mode, storage usage/limit, retention/interval settings, and stored interval coverage.",
		Examples:    []string{"asq --ctx client --db AppDB qs status"},
		Units:       []string{"current_storage_mb, max_storage_mb: MiB", "retention_days: days", "interval_minutes: minutes"},
		Permissions: []string{"VIEW DATABASE STATE"},
		Versions:    []string{"2019", "2022"},
		Tables: []model.TableSpec{
			{
				Name: "status",
				Columns: []model.Column{
					{Name: "desired_state", SQLType: "NVARCHAR"},
					{Name: "actual_state", SQLType: "NVARCHAR"},
					{Name: "readonly_reason", SQLType: "BIGINT"},
					{Name: "readonly_reason_decoded", SQLType: "NVARCHAR"},
					{Name: "capture_mode", SQLType: "NVARCHAR"},
					{Name: "current_storage_mb", SQLType: "DECIMAL(10,2)"},
					{Name: "max_storage_mb", SQLType: "DECIMAL(10,2)"},
					{Name: "retention_days", SQLType: "INT"},
					{Name: "interval_minutes", SQLType: "INT"},
				},
			},
			{
				Name: "coverage",
				Columns: []model.Column{
					{Name: "oldest_interval", SQLType: "DATETIME2"},
					{Name: "newest_interval", SQLType: "DATETIME2"},
					{Name: "has_history", SQLType: "BIT"},
				},
			},
		},
		Execute: func(context.Context, *sqlserver.Session, Request, model.Sink) error {
			return notImplemented("qs status")
		},
	}
}

// matchCommand finds, among r, the command whose Name exactly equals
// some leading run of bare (non-flag) tokens, joined by single spaces -
// trying the longest possible run first, then shorter ones, so a
// two-word command ("qs status") is preferred over a one-word command
// that happens to share its first token, and a bare token is never
// matched against a command name by a prefix or an abbreviation: only
// exact, whole-word equality ever resolves a command. It reports how
// many leading tokens of bare the match consumed, so the caller can
// treat anything after them as extra positional arguments.
func (r Registry) matchCommand(bare []string) (*Command, int, error) {
	if len(bare) == 0 {
		return nil, 0, &model.PublicError{Code: 2, Kind: "command", Message: "no command given"}
	}
	maxWords := 0
	for i := range r {
		if n := len(strings.Fields(r[i].Name)); n > maxWords {
			maxWords = n
		}
	}
	limit := maxWords
	if len(bare) < limit {
		limit = len(bare)
	}
	for n := limit; n >= 1; n-- {
		candidate := strings.Join(bare[:n], " ")
		for i := range r {
			if r[i].Name == candidate {
				return &r[i], n, nil
			}
		}
	}
	return nil, 0, &model.PublicError{Code: 2, Kind: "unknown_command", Message: fmt.Sprintf("unknown command: %s", strings.Join(bare, " "))}
}

// flagSuperset merges every flag this registry's commands declare,
// global flags included, into one name-keyed table - used only while
// tokenizing, to decide whether a "--name" the parser has not yet been
// able to attribute to a specific command takes a following value
// (anything but FlagBool does) before the command itself is known (see
// Parse's doc comment on why that order matters). The first definition
// of a given name wins; today no two commands in this registry define
// the same flag name with a different Kind.
func (r Registry) flagSuperset() map[string]Flag {
	all := map[string]Flag{}
	for _, f := range globalFlags() {
		all[f.Name] = f
	}
	for _, c := range r {
		for _, f := range c.Flags {
			if _, ok := all[f.Name]; !ok {
				all[f.Name] = f
			}
		}
	}
	return all
}

// checkExclusions reports the first ExclusiveWith conflict among flags
// actually given, per explicit - true regardless of which of the two
// conflicting names flags happens to declare the relationship on.
func checkExclusions(flags []Flag, explicit map[string]bool) error {
	for _, f := range flags {
		if !explicit[f.Name] {
			continue
		}
		for _, other := range f.ExclusiveWith {
			if explicit[other] {
				return &model.PublicError{
					Code:    2,
					Kind:    "flag_conflict",
					Message: fmt.Sprintf("--%s cannot be combined with --%s", f.Name, other),
				}
			}
		}
	}
	return nil
}
