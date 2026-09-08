// Package config loads YAML connection profiles and turns them into the SQL
// Server connection string the rest of argosql opens a session with.
package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rudi-bruchez/argosql/internal/model"
	yaml "go.yaml.in/yaml/v4"
)

const defaultPort = 1433

// Profile is a fully resolved connection profile: everything DSN needs to
// build a SQL Server connection string, with the secret already resolved
// from the environment. Never log or print Password; DSN itself must not be
// printed either.
type Profile struct {
	Host                   string
	Database               string
	Username               string
	Password               string
	CAFile                 string
	Port                   int
	TrustServerCertificate bool
}

// rawProfile mirrors the YAML shape of one profile. Pointer fields
// distinguish an absent key from an explicit zero value: only *bool can
// tell "trust_server_certificate absent" from "trust_server_certificate:
// false", which is why TrustServerCertificate below is not a plain bool.
type rawProfile struct {
	Host                   *string `yaml:"host"`
	Database               *string `yaml:"database"`
	Username               *string `yaml:"username"`
	PasswordEnv            *string `yaml:"password_env"`
	Port                   *int    `yaml:"port"`
	TrustServerCertificate *bool   `yaml:"trust_server_certificate"`
	CAFile                 *string `yaml:"ca_file"`
}

// rawConfig mirrors the YAML shape of the whole profile file. Any key
// outside this shape - including a literal "password" field - is rejected
// by strict decoding rather than special-cased here.
type rawConfig struct {
	Profiles map[string]rawProfile `yaml:"profiles"`
}

// configError builds the *model.PublicError every configuration failure in
// this package returns: exit code 2 (arguments/config), never wrapping the
// underlying YAML library error verbatim, since that error can quote raw
// document content.
func configError(message string) error {
	return &model.PublicError{Code: 2, Kind: "config", Message: message}
}

// Load reads the profile named name from the YAML file at path, resolves
// its secret through getenv, applies the TLS defaults (encrypt=true,
// TrustServerCertificate=true when the field is absent), and validates the
// result. databaseOverride, when non-empty, replaces the profile's database
// field. getenv is the caller's environment lookup: Load never reads a
// .env file itself.
func Load(path, name, databaseOverride string, getenv func(string) string) (Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, configError(fmt.Sprintf("cannot read config file %q: %v", path, err))
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		return Profile{}, configError(fmt.Sprintf("invalid profile file %q", path))
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Profile{}, configError(fmt.Sprintf("invalid profile file %q: unexpected content after the profiles document", path))
	}

	rp, ok := raw.Profiles[name]
	if !ok {
		return Profile{}, configError(fmt.Sprintf("profile %q not found in %q", name, path))
	}

	if rp.Host == nil || *rp.Host == "" {
		return Profile{}, configError("profile is missing host")
	}
	if rp.Username == nil || *rp.Username == "" {
		return Profile{}, configError("profile is missing username")
	}
	if rp.PasswordEnv == nil || *rp.PasswordEnv == "" {
		return Profile{}, configError("profile is missing password_env")
	}

	p := Profile{
		Host:     *rp.Host,
		Username: *rp.Username,
		Password: getenv(*rp.PasswordEnv),
		Port:     defaultPort,
	}

	if rp.Database != nil {
		p.Database = *rp.Database
	}
	if databaseOverride != "" {
		p.Database = databaseOverride
	}
	if p.Database == "" {
		return Profile{}, configError("no database: set database in the profile or pass an override")
	}

	if rp.Port != nil {
		p.Port = *rp.Port
	}
	if p.Port < 1 || p.Port > 65535 {
		return Profile{}, configError(fmt.Sprintf("port %d is out of range 1-65535", p.Port))
	}

	// TrustServerCertificate defaults to true, including when the field is
	// absent from the YAML: this is the project-wide TLS default, and there
	// is no fallback or confirmation prompt around it.
	p.TrustServerCertificate = true
	if rp.TrustServerCertificate != nil {
		p.TrustServerCertificate = *rp.TrustServerCertificate
	}

	if rp.CAFile != nil && *rp.CAFile != "" {
		caFile := *rp.CAFile
		if !filepath.IsAbs(caFile) {
			caFile = filepath.Join(filepath.Dir(path), caFile)
		}
		if err := validateCAFile(caFile, p.TrustServerCertificate); err != nil {
			return Profile{}, err
		}
		p.CAFile = caFile
	}

	return p, nil
}
