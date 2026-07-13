package graphplan

import (
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

func TestQuestionScopeIgnoresUnrelatedObjectsAndTracksParticipants(t *testing.T) {
	question := protocol.Question{
		Kind: "rename_table", Key: "table:app:old_users",
		Choices: []string{"table:app:users", "create"},
	}
	current := scopeSnapshot(t, pgschema.Table{Schema: "app", Name: "old_users"})
	desired := scopeSnapshot(t, pgschema.Table{Schema: "app", Name: "users"})
	initial := questionScopeFingerprint(question, current, desired)
	if initial == "" {
		t.Fatal("question scope fingerprint is empty")
	}

	currentWithUnrelated := scopeSnapshot(t,
		pgschema.Table{Schema: "app", Name: "old_users"},
		pgschema.Table{Schema: "app", Name: "audit_log"},
	)
	desiredWithUnrelated := scopeSnapshot(t,
		pgschema.Table{Schema: "app", Name: "users"},
		pgschema.Table{Schema: "app", Name: "audit_log"},
	)
	if got := questionScopeFingerprint(question, currentWithUnrelated, desiredWithUnrelated); got != initial {
		t.Fatalf("unrelated table changed question scope: %s != %s", got, initial)
	}

	changedDesired := scopeSnapshot(t, pgschema.Table{Schema: "app", Name: "users", Unlogged: true})
	if got := questionScopeFingerprint(question, current, changedDesired); got == initial {
		t.Fatal("participating table change did not invalidate question scope")
	}
}

func scopeSnapshot(t *testing.T, tables ...pgschema.Table) *pgschema.Snapshot {
	t.Helper()
	snapshot := pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	if err := snapshot.Add(schema); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if err := snapshot.Add(table); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.AddDependency(table.ObjectID(), schema.ObjectID()); err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}
