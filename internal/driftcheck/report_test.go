package driftcheck

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReportJSONExplainsObserverProjection(t *testing.T) {
	report := Report{
		Outcome: "drift_free", Target: "primary", HistoryHead: "sha256:head",
		ExpectedFingerprint: "sha256:expected", ActualFingerprint: "sha256:actual",
		Observer: &Observer{
			Role: "onwardpg_reader", DatabaseOwner: "app_owner", Mode: "dedicated_read_only",
			ProjectedAccess: []string{"acl:schema:app", "table_privilege:app:orders:SELECT:onwardpg_reader"},
		},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`"observer":{"role":"onwardpg_reader"`,
		`"database_owner":"app_owner"`,
		`"mode":"dedicated_read_only"`,
		`"projected_access":["acl:schema:app","table_privilege:app:orders:SELECT:onwardpg_reader"]`,
	} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("report JSON %s does not contain %s", encoded, expected)
		}
	}
}

func TestReportJSONOmitsObserverWhenCatalogWasNotObserved(t *testing.T) {
	encoded, err := json.Marshal(Report{Outcome: "drift_free", Target: "primary", HistoryHead: "sha256:head"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"observer"`) {
		t.Fatalf("report JSON unexpectedly contains observer: %s", encoded)
	}
}
