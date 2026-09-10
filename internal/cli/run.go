package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/rudi-bruchez/argosql/internal/artifacts"
	"github.com/rudi-bruchez/argosql/internal/config"
	"github.com/rudi-bruchez/argosql/internal/model"
	"github.com/rudi-bruchez/argosql/internal/output"
	"github.com/rudi-bruchez/argosql/internal/sqlserver"
)

// stdoutByteLimit, collectionRowLimit and collectionByteLimit are the
// project-wide bounds the design spec fixes for every invocation: at
// most 32,768 bytes (newline included) ever written to stdout, and at
// most 10,000 collected rows / 104,857,600 bytes of artifacts per run,
// manifest included.
const (
	stdoutByteLimit     = 32768
	collectionRowLimit  = 10000
	collectionByteLimit = 104857600
)

// offlineDefaultPreviewRows is runOffline's row cap when the caller did
// not pass an explicit --preview: generous enough that help's own
// registry metadata (a handful of commands, each with a handful of
// flags) is never cut by it, while the project-wide stdout byte cap
// still applies underneath it as the real safety net.
const offlineDefaultPreviewRows = 1000

// configLoader and sessionOpener are config.Load's and sqlserver.Open's
// own signatures, named so run (below) can take a fake of either
// without a real config file or a real network connection: that is how
// TestRunEmitsUnvalidatedVersionNotice below exercises Run's Major==17
// notice without needing a live SQL Server.
type configLoader func(path, name, databaseOverride string, getenv func(string) string) (config.Profile, error)
type sessionOpener func(ctx context.Context, p config.Profile) (*sqlserver.Session, error)

// Run is the one function cmd/asq calls: it parses args, and for an
// offline command (help) renders its result with no file or network
// access at all; for every other command it resolves the deferred
// config/out-dir defaults, loads the profile, opens and holds the
// session, dispatches the command, and always renders and writes stdout
// exactly once, even when the command itself failed - the error is
// still reported, with its own exit code, after a best-effort bounded
// result. It never panics: every internal failure becomes a
// model.PublicError and a matching exit code instead.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return run(ctx, args, stdout, stderr, config.Load, sqlserver.Open)
}

// run is Run's real body, parameterized over config loading and
// session opening purely for testability (see configLoader and
// sessionOpener above); Run always calls it with the real
// implementations.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, loadConfig configLoader, openSession sessionOpener) int {
	req, cmd, err := Parse(args)
	if err != nil {
		return emitError(stdout, stderr, req, err, config.Profile{})
	}

	if cmd.Offline {
		return runOffline(*cmd, req, stdout, stderr)
	}

	if req.ConfigPath == "" {
		path, perr := defaultConfigPath()
		if perr != nil {
			return emitError(stdout, stderr, req, perr, config.Profile{})
		}
		req.ConfigPath = path
	}
	if req.OutDir == "" {
		dir, perr := defaultOutDir()
		if perr != nil {
			return emitError(stdout, stderr, req, perr, config.Profile{})
		}
		req.OutDir = dir
	}

	profile, err := loadConfig(req.ConfigPath, req.ContextName, req.Database, os.Getenv)
	if err != nil {
		return emitError(stdout, stderr, req, err, config.Profile{})
	}

	// The overall deadline covers connection, setup, probes, and result
	// consumption (design spec: "--timeout ... its clock starts before
	// connection acquisition"). runCtx.Err() is how the rest of this
	// function tells an internal timeout (context.DeadlineExceeded) from
	// a caller signal that canceled ctx itself before the deadline
	// (context.Canceled propagates from ctx to runCtx unchanged) -
	// "séparer expiration interne et signal utilisateur" without any
	// extra bookkeeping.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
	defer cancel()

	sess, err := openSession(runCtx, profile)
	if err != nil {
		return emitErrorCtx(runCtx, stdout, stderr, req, err, profile)
	}
	defer sess.Close()

	limits := artifacts.Limits{Rows: collectionRowLimit, Bytes: collectionByteLimit}
	collector, err := artifacts.New(req.OutDir, req.Format, limits)
	if err != nil {
		return emitErrorCtx(runCtx, stdout, stderr, req, err, profile)
	}

	// Run, not Open and not the command itself, is where this notice
	// belongs: Session has no Sink to report through, and the notice is
	// true of every command dispatched against an unvalidated major
	// version, not just info.
	if sess.Major == 17 {
		collector.Notice(model.Notice{Kind: "unvalidated_version"})
	}

	execErr := cmd.Execute(runCtx, sess, req, collector)

	info := model.ContextInfo{
		Server:                profile.Host,
		Database:              profile.Database,
		Principal:             profile.Username,
		Version:               fmt.Sprintf("%d", sess.Major),
		TLSEncryption:         "required",
		CertificateValidation: certValidation(profile),
		CollectedAt:           time.Now().UTC(),
	}
	result, finishErr := collector.Finish(info, execErr)
	finalErr := execErr
	if finishErr != nil {
		// A later file/serialization error always overrides an earlier
		// collection error - the project rule Finish itself documents.
		finalErr = finishErr
	}
	// result.Error carries execErr's own message (Collector.Finish built
	// it from the unredacted runErr it was handed) straight into the JSON
	// envelope Render produces below. redact already applies to stderr
	// (further down); without this, a message containing the connection
	// secret - a driver error that echoes a DSN, or any error string
	// that happens to embed it - reaches stdout in the clear while
	// stderr masks the exact same text. Fix 1's A1.
	result.Error = redactPublicError(result.Error, profile)

	out, renderErr := output.Render(result, previewOptionsFrom(req), req.Format)
	if renderErr != nil {
		return emitErrorCtx(runCtx, stdout, stderr, req, renderErr, profile)
	}
	// Design spec line 210, verbatim: "A later output failure uses code
	// 6 even if the collected data was already partial." A failed
	// stdout write is exactly that: nothing was actually delivered, so
	// this exit code wins over whatever finalErr would otherwise map
	// to, rather than silently claiming finalErr's own code (often 0)
	// for a response the caller never received.
	if _, werr := stdout.Write(out); werr != nil {
		fmt.Fprintln(stderr, werr.Error())
		return exitCodeFor(runCtx, writeOutputError(werr))
	}

	if finalErr != nil {
		fmt.Fprintln(stderr, redact(finalErr.Error(), profile))
	}
	return exitCodeFor(runCtx, finalErr)
}

// runOffline runs an offline command (help): an in-memory Sink, no
// file, no connection, no deferred default resolved. It is the only
// path through this package that may run when os.UserConfigDir and
// os.UserCacheDir both fail.
//
// It takes no config.Profile, and that is why its own result.Error is
// the one PublicError reaching stdout that redactPublicError does not
// touch: this path never loads a profile, so no password exists in this
// process to leak, and there is nothing to redact against. Do not
// "fix" that asymmetry by threading a profile in here - that would
// create the very exposure the redaction exists to prevent, by making
// a secret reachable from a command that has no use for one.
func runOffline(cmd Command, req Request, stdout, stderr io.Writer) int {
	sink := &memSink{}
	execErr := cmd.Execute(context.Background(), nil, req, sink)

	result := sink.result
	result.SchemaVersion = 1
	result.OK = execErr == nil
	result.Error = publicErrorOf(execErr)

	options := previewOptionsFrom(req)
	if !req.Explicit["preview"] {
		// An offline command's whole result is its registry metadata,
		// already in memory and small by construction: the general
		// --preview default of 10 rows exists to bound a SQL result set
		// a caller has not asked to see in full, not to cap help's own
		// inventory. Measured: with the 9 global flags alone, help's
		// "flags" table already holds 19 rows across 3 commands, and
		// the default --preview would have silently dropped "qs
		// status"'s flags from help's own output, omitted_reasons
		// row_limit, with no cell ever near its byte cap. An explicit
		// --preview is still honored: only the silent default changes.
		options.Rows = offlineDefaultPreviewRows
	}
	out, renderErr := output.Render(result, options, req.Format)
	if renderErr != nil {
		fmt.Fprintln(stderr, renderErr.Error())
		return model.ExitCode(renderErr)
	}
	// Same rule as run's own stdout.Write, below: design spec line 210,
	// a later output failure is code 6 regardless of what the command
	// itself produced.
	if _, werr := stdout.Write(out); werr != nil {
		fmt.Fprintln(stderr, werr.Error())
		return model.ExitCode(writeOutputError(werr))
	}

	if execErr != nil {
		fmt.Fprintln(stderr, execErr.Error())
	}
	return model.ExitCode(execErr)
}

// previewOptionsFrom derives Render's bounds from a parsed Request:
// --preview, --truncate and --no-truncate control the first three
// fields; the stdout byte cap itself is fixed project-wide and is never
// something a flag can relax.
func previewOptionsFrom(req Request) output.PreviewOptions {
	return output.PreviewOptions{
		Rows:       req.PreviewRows,
		CellLimit:  req.TruncateRunes,
		NoTruncate: req.NoTruncate,
		ByteLimit:  stdoutByteLimit,
	}
}

// certValidation reports ContextInfo's certificate_validation fact from
// a profile that a connection has already been opened with
// successfully: "skipped" under the project's TrustServerCertificate
// default, "verified" when the profile asked for chain/host validation
// - which, if the connection succeeded at all, it already passed.
func certValidation(p config.Profile) string {
	if p.TrustServerCertificate {
		return "skipped"
	}
	return "verified"
}

// exitCodeFor is model.ExitCode, refined by runCtx: when runCtx was
// canceled for a reason other than its own deadline expiring - the only
// way that happens is the caller's ctx (signal.NotifyContext's, in
// production) being canceled first - this run was interrupted by the
// user, exit code 130, regardless of what err would otherwise map to.
func exitCodeFor(runCtx context.Context, err error) int {
	if err != nil && errors.Is(runCtx.Err(), context.Canceled) {
		return 130
	}
	return model.ExitCode(err)
}

// redact replaces every occurrence of p's resolved secret in msg before
// it reaches stderr. p.Password is empty whenever no profile was loaded
// yet (an argument or config error, before any secret exists to leak),
// in which case this is a no-op.
func redact(msg string, p config.Profile) string {
	if p.Password == "" {
		return msg
	}
	return strings.ReplaceAll(msg, p.Password, "REDACTED")
}

// redactPublicError returns a copy of pe with Message passed through
// redact, or nil when pe is nil. Fix 1's A1: every *model.PublicError
// that can reach a JSON envelope (stdout) must be redacted exactly
// like stderr already is - stdout and stderr must never disagree on
// whether a secret is visible. A copy, never a mutation of pe itself:
// errors.As (inside publicErrorOf) can hand back the same *PublicError
// value a caller still holds elsewhere.
func redactPublicError(pe *model.PublicError, p config.Profile) *model.PublicError {
	if pe == nil {
		return nil
	}
	out := *pe
	out.Message = redact(out.Message, p)
	return &out
}

// writeOutputError wraps a failed stdout write as the project's own
// output-failure contract: design spec line 210, verbatim, "A later
// output failure uses code 6 even if the collected data was already
// partial." This always wins over whatever exit code the command's
// own result would otherwise map to, because nothing was actually
// delivered to the caller.
func writeOutputError(err error) *model.PublicError {
	return &model.PublicError{Code: 6, Kind: "output", Message: fmt.Sprintf("writing stdout: %s", err.Error())}
}

// publicErrorOf extracts the *model.PublicError behind err (at any
// wrapping depth), or synthesizes a generic code-5 one when err is
// non-nil but not a PublicError, or returns nil for a nil err.
func publicErrorOf(err error) *model.PublicError {
	if err == nil {
		return nil
	}
	var pub *model.PublicError
	if errors.As(err, &pub) {
		return pub
	}
	return &model.PublicError{Code: 5, Kind: "execution", Message: err.Error()}
}

// emitError reports err on stderr and, when req.Format is "json", also
// writes a bounded JSON error envelope to stdout - the design spec's
// "JSON errors use the same envelope with ok: false" applied to an
// error raised before any connection (and so before runCtx, hence the
// plain model.ExitCode here rather than exitCodeFor: an argument or
// config error can never be a user interruption).
func emitError(stdout, stderr io.Writer, req Request, err error, profile config.Profile) int {
	if werr := writeError(stdout, stderr, req, err, profile); werr != nil {
		return model.ExitCode(werr)
	}
	return model.ExitCode(err)
}

// emitErrorCtx is emitError for a failure that happened after runCtx
// existed (connection, collection, or render), so its exit code goes
// through exitCodeFor and can come back 130 on user interruption.
func emitErrorCtx(runCtx context.Context, stdout, stderr io.Writer, req Request, err error, profile config.Profile) int {
	if werr := writeError(stdout, stderr, req, err, profile); werr != nil {
		return exitCodeFor(runCtx, werr)
	}
	return exitCodeFor(runCtx, err)
}

// writeError is emitError's and emitErrorCtx's shared body. It returns
// a non-nil *model.PublicError only when the stdout write itself
// failed (design spec line 210: code 6 wins over whatever err would
// otherwise map to); a nil return means the caller should keep using
// its own err for the exit code.
func writeError(stdout, stderr io.Writer, req Request, err error, profile config.Profile) *model.PublicError {
	fmt.Fprintln(stderr, redact(err.Error(), profile))
	if !strings.EqualFold(req.Format, "json") {
		return nil
	}
	// Fix 1's A1: publicErrorOf(err) can still carry the unredacted
	// secret (a driver error that echoed a DSN, or any message that
	// happens to contain it) - redactPublicError is what keeps this
	// JSON envelope in agreement with the line above, which already
	// redacts the same err for stderr.
	fe := model.FallbackError{SchemaVersion: 1, OK: false, Error: redactPublicError(publicErrorOf(err), profile)}
	b, merr := json.Marshal(fe)
	if merr != nil {
		return nil
	}
	if _, werr := stdout.Write(append(b, '\n')); werr != nil {
		return writeOutputError(werr)
	}
	return nil
}

// memSink is the model.Sink Run uses for an offline command: every row
// is kept in memory, nothing is ever written to disk, and File always
// fails - help has no artifact to produce, so a command wired to this
// Sink that tried to write one would be a defect, not a legitimate path.
type memSink struct {
	result model.Result
	cur    *model.TableResult
}

func (m *memSink) Begin(spec model.TableSpec) error {
	m.cur = &model.TableResult{Spec: spec}
	return nil
}

func (m *memSink) Row(row []model.Cell) error {
	if m.cur == nil {
		return fmt.Errorf("cli: Row called with no open table")
	}
	m.cur.Rows = append(m.cur.Rows, row)
	m.cur.State.RowsCollected++
	return nil
}

func (m *memSink) End(collectionComplete, propertiesComplete bool) error {
	if m.cur == nil {
		return fmt.Errorf("cli: End called with no open table")
	}
	m.cur.State.CollectionComplete = collectionComplete
	m.cur.State.PropertiesComplete = propertiesComplete
	m.result.Tables = append(m.result.Tables, *m.cur)
	m.cur = nil
	return nil
}

func (m *memSink) File(kind, suffix string, src io.Reader) (model.Artifact, error) {
	return model.Artifact{}, fmt.Errorf("cli: offline commands cannot write artifacts")
}

func (m *memSink) Notice(n model.Notice) {
	m.result.Notices = append(m.result.Notices, n)
}
