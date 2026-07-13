package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/internal/source"
)

func TestSourceReceiptRedactsDatabaseURLAndAbsoluteDDLPath(t *testing.T) {
	database := sourceReceipt(source.Parse("postgres://user:secret@db.example/app"), "sha256:"+repeat("a", 64), "current")
	data, err := json.Marshal(database)
	if err != nil {
		t.Fatal(err)
	}
	if contains(string(data), "secret") || contains(string(data), "db.example") || database.Description != "current database" {
		t.Fatalf("database receipt leaked source URL: %s", data)
	}
	ddl := sourceReceipt(source.Parse("file:///private/work/schema.sql"), "sha256:"+repeat("b", 64), "desired")
	if ddl.Description != "desired DDL schema.sql" {
		t.Fatalf("DDL description = %q", ddl.Description)
	}
}

func TestVersionedDiagnosticContract(t *testing.T) {
	diagnostic := protocol.ErrorDiagnostic("invalid_invocation", errors.New("bad flags"))
	data, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), protocol.DiagnosticVersion) || !contains(string(data), `"code":"invalid_invocation"`) {
		t.Fatalf("diagnostic = %s", data)
	}
}

func repeat(value string, count int) string {
	result := ""
	for range count {
		result += value
	}
	return result
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
