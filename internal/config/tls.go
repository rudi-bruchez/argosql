package config

import (
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// validateCAFile rejects, before any DSN is built, everything that
// github.com/microsoft/go-mssqldb v1.11.0 would only discover once it is
// already trying to connect. Measured: msdsn.Parse's readCertificate
// dispatches on the file extension and accepts only ".pem" and ".der"; a
// perfectly valid PEM file named "ca.crt", "ca.cer", or with no extension at
// all fails at connection time with "certificate type .crt is not
// supported" - a code 3 - when the spec requires a code 2 before any
// connection is attempted. The driver also calls x509.CertPool's
// AppendCertsFromPEM without checking its return value, so a ".pem" file
// with no valid certificate in it silently produces an empty pool and only
// fails once TLS is negotiated. This function reads and parses the file
// itself so both cases surface as a code 2 configuration error instead.
func validateCAFile(caFile string, trustServerCertificate bool) error {
	// Checked before the extension: trust_server_certificate defaults to
	// true, so most operators who hit this never wrote "true" themselves,
	// and the message must not accuse them of a line they didn't write.
	// Checking the contradiction first also means a ca_file with both a
	// wrong extension and no trust_server_certificate: false gets one
	// actionable error instead of two round trips.
	if trustServerCertificate {
		return configError("ca_file requires trust_server_certificate: false; trust_server_certificate is true by default, and under it the certificate is never validated")
	}

	ext := strings.ToLower(filepath.Ext(caFile))
	if ext != ".pem" && ext != ".der" {
		return configError(fmt.Sprintf("ca_file must have extension .pem or .der, got %q", ext))
	}

	data, err := os.ReadFile(caFile)
	if err != nil {
		return configError(fmt.Sprintf("cannot read ca_file %q: %v", caFile, err))
	}

	switch ext {
	case ".pem":
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return configError(fmt.Sprintf("ca_file %q contains no usable certificate", caFile))
		}
	case ".der":
		if _, err := x509.ParseCertificate(data); err != nil {
			return configError(fmt.Sprintf("ca_file %q is not a valid DER certificate: %v", caFile, err))
		}
	}

	return nil
}

// DSN renders p as a sqlserver:// connection string. It is internal: never
// print, log, or otherwise surface its result, since it carries the
// resolved secret in the URL's userinfo.
func DSN(p Profile) (string, error) {
	q := url.Values{
		"encrypt":                {"true"},
		"TrustServerCertificate": {strconv.FormatBool(p.TrustServerCertificate)},
		"database":               {p.Database},
	}
	if p.CAFile != "" {
		q.Set("certificate", p.CAFile)
	}
	u := url.URL{
		Scheme:   "sqlserver",
		Host:     net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
		User:     url.UserPassword(p.Username, p.Password),
		RawQuery: q.Encode(),
	}
	return u.String(), nil
}
