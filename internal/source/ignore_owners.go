package source

import "strings"

// Unsupported-state markers name the object whose state they describe, with
// identifiers as quote_ident writes them (raw names in the few families that
// graphOutsideCoreQuery and the inspectors build). An ignore selector that
// excludes an object also masks the markers of that object and of the objects
// in it: schema:NAME masks acl:schema:NAME, ownership:schema:NAME=ROLE, and the
// ACL, ownership, and storage markers of each table, column, type, routine,
// and index in the schema; table:NAME masks the markers of the table and its
// columns and indexes. A marker of state outside the excluded objects, and a
// marker of cluster or database state (a publication, a cast, a server
// parameter, a default ACL for every schema), still blocks.
//
// markerShape is how the name in a family of markers is built.
type markerShape int

const (
	// shapeSchema: SCHEMA, then text that is not a name.
	shapeSchema markerShape = iota
	// shapeRelation: SCHEMA.RELATION.
	shapeRelation
	// shapeMember: SCHEMA.RELATION.MEMBER.
	shapeMember
	// shapeRoutine: SCHEMA.NAME(ARGUMENTS), optionally followed by =ROLE.
	shapeRoutine
	// shapeRaw: unquoted SCHEMA.REST, as the inspectors build it.
	shapeRaw
)

// markerFamily is one family of unsupported-state markers. Owners lists the
// selector kinds that the name can be an object of; the schema of the name is
// always an owner, so an ignored schema masks every family listed here.
type markerFamily struct {
	prefix string
	shape  markerShape
	owners []string
	// index is set when the relation of the name can be an index; the
	// index is resolved to its table through the inspected indexes.
	index bool
}

var relationKinds = []string{"table", "view", "materialized_view"}
var typeKinds = []string{"enum", "domain", "composite", "range_type"}

// markerFamilies lists every unsupported-state family that belongs to a
// schema object. A family not listed here is never masked by an object
// selector; it can still be ignored by its exact selector.
var markerFamilies = []markerFamily{
	{prefix: "ownership:schema:", shape: shapeSchema},
	{prefix: "acl:schema:", shape: shapeSchema},
	{prefix: "ownership:relation:", shape: shapeRelation, owners: []string{"sequence"}},
	{prefix: "acl:relation:", shape: shapeRelation, owners: []string{"sequence"}},
	{prefix: "sequence_persistence:", shape: shapeRelation, owners: []string{"sequence"}},
	{prefix: "ownership:type:", shape: shapeRelation, owners: typeKinds},
	{prefix: "acl:type:", shape: shapeRelation, owners: typeKinds},
	{prefix: "ownership:routine:", shape: shapeRoutine, owners: []string{"routine"}},
	{prefix: "acl:routine:", shape: shapeRoutine, owners: []string{"routine"}},
	{prefix: "acl:table_owner:", shape: shapeRelation, owners: []string{"table"}},
	{prefix: "acl:table_non_owner_grantor:", shape: shapeRelation, owners: relationKinds},
	{prefix: "typed_table:", shape: shapeRelation, owners: []string{"table"}},
	{prefix: "table_access_method:", shape: shapeRelation, owners: relationKinds},
	{prefix: "table_inheritance:", shape: shapeRelation, owners: []string{"table"}},
	{prefix: "table_options:", shape: shapeRelation, owners: relationKinds, index: true},
	{prefix: "toast_options:", shape: shapeRelation, owners: relationKinds},
	{prefix: "toast_tablespace:", shape: shapeRelation, owners: relationKinds},
	{prefix: "relation_tablespace:", shape: shapeRelation, owners: relationKinds, index: true},
	{prefix: "clustered_index:", shape: shapeRelation, index: true},
	{prefix: "invalid_index:", shape: shapeRelation, index: true},
	{prefix: "acl:column:", shape: shapeMember, owners: []string{"column"}},
	{prefix: "serial_sequence_state:", shape: shapeMember, owners: []string{"column"}},
	{prefix: "identity_sequence_metadata:", shape: shapeMember, owners: []string{"column"}},
	{prefix: "column_storage:", shape: shapeMember, owners: []string{"column"}},
	{prefix: "column_compression:", shape: shapeMember, owners: []string{"column"}},
	{prefix: "column_statistics:", shape: shapeMember, owners: []string{"column"}},
	{prefix: "column_options:", shape: shapeMember, owners: []string{"column"}},
	{prefix: "comment:view_column:", shape: shapeMember},
	{prefix: "view_column_default:", shape: shapeMember},
	{prefix: "rule:", shape: shapeMember},
	{prefix: "comment:policy:", shape: shapeMember, owners: []string{"policy"}},
	{prefix: "not_null_constraint:", shape: shapeMember, owners: []string{"constraint"}},
	{prefix: "comment:domain_constraint:", shape: shapeRelation, owners: []string{"domain"}},
	{prefix: "comment:composite_attribute:", shape: shapeRelation, owners: []string{"composite"}},
	{prefix: "extended_statistics:", shape: shapeSchema},
	{prefix: "text_search_configuration:", shape: shapeSchema},
	{prefix: "text_search_dictionary:", shape: shapeSchema},
	{prefix: "text_search_parser:", shape: shapeSchema},
	{prefix: "text_search_template:", shape: shapeSchema},
	{prefix: "operator:", shape: shapeSchema},
	{prefix: "operator_class:", shape: shapeSchema},
	{prefix: "operator_family:", shape: shapeSchema},
	{prefix: "conversion:", shape: shapeSchema},
	{prefix: "aggregate:", shape: shapeRaw},
	{prefix: "collation:", shape: shapeRaw},
	{prefix: "foreign_table:", shape: shapeRaw},
	{prefix: "partition_detach_pending:", shape: shapeRaw},
}

// markerOwners returns the object selectors, with raw names as the
// inspectors record them, that the marker describes state of: the most
// specific object first and the schema last. indexRelation resolves an index
// in a schema to the selectors of the index and its table. It returns nil for
// a marker of no schema object.
func markerOwners(marker string, indexRelation func(schema, index string) []string) []string {
	if owners := extensionMarkerOwners(marker); owners != nil {
		return owners
	}
	if owners := defaultACLMarkerOwners(marker); owners != nil {
		return owners
	}
	for _, family := range markerFamilies {
		rest, found := strings.CutPrefix(marker, family.prefix)
		if !found {
			continue
		}
		if family.shape == shapeRaw {
			schema, _, found := strings.Cut(rest, ".")
			if !found || schema == "" {
				return nil
			}
			return []string{"schema:" + schema}
		}
		count := map[markerShape]int{shapeSchema: 1, shapeRelation: 2, shapeMember: 3, shapeRoutine: 2}[family.shape]
		names, rest, ok := cutIdentifierPath(rest, count)
		if !ok {
			return nil
		}
		var owners []string
		switch family.shape {
		case shapeRelation, shapeMember:
			relation := names[0] + "." + names[1]
			if len(names) == 3 {
				for _, kind := range family.owners {
					owners = append(owners, kind+":"+relation+"."+names[2])
				}
				for _, kind := range relationKinds {
					owners = append(owners, kind+":"+relation)
				}
			} else {
				for _, kind := range family.owners {
					owners = append(owners, kind+":"+relation)
				}
			}
			if family.index && indexRelation != nil {
				owners = append(owners, indexRelation(names[0], names[1])...)
			}
		case shapeRoutine:
			arguments, ok := routineArguments(rest, family.prefix == "ownership:routine:")
			if !ok {
				return nil
			}
			owners = append(owners, "routine:"+names[0]+"."+names[1]+"("+arguments+")")
		}
		return append(owners, "schema:"+names[0])
	}
	return nil
}

// extensionMarkerOwners handles ownership:extension:NAME=ROLE, which belongs
// to the extension.
func extensionMarkerOwners(marker string) []string {
	rest, found := strings.CutPrefix(marker, "ownership:extension:")
	if !found {
		return nil
	}
	names, _, ok := cutIdentifierPath(rest, 1)
	if !ok {
		return nil
	}
	return []string{"extension:" + names[0]}
}

// defaultACLMarkerOwners handles acl:default:ROLE:SCHEMA:TYPE. Default
// privileges set IN SCHEMA belong to that schema; those for every schema (*)
// belong to none.
func defaultACLMarkerOwners(marker string) []string {
	rest, found := strings.CutPrefix(marker, "acl:default:")
	if !found {
		return nil
	}
	_, rest, err := cutQuotedIdentifier(rest)
	if err != nil {
		return nil
	}
	rest, found = strings.CutPrefix(rest, ":")
	if !found || strings.HasPrefix(rest, "*") {
		return nil
	}
	names, _, ok := cutIdentifierPath(rest, 1)
	if !ok {
		return nil
	}
	return []string{"schema:" + names[0]}
}

// cutIdentifierPath reads count dot-separated identifiers, as quote_ident
// writes them, off the front of text and returns them unquoted.
func cutIdentifierPath(text string, count int) ([]string, string, bool) {
	names := make([]string, 0, count)
	for index := 0; index < count; index++ {
		if index > 0 {
			var found bool
			if text, found = strings.CutPrefix(text, "."); !found {
				return nil, "", false
			}
		}
		identifier, rest, err := cutQuotedIdentifier(text)
		if err != nil {
			return nil, "", false
		}
		names = append(names, unquoteIdentifier(identifier))
		text = rest
	}
	return names, text, true
}

func unquoteIdentifier(identifier string) string {
	if len(identifier) < 2 || identifier[0] != '"' {
		return identifier
	}
	return strings.ReplaceAll(identifier[1:len(identifier)-1], `""`, `"`)
}

// routineArguments returns ARGUMENTS from "(ARGUMENTS)" or, for an ownership
// marker, from "(ARGUMENTS)=ROLE".
func routineArguments(text string, owner bool) (string, bool) {
	if !strings.HasPrefix(text, "(") {
		return "", false
	}
	end := len(text) - 1
	if owner {
		end = strings.LastIndex(text, ")=")
	}
	if end < 1 || text[end] != ')' {
		return "", false
	}
	return text[1:end], true
}
