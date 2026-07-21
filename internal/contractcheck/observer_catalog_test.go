package contractcheck

import (
	"reflect"
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

func TestProjectObserverSnapshotNormalizesOnlyOwnerAndObserverOverlay(t *testing.T) {
	snapshot := pgschema.New()
	ownedTable := pgschema.Table{Schema: "app", Name: "orders", Owner: "app_owner"}
	externalTable := pgschema.Table{Schema: "app", Name: "audit", Owner: "audit_owner"}
	ownedView := pgschema.View{Schema: "app", Name: "order_summary", Owner: "app_owner"}
	externalView := pgschema.View{Schema: "app", Name: "audit_summary", Owner: "audit_owner"}
	observerGrant := pgschema.TablePrivilege{Table: ownedTable.ObjectID(), Grantee: "onwardpg_reader", Grantor: "@owner", Privilege: "SELECT"}
	applicationGrant := pgschema.TablePrivilege{Table: ownedTable.ObjectID(), Grantee: "application_reader", Grantor: "@owner", Privilege: "SELECT"}
	for _, object := range []pgschema.Object{ownedTable, externalTable, ownedView, externalView, observerGrant, applicationGrant} {
		if err := snapshot.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, grant := range []pgschema.TablePrivilege{observerGrant, applicationGrant} {
		if err := snapshot.AddDependency(grant.ObjectID(), grant.Table); err != nil {
			t.Fatal(err)
		}
	}
	if err := snapshot.AddUnsupported("acl:schema:app"); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.AddUnsupported("ownership:schema:shared=audit_owner"); err != nil {
		t.Fatal(err)
	}

	projected, access, finding, err := projectObserverSnapshot(snapshot, observerContext{
		Role: "onwardpg_reader", DatabaseOwner: "app_owner",
		AccessRoles:           map[string]bool{"onwardpg_reader": true},
		ContextualUnsupported: map[string]bool{"acl:schema:app": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if finding != nil {
		t.Fatalf("finding = %#v", finding)
	}
	wantAccess := []string{"acl:schema:app", observerGrant.ObjectID().String()}
	if !reflect.DeepEqual(access, wantAccess) {
		t.Fatalf("projected access = %#v, want %#v", access, wantAccess)
	}
	assertOwner := func(id pgschema.ID, want string) {
		t.Helper()
		object, exists := projected.Object(id)
		if !exists {
			t.Fatalf("missing projected object %s", id)
		}
		var got string
		switch value := object.(type) {
		case pgschema.Table:
			got = value.Owner
		case pgschema.View:
			got = value.Owner
		default:
			t.Fatalf("projected object %s has type %T", id, object)
		}
		if got != want {
			t.Fatalf("projected owner for %s = %q, want %q", id, got, want)
		}
	}
	assertOwner(ownedTable.ObjectID(), "")
	assertOwner(ownedView.ObjectID(), "")
	assertOwner(externalTable.ObjectID(), "audit_owner")
	assertOwner(externalView.ObjectID(), "audit_owner")
	if _, exists := projected.Object(observerGrant.ObjectID()); exists {
		t.Fatal("observer SELECT overlay remains in projected snapshot")
	}
	if _, exists := projected.Object(applicationGrant.ObjectID()); !exists {
		t.Fatal("genuine application SELECT was projected away")
	}
	if got := projected.Unsupported(); !reflect.DeepEqual(got, []string{"ownership:schema:shared=audit_owner"}) {
		t.Fatalf("projected unsupported selectors = %#v", got)
	}
}

func TestProjectObserverSnapshotRejectsUnsafeObserverPrivilege(t *testing.T) {
	tests := []struct {
		name      string
		privilege string
		grantable bool
	}{
		{name: "grant option", privilege: "SELECT", grantable: true},
		{name: "write privilege", privilege: "INSERT"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := pgschema.New()
			table := pgschema.Table{Schema: "app", Name: "orders", Owner: "app_owner"}
			grant := pgschema.TablePrivilege{Table: table.ObjectID(), Grantee: "onwardpg_reader", Grantor: "@owner", Privilege: test.privilege, Grantable: test.grantable}
			if err := snapshot.Add(table); err != nil {
				t.Fatal(err)
			}
			if err := snapshot.Add(grant); err != nil {
				t.Fatal(err)
			}
			if err := snapshot.AddDependency(grant.ObjectID(), table.ObjectID()); err != nil {
				t.Fatal(err)
			}

			projected, _, finding, err := projectObserverSnapshot(snapshot, observerContext{
				Role: "onwardpg_reader", DatabaseOwner: "app_owner",
				AccessRoles: map[string]bool{"onwardpg_reader": true},
			})
			if err != nil {
				t.Fatal(err)
			}
			if projected == nil || finding == nil || finding.Code != "observer_access_policy_unsafe" {
				t.Fatalf("projected=%#v finding=%#v", projected, finding)
			}
		})
	}
}
