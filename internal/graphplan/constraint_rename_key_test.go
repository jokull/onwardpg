package graphplan

import (
	"strings"
	"testing"

	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

// keyRenameNames carries the constraint names of one referenced-key fixture:
// a primary key and a unique key on app.accounts, and a foreign key on
// app.orders that references the primary key's index.
type keyRenameNames struct {
	primary, unique, foreign string
}

type keyRenameFixture struct {
	primary, unique, foreign pgschema.Constraint
}

// keyRenameSnapshot builds the ordinary-table shape a PostgreSQL catalog
// produces: key constraints own an index of the same name, and a foreign key
// records the referenced key's index in UsingIndex.
func keyRenameSnapshot(t *testing.T, names keyRenameNames, foreignOnDelete pgschema.ForeignKeyAction) (*pgschema.Snapshot, keyRenameFixture) {
	t.Helper()
	snapshot := pgschema.New()
	schema := pgschema.Schema{Name: "app"}
	accounts, orders := pgschema.Table{Schema: "app", Name: "accounts"}, pgschema.Table{Schema: "app", Name: "orders"}
	primary := pgschema.Constraint{Table: accounts.ObjectID(), Name: names.primary, Type: pgschema.ConstraintPrimary, Definition: "PRIMARY KEY (id)", UsingIndex: names.primary, Validated: true}
	unique := pgschema.Constraint{Table: accounts.ObjectID(), Name: names.unique, Type: pgschema.ConstraintUnique, Definition: "UNIQUE (email)", UsingIndex: names.unique, Validated: true}
	foreign := pgschema.Constraint{
		Table: orders.ObjectID(), Name: names.foreign, Type: pgschema.ConstraintForeign, Definition: "FOREIGN KEY (account_id) REFERENCES app.accounts(id)",
		Reference: ptrID(accounts.ObjectID()), Validated: true, UsingIndex: names.primary,
		ForeignKeyColumns: []string{"account_id"}, ReferencedColumns: []string{"id"},
		ForeignKeyMatch: pgschema.ForeignKeyMatchSimple, ForeignKeyOnUpdate: pgschema.ForeignKeyNoAction, ForeignKeyOnDelete: foreignOnDelete,
		ForeignKeyEqualityOperators: []pgschema.ForeignKeyOperator{{Schema: "pg_catalog", Name: "="}},
	}
	primaryIndex := pgschema.Index{Table: accounts.ObjectID(), Name: names.primary, Unique: true, Primary: true, Method: "btree", Parts: []pgschema.IndexPart{{Column: "id"}}, Constraint: names.primary}
	uniqueIndex := pgschema.Index{Table: accounts.ObjectID(), Name: names.unique, Unique: true, Method: "btree", Parts: []pgschema.IndexPart{{Column: "email"}}, Constraint: names.unique}
	objects := []pgschema.Object{
		schema, accounts, orders,
		pgschema.Column{Table: accounts.ObjectID(), Name: "id", Position: 1, Type: "bigint", NotNull: true},
		pgschema.Column{Table: accounts.ObjectID(), Name: "email", Position: 2, Type: "text"},
		pgschema.Column{Table: orders.ObjectID(), Name: "account_id", Position: 1, Type: "bigint"},
		primary, unique, foreign, primaryIndex, uniqueIndex,
	}
	for _, object := range objects {
		if err := snapshot.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, edge := range [][2]pgschema.ID{
		{accounts.ObjectID(), schema.ObjectID()}, {orders.ObjectID(), schema.ObjectID()},
		{primary.ObjectID(), accounts.ObjectID()}, {unique.ObjectID(), accounts.ObjectID()},
		{foreign.ObjectID(), orders.ObjectID()}, {foreign.ObjectID(), accounts.ObjectID()}, {foreign.ObjectID(), primary.ObjectID()},
		{primaryIndex.ObjectID(), accounts.ObjectID()}, {uniqueIndex.ObjectID(), accounts.ObjectID()},
	} {
		if err := snapshot.AddDependency(edge[0], edge[1]); err != nil {
			t.Fatal(err)
		}
	}
	return snapshot, keyRenameFixture{primary: primary, unique: unique, foreign: foreign}
}

func renameConstraintAnswer(from, to pgschema.Constraint) protocol.Answer {
	return protocol.Answer{Kind: "rename_constraint", Key: from.ObjectID().String(), Value: to.ObjectID().String()}
}

func constraintRenameQuestionKeys(result protocol.Result) []string {
	var keys []string
	for _, question := range result.Questions {
		if question.Kind == "rename_constraint" {
			keys = append(keys, question.Key)
		}
	}
	return keys
}

func TestForeignKeyReferencingRenamedKeyIsOfferedAsRenameAndStaysMetadataOnly(t *testing.T) {
	current, before := keyRenameSnapshot(t, keyRenameNames{primary: "accounts_pk", unique: "accounts_email_unique", foreign: "orders_account_id_fk"}, pgschema.ForeignKeyNoAction)
	desired, after := keyRenameSnapshot(t, keyRenameNames{primary: "accounts_pkey", unique: "accounts_email_key", foreign: "orders_account_id_fkey"}, pgschema.ForeignKeyNoAction)

	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != protocol.NeedsInput || len(constraintRenameQuestionKeys(pending)) != 3 {
		t.Fatalf("primary key, unique key, and the foreign key referencing the renamed index must all be offered in one round: %#v", pending)
	}

	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{
			renameConstraintAnswer(before.primary, after.primary),
			renameConstraintAnswer(before.unique, after.unique),
			renameConstraintAnswer(before.foreign, after.foreign),
		},
	}
	planned, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if planned.Status != protocol.Planned {
		t.Fatalf("confirmed renames must plan: %#v", planned)
	}
	var statements []string
	for _, statement := range planned.Statements {
		statements = append(statements, statement.SQL)
	}
	want := []string{
		`ALTER TABLE "app"."accounts" RENAME CONSTRAINT "accounts_email_unique" TO "accounts_email_key";`,
		`ALTER TABLE "app"."accounts" RENAME CONSTRAINT "accounts_pk" TO "accounts_pkey";`,
		`ALTER TABLE "app"."orders" RENAME CONSTRAINT "orders_account_id_fk" TO "orders_account_id_fkey";`,
	}
	if strings.Join(statements, "\n") != strings.Join(want, "\n") {
		t.Fatalf("plan must contain only the three metadata renames, got:\n%s", strings.Join(statements, "\n"))
	}
}

func TestForeignKeyRenameAnswerBeforeItsKeyIsAnsweredWaitsForTheKey(t *testing.T) {
	current, before := keyRenameSnapshot(t, keyRenameNames{primary: "accounts_pk", unique: "accounts_email_unique", foreign: "orders_account_id_fk"}, pgschema.ForeignKeyNoAction)
	desired, after := keyRenameSnapshot(t, keyRenameNames{primary: "accounts_pkey", unique: "accounts_email_key", foreign: "orders_account_id_fkey"}, pgschema.ForeignKeyNoAction)
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	answers := protocol.Answers{
		CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint,
		Answers: []protocol.Answer{renameConstraintAnswer(before.foreign, after.foreign), renameConstraintAnswer(before.unique, after.unique)},
	}
	result, err := Build(current, desired, answers, Options{})
	if err != nil {
		t.Fatal(err)
	}
	keys := constraintRenameQuestionKeys(result)
	if result.Status != protocol.NeedsInput || len(keys) != 1 || keys[0] != before.primary.ObjectID().String() {
		t.Fatalf("only the unanswered primary key may remain pending: %#v", result)
	}
}

func TestForeignKeyIsNotARenameCandidateWhenItsKeyRenameIsDeclined(t *testing.T) {
	current, before := keyRenameSnapshot(t, keyRenameNames{primary: "accounts_pk", unique: "accounts_email_unique", foreign: "orders_account_id_fk"}, pgschema.ForeignKeyNoAction)
	desired, after := keyRenameSnapshot(t, keyRenameNames{primary: "accounts_pkey", unique: "accounts_email_key", foreign: "orders_account_id_fkey"}, pgschema.ForeignKeyNoAction)
	pending, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	fingerprints := protocol.Answers{CurrentFingerprint: pending.CurrentFingerprint, DesiredFingerprint: pending.DesiredFingerprint}

	declined := fingerprints
	declined.Answers = []protocol.Answer{
		{Kind: "rename_constraint", Key: before.primary.ObjectID().String(), Value: "create"},
		renameConstraintAnswer(before.unique, after.unique),
	}
	result, err := Build(current, desired, declined, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range constraintRenameQuestionKeys(result) {
		if key == before.foreign.ObjectID().String() {
			t.Fatalf("a foreign key must not be offered as a rename when its referenced key is recreated: %#v", result)
		}
	}
	if strings.Contains(joinSQL(result), "RENAME CONSTRAINT") && strings.Contains(joinSQL(result), "orders_account_id_fk") {
		t.Fatalf("the foreign key must not be renamed when its referenced key is recreated:\n%s", joinSQL(result))
	}

	contradictory := declined
	contradictory.Answers = append(append([]protocol.Answer(nil), declined.Answers...), renameConstraintAnswer(before.foreign, after.foreign))
	if _, err := Build(current, desired, contradictory, Options{}); err == nil || !strings.Contains(err.Error(), "unused answers") {
		t.Fatalf("a foreign key rename answer with a declined key rename must be rejected as unused, got %v", err)
	}
}

func TestForeignKeyDefinitionDifferencesAreNeverAbsorbedIntoAKeyDrivenRename(t *testing.T) {
	current, before := keyRenameSnapshot(t, keyRenameNames{primary: "accounts_pk", unique: "accounts_email_unique", foreign: "orders_account_id_fk"}, pgschema.ForeignKeyNoAction)
	desired, _ := keyRenameSnapshot(t, keyRenameNames{primary: "accounts_pkey", unique: "accounts_email_key", foreign: "orders_account_id_fkey"}, pgschema.ForeignKeyCascade)
	result, err := Build(current, desired, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range constraintRenameQuestionKeys(result) {
		if key == before.foreign.ObjectID().String() {
			t.Fatalf("a foreign key whose ON DELETE action changed must not be offered as a rename: %#v", result)
		}
	}
}

func TestForeignKeyIsNotARenameCandidateWhenItsReferencedKeyAlsoChangesDefinition(t *testing.T) {
	current, before := keyRenameSnapshot(t, keyRenameNames{primary: "accounts_pk", unique: "accounts_email_unique", foreign: "orders_account_id_fk"}, pgschema.ForeignKeyNoAction)
	desired, _ := keyRenameSnapshot(t, keyRenameNames{primary: "accounts_pkey", unique: "accounts_email_key", foreign: "orders_account_id_fkey"}, pgschema.ForeignKeyNoAction)
	primary, ok := desired.Object(before.primary.ObjectID())
	if ok {
		t.Fatalf("fixture must not share key identities: %#v", primary)
	}
	changed := (pgschema.Constraint{Table: before.primary.Table, Name: "accounts_pkey"}).ObjectID()
	object, ok := desired.Object(changed)
	if !ok {
		t.Fatal("desired primary key missing")
	}
	key := object.(pgschema.Constraint)
	key.Definition = "PRIMARY KEY (id, email)"
	replaced := pgschema.New()
	for _, item := range desired.Objects() {
		if item.ObjectID() == changed {
			item = key
		}
		if err := replaced.Add(item); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range desired.Objects() {
		for _, dependency := range desired.Dependencies(item.ObjectID()) {
			if err := replaced.AddDependency(item.ObjectID(), dependency); err != nil {
				t.Fatal(err)
			}
		}
	}
	result, err := Build(current, replaced, protocol.Answers{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range constraintRenameQuestionKeys(result) {
		if key == before.foreign.ObjectID().String() || key == before.primary.ObjectID().String() {
			t.Fatalf("neither the changed key nor the foreign key depending on it may be offered as a rename: %#v", result)
		}
	}
}
