package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// Both observed while restarting PostgreSQL under live ingest.
		{"dial refused", errors.New("failed to connect to `user=x database=y`: dial error: dial tcp 10.0.0.1:5432: connect: connection refused"), true},
		{"admin shutdown", &pgconn.PgError{Code: "57P01", Message: "terminating connection due to administrator command"}, true},

		{"crash shutdown", &pgconn.PgError{Code: "57P02"}, true},
		{"cannot connect now", &pgconn.PgError{Code: "57P03"}, true},
		{"connection exception", &pgconn.PgError{Code: "08006"}, true},
		{"reset mid-handshake", errors.New("failed to receive message: read tcp: read: connection reset by peer"), true},
		{"wrapped", fmt.Errorf("ingest: %w", &pgconn.PgError{Code: "57P01"}), true},

		// Rolled back for colliding with another transaction: retry works.
		{"deadlock", &pgconn.PgError{Code: "40P01"}, true},
		{"serialization failure", &pgconn.PgError{Code: "40001"}, true},

		// The caller's problem: retrying produces the same answer.
		{"integrity constraint (class 40, not a conflict)", &pgconn.PgError{Code: "40002"}, false},
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"syntax error", &pgconn.PgError{Code: "42601"}, false},
		{"permission denied", &pgconn.PgError{Code: "42501"}, false},
		{"plain error", errors.New("no such alert group"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := IsUnavailable(c.err); got != c.want {
			t.Errorf("%s: IsUnavailable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRetryConflicts(t *testing.T) {
	deadlock := &pgconn.PgError{Code: "40P01"}

	calls := 0
	err := retryConflicts(context.Background(), func() error {
		calls++
		if calls == 1 {
			return deadlock
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("a deadlock then success: err=%v calls=%d, want nil after 2", err, calls)
	}

	calls = 0
	err = retryConflicts(context.Background(), func() error { calls++; return deadlock })
	if !IsTransactionConflict(err) || calls != 3 {
		t.Fatalf("deadlock every time: err=%v calls=%d, want the deadlock after 3", err, calls)
	}

	calls = 0
	other := &pgconn.PgError{Code: "23505"}
	if err := retryConflicts(context.Background(), func() error { calls++; return other }); !errors.Is(err, other) || calls != 1 {
		t.Fatalf("a unique violation: err=%v calls=%d, want it at once", err, calls)
	}
}
