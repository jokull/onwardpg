package source

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

func TestValidateLiveIgnoreSelectorsGrammar(t *testing.T) {
	valid := []string{
		"ownership:extension:earthdistance=pscale_admin",
		"ownership:extension:pg_stat_statements=pscale_admin",
		"ownership:schema:pscale_extensions=pscale_admin",
		`ownership:schema:"Provider Schema"=pscale_admin`,
		`ownership:schema:"a=b"="role=1"`,
		`ownership:schema:"say ""hi"""="Admin ""One"""`,
		`ownership:extension:"uuid-ossp"=pscale_admin`,
		"parameter_acl:session_replication_role",
		`parameter_acl:"extwlist.extensions"`,
		`parameter_acl:"a""b"`,
	}
	for _, selector := range valid {
		if err := ValidateLiveIgnoreSelectors([]string{selector}); err != nil {
			t.Errorf("%s rejected: %v", selector, err)
		}
	}
	invalid := []string{
		"",
		"ownership:extension:",
		"ownership:extension:earthdistance",
		"ownership:schema:pscale_extensions",
		"ownership:schema:=provider",
		"ownership:schema:provider_ext=",
		"ownership:extension:name=role=extra",
		"ownership:extension:name==role",
		"ownership:extension:name=role ",
		"ownership:extension:name=role,other",
		"ownership:extension:name =role",
		`ownership:schema:""=role`,
		`ownership:schema:name=""`,
		`ownership:schema:"name=role`,
		`ownership:schema:"name"role`,
		`ownership:schema:"name"x=role`,
		`ownership:schema:"na"me"=role`,
		`ownership:schema:name="role`,
		`ownership:schema:name="ro"le"`,
		"ownership:schema:Provider=role",
		"ownership:schema:name=Role",
		"ownership:schema:1name=role",
		"ownership:schema:na$me=role",
		"ownership:*",
		"ownership:extension:*",
		"ownership:relation:public.sessions=provider",
		"ownership:routine:public.f()=provider",
		"parameter_acl:*",
		"parameter_acl:",
		`parameter_acl:""`,
		`parameter_acl:"unterminated`,
		`parameter_acl:"a"b`,
		"parameter_acl:a.b",
		"parameter_acl:session_replication_role=on",
		"parameter_acl:session_replication_role ",
		" parameter_acl:session_replication_role",
		"extension:earthdistance",
		"table:public.orders",
		"acl:schema:public",
		"event_trigger:audit",
	}
	for _, selector := range invalid {
		err := ValidateLiveIgnoreSelectors([]string{selector})
		if err == nil || !strings.Contains(err.Error(), "live_ignore") {
			t.Errorf("%q accepted or mislabelled: %v", selector, err)
		}
	}
}

// Selectors the catalog queries emit are built with quote_ident; these are the
// forms PostgreSQL produces for identifiers that need quoting.
func TestValidateLiveIgnoreSelectorsAcceptsWhatTheGeneratorEmits(t *testing.T) {
	for _, identifier := range []string{
		"plain", "_leading_underscore", "with_digits_2", `"Mixed"`, `"has space"`, `"with.dot"`, `"with-dash"`,
		`"uuid-ossp"`, `"dollar$"`, `"equals=sign"`, `"quote""inside"`, `"1leading_digit"`, `"user"`, `"ünïcode"`,
	} {
		for _, selector := range []string{
			"ownership:extension:" + identifier + "=" + identifier,
			"ownership:schema:" + identifier + "=plain",
			"ownership:schema:plain=" + identifier,
			"parameter_acl:" + identifier,
		} {
			if err := ValidateLiveIgnoreSelectors([]string{selector}); err != nil {
				t.Errorf("%s rejected: %v", selector, err)
			}
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
	projected, removed, unmatched, err := ProjectLiveIgnored(snapshot, []string{
		"parameter_acl:session_replication_role",
		"ownership:schema:pscale_extensions=pscale_admin",
		"ownership:extension:hypopg=pscale_admin",
		"ownership:extension:earthdistance=pscale_admin", // owner differs in this cluster
		"parameter_acl:not_present_here",
		"parameter_acl:user", // an unquoted keyword never matches what the generator prints
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
	wantUnmatched := []string{"ownership:extension:earthdistance=pscale_admin", "parameter_acl:not_present_here", "parameter_acl:user"}
	if !reflect.DeepEqual(unmatched, wantUnmatched) {
		t.Fatalf("unmatched = %#v, want %#v", unmatched, wantUnmatched)
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
	projected, removed, unmatched, err := ProjectLiveIgnored(snapshot, nil)
	if err != nil || projected != snapshot || removed != nil || unmatched != nil {
		t.Fatalf("projected=%p removed=%#v err=%v", projected, removed, err)
	}
	if _, _, _, err := ProjectLiveIgnored(snapshot, []string{"table:public.orders"}); err == nil {
		t.Fatal("unsupported kind accepted")
	}
}
