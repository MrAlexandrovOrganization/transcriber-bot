// Package testdb provisions only isolated synthetic schemas for integration tests.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func New(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TRANSCRIBER_BOT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TRANSCRIBER_BOT_TEST_DATABASE_URL to an isolated PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("isolated database connection failed")
	}
	random := make([]byte, 12)
	if _, err = rand.Read(random); err != nil {
		t.Fatal(err)
	}
	schema := "test_transcriber_" + hex.EncodeToString(random)
	name := pgx.Identifier{schema}.Sanitize()
	if _, err = conn.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "DROP SCHEMA "+name+" CASCADE"); err != nil {
			t.Error(err)
		}
		conn.Close(ctx)
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		endpoint, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid isolated database URL")
		}
		values := endpoint.Query()
		values.Set("search_path", schema)
		endpoint.RawQuery = values.Encode()
		return endpoint.String()
	}
	return dsn + " search_path=" + schema
}
