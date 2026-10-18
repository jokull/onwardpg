package source

import (
	"testing"

	"github.com/jokull/onwardpg/pgschema"
)

// An excluded object masks the unsupported state of itself and of the objects
// in it, and nothing else.
func TestExcludedObjectsMaskTheirUnsupportedState(t *testing.T) {
	constraint := "t_pkey"
	for _, test := range []struct {
		name     string
		excluded []string
		masked   []string
		kept     []string
	}{
		{
			name:     "schema",
			excluded: []string{"schema:legacy"},
			masked: []string{
				"acl:schema:legacy", "ownership:schema:legacy=archive_owner",
				"acl:relation:legacy.archive_seq", "ownership:relation:legacy.archive_seq=archive_owner",
				"acl:type:legacy.state", "ownership:type:legacy.state=archive_owner",
				"acl:routine:legacy.archive(integer, text)", "ownership:routine:legacy.archive(integer)=archive_owner",
				"acl:column:legacy.order_snapshot.body", "acl:table_owner:legacy.order_snapshot",
				"acl:table_non_owner_grantor:legacy.order_snapshot=someone", "clustered_index:legacy.order_snapshot_idx",
				"table_options:legacy.order_snapshot", "rule:legacy.order_snapshot.protect",
				"acl:default:app:legacy:r", "aggregate:legacy.total(integer)", "collation:legacy.c",
				"not_null_constraint:legacy.order_snapshot.body_required", `operator:legacy."+"(integer,integer)`,
				"table_inheritance:legacy.child->public.parent", "extended_statistics:legacy.order_stats",
			},
			kept: []string{
				"acl:schema:legacy_v2", "acl:schema:public", "acl:column:public.legacy.body",
				"acl:default:app:*:r", "acl:default:legacy:public:r", "parameter_acl:legacy", "publication:legacy",
				"ownership:extension:legacy=app", "security_label:legacy:table legacy.order_snapshot",
			},
		},
		{
			name:     "quoted schema",
			excluded: []string{`schema:Legacy "v2"`},
			masked:   []string{`acl:schema:"Legacy ""v2"""`, `acl:column:"Legacy ""v2""".t."Body"`},
			kept:     []string{"acl:schema:legacy", `acl:schema:"Legacy"`},
		},
		{
			name:     "table",
			excluded: []string{"table:public.order_snapshot"},
			masked: []string{
				"acl:column:public.order_snapshot.body", "acl:table_owner:public.order_snapshot",
				"column_storage:public.order_snapshot.body=x", "clustered_index:public.order_snapshot_idx",
				"invalid_index:public.order_snapshot_pkey", "relation_tablespace:public.order_snapshot=fast",
				"not_null_constraint:public.order_snapshot.body_required",
			},
			kept: []string{"acl:column:public.order_snapshot_v2.body", "acl:schema:public", "clustered_index:public.other_idx", "acl:relation:public.order_snapshot"},
		},
		{
			name:     "column",
			excluded: []string{"column:public.order_snapshot.body"},
			masked:   []string{"acl:column:public.order_snapshot.body", "column_compression:public.order_snapshot.body=p"},
			kept:     []string{"acl:column:public.order_snapshot.id", "acl:table_owner:public.order_snapshot", "clustered_index:public.order_snapshot_idx"},
		},
		{
			name:     "index",
			excluded: []string{"index:public.order_snapshot.order_snapshot_idx"},
			masked:   []string{"clustered_index:public.order_snapshot_idx", "relation_tablespace:public.order_snapshot_idx=fast"},
			kept:     []string{"invalid_index:public.order_snapshot_pkey", "acl:table_owner:public.order_snapshot"},
		},
		{
			name:     "constraint",
			excluded: []string{"constraint:public.order_snapshot.t_pkey", "constraint:public.order_snapshot.body_required"},
			masked:   []string{"invalid_index:public.order_snapshot_pkey", "not_null_constraint:public.order_snapshot.body_required"},
			kept:     []string{"clustered_index:public.order_snapshot_idx"},
		},
		{
			name:     "routine and sequence",
			excluded: []string{"routine:public.archive(integer)", "sequence:public.archive_seq"},
			masked:   []string{"acl:routine:public.archive(integer)", "ownership:routine:public.archive(integer)=app", "acl:relation:public.archive_seq", "sequence_persistence:public.archive_seq=u"},
			kept:     []string{"acl:routine:public.archive(text)", "acl:relation:public.other_seq"},
		},
		{
			name:     "extension",
			excluded: []string{"extension:pg_stat_statements"},
			masked:   []string{"ownership:extension:pg_stat_statements=provider_admin"},
			kept:     []string{"ownership:extension:pgcrypto=provider_admin"},
		},
		{name: "nothing excluded", kept: []string{"acl:schema:legacy", "acl:column:public.order_snapshot.body"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker, err := newIgnoreTracker(nil)
			if err != nil {
				t.Fatal(err)
			}
			tracker.recordIndex("public", "order_snapshot", "r", "order_snapshot_idx", nil)
			tracker.recordIndex("public", "order_snapshot", "r", "order_snapshot_pkey", &constraint)
			tracker.recordIndex("legacy", "order_snapshot", "r", "order_snapshot_idx", nil)
			for _, selector := range test.excluded {
				tracker.excluded[selector] = true
			}
			for _, marker := range test.masked {
				if !tracker.masks(marker) {
					t.Errorf("%s was not masked by %v", marker, test.excluded)
				}
			}
			for _, marker := range test.kept {
				if tracker.masks(marker) {
					t.Errorf("%s was masked by %v", marker, test.excluded)
				}
			}
		})
	}
}

// A masked marker leaves no receipt: the receipt of the excluded object is the
// record of the exclusion, so a catalog an existing checkpoint accepted keeps
// its fingerprint. A marker that a selector names exactly keeps its receipt.
func TestMaskedMarkerLeavesNoReceipt(t *testing.T) {
	tracker, err := newIgnoreTracker([]string{"schema:legacy", "acl:schema:public"})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := pgschema.New()
	if skipped, err := tracker.Skip("schema:legacy", snapshot); err != nil || !skipped {
		t.Fatalf("Skip = %v, %v", skipped, err)
	}
	for _, marker := range []string{"acl:schema:legacy", "acl:schema:public", "acl:schema:other"} {
		if err := addBlocker(marker, snapshot, tracker); err != nil {
			t.Fatal(err)
		}
	}
	if got := snapshot.Unsupported(); len(got) != 1 || got[0] != "acl:schema:other" {
		t.Fatalf("unsupported = %v", got)
	}
	if got := snapshot.Ignored(); len(got) != 2 || got[0] != "acl:schema:public" || got[1] != "schema:legacy" {
		t.Fatalf("ignored = %v", got)
	}
}
