package agent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/minio/minio-go/v7"
)

func TestTransient(t *testing.T) {
	pg := func(stderr string) error {
		return fmt.Errorf("step: %w", &toolError{tool: "pg_dump", stderr: stderr, err: errors.New("exit status 1")})
	}
	cases := []struct {
		name                 string
		err                  error
		session, connectOnly bool
	}{
		{"refused", pg(`pg_dump: error: connection to server at "db" (10.0.0.1), port 5432 failed: Connection refused`), true, true},
		{"starting up", pg(`psql: error: connection to server at "db", port 5432 failed: FATAL:  the database system is starting up`), true, true},
		{"too many clients", pg(`connection to server at "db", port 5432 failed: FATAL:  sorry, too many clients already`), true, true},
		{"dns", pg(`psql: error: could not translate host name "db" to address: Temporary failure in name resolution`), true, true},
		{"dns via connection", pg(`connection to server at "db" failed: could not translate host name "db" to address: Name or service not known`), true, true},
		{"lost mid-dump", pg(`pg_dump: error: Dumping the contents of table "t" failed: PQgetResult() failed. server closed the connection unexpectedly`), true, false},
		{"bad password", pg(`connection to server at "db", port 5432 failed: FATAL:  password authentication failed for user "x"`), false, false},
		{"missing database", pg(`connection to server at "db", port 5432 failed: FATAL:  database "nope" does not exist`), false, false},
		{"sql error", pg(`pg_restore: error: could not execute query: ERROR:  relation "t" already exists`), false, false},
		{"s3 throttled", minio.ErrorResponse{Code: "SlowDown", StatusCode: 503}, true, true},
		{"s3 denied", minio.ErrorResponse{Code: "AccessDenied", StatusCode: 403}, false, false},
	}
	for _, c := range cases {
		if got := transient(false)(c.err); got != c.session {
			t.Errorf("%s: transient(false) = %v, want %v", c.name, got, c.session)
		}
		if got := transient(true)(c.err); got != c.connectOnly {
			t.Errorf("%s: transient(true) = %v, want %v", c.name, got, c.connectOnly)
		}
	}
}
