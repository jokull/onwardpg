package scratchdb

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// refusal builds the error PostgreSQL 15 through 18 return when the restricted
// role asks for an untrusted extension (see TestRefusalSourceLocationOnPostgreSQL).
func refusal(name string) *pgconn.PgError {
	return &pgconn.PgError{Code: "42501", Message: `permission denied to create extension "` + name + `"`, File: "extension.c", Routine: "execute_extension_script"}
}

func TestValidateAdminExtensions(t *testing.T) {
	valid := []AdminExtension{{Name: "earthdistance", Schema: "extensions"}, {Name: "uuid-ossp", Schema: "public", Version: "1.1"}, {Name: "pg_trgm", Schema: "Mixed Case", Version: "1.6-beta+2"}}
	if err := ValidateAdminExtensions(valid); err != nil {
		t.Fatal(err)
	}
	for label, entries := range map[string][]AdminExtension{
		"empty name":      {{Schema: "public"}},
		"empty schema":    {{Name: "cube"}},
		"upper case":      {{Name: "Cube", Schema: "public"}},
		"quote":           {{Name: `a"b`, Schema: "public"}},
		"space":           {{Name: "a b", Schema: "public"}},
		"leading hyphen":  {{Name: "-a", Schema: "public"}},
		"too long":        {{Name: strings.Repeat("a", 64), Schema: "public"}},
		"plpgsql":         {{Name: "plpgsql", Schema: "public"}},
		"pg_ schema":      {{Name: "cube", Schema: "pg_temp"}},
		"control in name": {{Name: "cube", Schema: "a\nb"}},
		"duplicate":       {{Name: "cube", Schema: "a"}, {Name: "cube", Schema: "a"}},
		"bad version":     {{Name: "cube", Schema: "a", Version: "1.0'; DROP"}},
		"version space":   {{Name: "cube", Schema: "a", Version: "1 0"}},
		"long version":    {{Name: "cube", Schema: "a", Version: strings.Repeat("1", 64)}},
	} {
		if err := ValidateAdminExtensions(entries); err == nil {
			t.Errorf("%s: invalid entries were accepted", label)
		}
	}
}

func TestNormalizeAdminExtensionsSortsACopy(t *testing.T) {
	input := []AdminExtension{{Name: "unaccent", Schema: "s"}, {Name: "cube", Schema: "s"}}
	got := NormalizeAdminExtensions(input)
	if got[0].Name != "cube" || input[0].Name != "unaccent" {
		t.Fatalf("normalized %v from %v", got, input)
	}
	if NormalizeAdminExtensions(nil) != nil || NormalizeAdminExtensions([]AdminExtension{}) != nil {
		t.Fatal("empty allowlists must normalize to nil so receipts omit them")
	}
}

func TestExplainDeniedNamesTheSettingOnlyForExtensionRefusals(t *testing.T) {
	denied := refusal("earthdistance")
	explained := ExplainDenied(fmt.Errorf("execute DDL: %w", denied))
	if !strings.Contains(explained.Error(), "scratch_admin_extensions") || !strings.Contains(explained.Error(), `permission denied to create extension "earthdistance"`) {
		t.Fatalf("explained = %v", explained)
	}
	if !errors.As(explained, new(*pgconn.PgError)) {
		t.Fatal("the original PostgreSQL error must stay in the chain")
	}
	for _, other := range []error{
		&pgconn.PgError{Code: "42501", Message: `permission denied for table "earthdistance"`},
		// Crafted by project SQL: right code and wording, wrong source location.
		&pgconn.PgError{Code: "42501", Message: `permission denied to create extension "earthdistance"`, File: "pl_exec.c", Routine: "exec_stmt_raise"},
		&pgconn.PgError{Code: "42501", Message: `permission denied to create extension "earthdistance"`},
		&pgconn.PgError{Code: "42501", Message: `permission denied to create extension "earthdistance"`, File: "extension.c", Routine: "CreateExtension"},
		&pgconn.PgError{Code: "42P07", Message: `relation "x" already exists`},
		errors.New(`permission denied to create extension "earthdistance"`),
		nil,
	} {
		if got := ExplainDenied(other); got != other {
			t.Errorf("unrelated error %v was rewritten to %v", other, got)
		}
	}
}

func TestRecoverIgnoresEverythingExceptListedRefusals(t *testing.T) {
	database := &Database{extensions: map[string]AdminExtension{"earthdistance": {Name: "earthdistance", Schema: "extensions"}}}
	for _, err := range []error{
		nil,
		errors.New("boom"),
		refusal("dblink"),
		&pgconn.PgError{Code: "23505", Message: `duplicate key "earthdistance"`, File: "extension.c", Routine: "execute_extension_script"},
		&pgconn.PgError{Code: "42501", Message: `permission denied for table "earthdistance"`, File: "pl_exec.c", Routine: "exec_stmt_raise"},
	} {
		recovered, recoverErr := database.Recover(t.Context(), err)
		if recovered || recoverErr != nil {
			t.Errorf("Recover(%v) = %v, %v", err, recovered, recoverErr)
		}
	}
	var none *Database
	if recovered, err := none.Recover(t.Context(), refusal("earthdistance")); recovered || err != nil {
		t.Fatalf("a nil database must never recover: %v %v", recovered, err)
	}
	if none.InstalledByAdministrator() != nil {
		t.Fatal("a nil database installed nothing")
	}
}
