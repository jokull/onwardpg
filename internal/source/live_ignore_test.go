package source

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

func TestValidateLiveIgnoreSelectorsAcceptsOnlyExactProviderState(t *testing.T) {
	for _, selector := range []string{
		"ownership:extension:earthdistance=pscale_admin",
		`ownership:schema:"Provider Schema"=pscale_admin`,
		"parameter_acl:session_replication_role",
		`parameter_acl:"extwlist.extensions"`,
	} {
		if err := ValidateLiveIgnoreSelectors([]string{selector}); err != nil {
			t.Errorf("%s rejected: %v", selector, err)
		}
	}
	for _, selector := range []string{
		"",
		"ownership:extension:",
		"ownership:extension:earthdistance",
		"ownership:schema:pscale_extensions",
		"ownership:*",
		"ownership:extension:*",
		"ownership:relation:public.sessions=provider",
		"ownership:routine:public.f()=provider",
		"parameter_acl:*",
		"parameter_acl:",
		" parameter_acl:session_replication_role",
		"extension:earthdistance",
		"table:public.orders",
		"acl:schema:public",
		"event_trigger:audit",
	} {
		err := ValidateLiveIgnoreSelectors([]string{selector})
		if err == nil || !strings.Contains(err.Error(), "live_ignore") {
			t.Errorf("%q accepted or mislabelled: %v", selector, err)
		}
	}
}

func TestProjectLiveIgnoredRemovesOnlyAcknowledgedMarkers(t *testing.T) {
	snapshot := pgschema.New()
	for _, object := range []pgschema.Object{pgschema.Schema{Name: "pscale_extensions"}, pgschema.Table{Schema: "public", Name: "orders"}} {
		if err := snapshot.Add(object); err != nil {
			t.Fatal(err)
		}
	}
	for _, selector := range []string{
		"ownership:schema:pscale_extensions=pscale_admin",
		"ownership:extension:hypopg=pscale_admin",
		"ownership:extension:earthdistance=someone_else",
		"parameter_acl:session_replication_role",
		"ownership:relation:public.sessions=pscale_admin",
	} {
		if err := snapshot.AddUnsupported(selector); err != nil {
			t.Fatal(err)
		}
	}
	if err := snapshot.AddIgnored("table:public.django_migrations"); err != nil {
		t.Fatal(err)
	}
	before, err := snapshot.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	projected, removed, err := ProjectLiveIgnored(snapshot, []string{
		"parameter_acl:session_replication_role",
		"ownership:schema:pscale_extensions=pscale_admin",
		"ownership:extension:hypopg=pscale_admin",
		"ownership:extension:earthdistance=pscale_admin", // owner differs in this cluster
		"parameter_acl:not_present_here",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantRemoved := []string{
		"ownership:extension:hypopg=pscale_admin",
		"ownership:schema:pscale_extensions=pscale_admin",
		"parameter_acl:session_replication_role",
	}
	if !reflect.DeepEqual(removed, wantRemoved) {
		t.Fatalf("removed = %#v, want %#v", removed, wantRemoved)
	}
	wantUnsupported := []string{"ownership:extension:earthdistance=someone_else", "ownership:relation:public.sessions=pscale_admin"}
	if !reflect.DeepEqual(projected.Unsupported(), wantUnsupported) {
		t.Fatalf("unsupported = %#v, want %#v", projected.Unsupported(), wantUnsupported)
	}
	if len(projected.IDs()) != 2 || !reflect.DeepEqual(projected.Ignored(), snapshot.Ignored()) {
		t.Fatalf("objects or ignore receipts changed: %#v %#v", projected.IDs(), projected.Ignored())
	}
	after, err := snapshot.Fingerprint()
	if err != nil || after != before {
		t.Fatalf("input snapshot was modified: %v", err)
	}
}

func TestProjectLiveIgnoredWithoutSelectorsKeepsTheSnapshot(t *testing.T) {
	snapshot := pgschema.New()
	if err := snapshot.AddUnsupported("parameter_acl:session_replication_role"); err != nil {
		t.Fatal(err)
	}
	projected, removed, err := ProjectLiveIgnored(snapshot, nil)
	if err != nil || projected != snapshot || removed != nil {
		t.Fatalf("projected=%p removed=%#v err=%v", projected, removed, err)
	}
	if _, _, err := ProjectLiveIgnored(snapshot, []string{"table:public.orders"}); err == nil {
		t.Fatal("unsupported kind accepted")
	}
}
