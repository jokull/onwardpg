// Package graph defines the typed PostgreSQL schema graph used by onwardpg.
//
// A node is a database object and an edge points from that node to an object it
// depends on. Keeping the direction explicit makes both creation order and
// destructive reverse order mechanical instead of a collection of hard-coded
// planner buckets.
package pgschema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type Kind string

const (
	KindSchema          Kind = "schema"
	KindExtension       Kind = "extension"
	KindEnum            Kind = "enum"
	KindDomain          Kind = "domain"
	KindComposite       Kind = "composite"
	KindRange           Kind = "range"
	KindSequence        Kind = "sequence"
	KindTable           Kind = "table"
	KindColumn          Kind = "column"
	KindConstraint      Kind = "constraint"
	KindIndex           Kind = "index"
	KindView            Kind = "view"
	KindMatView         Kind = "materialized_view"
	KindRoutine         Kind = "routine"
	KindTrigger         Kind = "trigger"
	KindPolicy          Kind = "policy"
	KindReplicaIdentity Kind = "replica_identity"
	KindRowSecurity     Kind = "row_security"
	KindPrivilege       Kind = "table_privilege"
	KindForeignTable    Kind = "foreign_table"
)

// ID identifies an object independently of its Go representation. Part is
// used for child resources (a column, constraint or index) and Signature
// distinguishes overloaded routines.
type ID struct {
	Kind      Kind
	Schema    string
	Name      string
	Part      string
	Signature string
}

func (id ID) String() string {
	parts := []string{string(id.Kind)}
	if id.Schema != "" {
		parts = append(parts, id.Schema)
	}
	if id.Name != "" {
		parts = append(parts, id.Name)
	}
	if id.Part != "" {
		parts = append(parts, id.Part)
	}
	if id.Signature != "" {
		parts = append(parts, "("+id.Signature+")")
	}
	return strings.Join(parts, ":")
}

func (id ID) Valid() bool { return id.Kind != "" && id.Name != "" }

// Object is a strongly typed schema resource. Payload types deliberately do
// not use an unstructured map: a mutation can only be planned after its
// relevant PostgreSQL attributes have a home in the graph.
type Object interface {
	ObjectID() ID
	object()
}

type Schema struct {
	Name    string
	Comment *string
}

func (o Schema) ObjectID() ID { return ID{Kind: KindSchema, Name: o.Name} }
func (Schema) object()        {}

type Extension struct {
	Schema  string
	Name    string
	Version string
	Comment *string
}

func (o Extension) ObjectID() ID { return ID{Kind: KindExtension, Schema: o.Schema, Name: o.Name} }
func (Extension) object()        {}

type Enum struct {
	Schema  string
	Name    string
	Labels  []string
	Comment *string
}

func (o Enum) ObjectID() ID { return ID{Kind: KindEnum, Schema: o.Schema, Name: o.Name} }
func (Enum) object()        {}

type Domain struct {
	Schema      string
	Name        string
	BaseType    string
	Collation   string
	Default     *string
	NotNull     bool
	Constraints []DomainConstraint
	Comment     *string
}

type DomainConstraint struct {
	Name       string
	Definition string
	Validated  bool
}

func (o Domain) ObjectID() ID { return ID{Kind: KindDomain, Schema: o.Schema, Name: o.Name} }
func (Domain) object()        {}

type Composite struct {
	Schema     string
	Name       string
	Attributes []CompositeAttribute
	Comment    *string
}

type CompositeAttribute struct {
	Name      string
	Position  int
	Type      string
	Collation string
}

func (o Composite) ObjectID() ID { return ID{Kind: KindComposite, Schema: o.Schema, Name: o.Name} }
func (Composite) object()        {}

type Range struct {
	Schema         string
	Name           string
	Subtype        string
	Collation      string
	SubtypeOpClass string
	Canonical      string
	SubtypeDiff    string
	MultirangeName string
	Comment        *string
}

func (o Range) ObjectID() ID { return ID{Kind: KindRange, Schema: o.Schema, Name: o.Name} }
func (Range) object()        {}

type Sequence struct {
	Schema    string
	Name      string
	Type      string
	Start     int64
	Increment int64
	Min       int64
	Max       int64
	Cache     int64
	Cycle     bool
	Unlogged  bool
	Comment   *string
	// OwnedBy captures ALTER SEQUENCE ... OWNED BY for standalone
	// sequences. Identity and canonical serial sequences remain attributes of
	// their owning Column and are not duplicated as standalone nodes.
	OwnedBy *ID
}

func (o Sequence) ObjectID() ID { return ID{Kind: KindSequence, Schema: o.Schema, Name: o.Name} }
func (Sequence) object()        {}

type Table struct {
	Schema      string
	Name        string
	Owner       string // Empty means the connection's current role.
	Unlogged    bool
	Comment     *string
	Partition   *Partition
	PartitionOf *PartitionOf
}

func (o Table) ObjectID() ID { return ID{Kind: KindTable, Schema: o.Schema, Name: o.Name} }
func (Table) object()        {}

type Column struct {
	Table    ID
	Name     string
	Position int
	Type     string
	NotNull  bool
	// NotNullConstraintName retains PostgreSQL 18's physical constraint name
	// so a confirmed column rename can preserve the catalog identity PostgreSQL
	// itself leaves behind. The name is operational evidence, not semantic
	// column identity; exceptional named-constraint semantics remain explicit
	// unsupported selectors until they are modeled completely.
	NotNullConstraintName string `json:"-"`
	Default               *string
	Identity              *Identity
	Serial                *Serial
	Generated             *Generated
	Collation             string
	Comment               *string
}

func (o Column) ObjectID() ID {
	return ID{Kind: KindColumn, Schema: o.Table.Schema, Name: o.Table.Name, Part: o.Name}
}
func (Column) object() {}

type Identity struct {
	Generation string // ALWAYS or BY DEFAULT
	Start      int64
	Increment  int64
	Min        *int64
	Max        *int64
	Cache      int64
	Cycle      bool
}

// Serial records PostgreSQL's legacy serial pseudo-types. Catalogs represent
// them as an integer column with an auto-owned sequence and nextval default.
type Serial struct {
	Type         string // smallserial, serial, or bigserial.
	SequenceName string // catalog sequence name; empty is the canonical serial form.
}

type Generated struct {
	Expression string
	Kind       string // PostgreSQL currently supports STORED.
}

type Constraint struct {
	Table ID
	Name  string
	// Parent identifies the partitioned-table constraint from which this
	// constraint was propagated. It is an explicit graph edge, not a naming
	// convention inferred by the planner.
	Parent     *ID
	Type       ConstraintType
	Definition string
	// CheckExpression is PostgreSQL's catalog-deparsed CHECK predicate without
	// the surrounding CHECK (...) presentation. Keeping it separate lets the
	// rollout planner reason about accepted writes without scraping
	// pg_get_constraintdef output. It is empty for non-CHECK constraints.
	CheckExpression string `json:"-"`
	Validated       bool
	NoInherit       bool
	Deferrable      bool
	Deferred        bool
	Reference       *ID
	// Foreign-key semantics are retained as typed catalog state. The ordered
	// columns and equality operators are required to generate exact readiness
	// probes without parsing pg_get_constraintdef presentation SQL.
	ForeignKeyColumns           []string
	ReferencedColumns           []string
	ForeignKeyMatch             ForeignKeyMatch
	ForeignKeyOnUpdate          ForeignKeyAction
	ForeignKeyOnDelete          ForeignKeyAction
	ForeignKeyEqualityOperators []ForeignKeyOperator
	UsingIndex                  string
	Comment                     *string
}

type ForeignKeyMatch string

const (
	ForeignKeyMatchSimple  ForeignKeyMatch = "simple"
	ForeignKeyMatchFull    ForeignKeyMatch = "full"
	ForeignKeyMatchPartial ForeignKeyMatch = "partial"
)

type ForeignKeyAction string

const (
	ForeignKeyNoAction   ForeignKeyAction = "no_action"
	ForeignKeyRestrict   ForeignKeyAction = "restrict"
	ForeignKeyCascade    ForeignKeyAction = "cascade"
	ForeignKeySetNull    ForeignKeyAction = "set_null"
	ForeignKeySetDefault ForeignKeyAction = "set_default"
)

type ForeignKeyOperator struct {
	Schema string
	Name   string
}

type ConstraintType string

const (
	ConstraintPrimary   ConstraintType = "primary_key"
	ConstraintUnique    ConstraintType = "unique"
	ConstraintCheck     ConstraintType = "check"
	ConstraintForeign   ConstraintType = "foreign_key"
	ConstraintExclusion ConstraintType = "exclusion"
)

func (o Constraint) ObjectID() ID {
	return ID{Kind: KindConstraint, Schema: o.Table.Schema, Name: o.Table.Name, Part: o.Name}
}
func (Constraint) object() {}

type Index struct {
	Table ID
	Name  string
	// Parent identifies the partitioned index from which this child index was
	// propagated by PostgreSQL.
	Parent           *ID
	Unique           bool
	Method           string
	Parts            []IndexPart
	Include          []string
	Predicate        string
	Storage          IndexStorage
	NullsNotDistinct bool
	Comment          *string
	Primary          bool
	Exclusion        bool
	Constraint       string
	// Definition is a non-semantic diagnostic compatibility field from
	// pg_get_indexdef. It is intentionally excluded from fingerprints and
	// planning: structured fields above are the source of truth.
	Definition string `json:"-"`
	Concurrent bool
}

func (o Index) ObjectID() ID {
	return ID{Kind: KindIndex, Schema: o.Table.Schema, Name: o.Table.Name, Part: o.Name}
}
func (Index) object() {}

type IndexPart struct {
	Column     string
	Expression string
	// Collation is a catalog-quoted non-default collation for this index key.
	// PostgreSQL stores it independently from the column definition, so it is
	// part of index identity and a change requires index replacement.
	Collation  string
	Descending bool
	NullsFirst bool
	NullsLast  bool
	OpClass    *OpClass
}

type OpClass struct {
	Name       string
	IsDefault  bool
	Parameters []Option
}

type Option struct {
	Name  string
	Value string
}

type IndexStorage struct {
	AutoSummarize *bool
	PagesPerRange *int64
	// Options preserves every reloption reported by PostgreSQL. The named
	// fields above make BRIN's Atlas-visible options convenient to inspect;
	// Options prevents a newer or extension-provided reloption from vanishing
	// from a typed snapshot.
	Options []Option
}

type Partition struct {
	Strategy string // RANGE, LIST or HASH.
	Parts    []PartitionPart
	Raw      string // Canonical pg_get_partkeydef output retained losslessly.
}

// PartitionOf describes a declarative partition child. Bound is PostgreSQL's
// canonical FOR VALUES clause from pg_get_expr(relpartbound, ...).
type PartitionOf struct {
	Parent ID
	Bound  string
}

type PartitionPart struct {
	Column     string
	Expression string
	Collation  string
}

type View struct {
	Schema string
	Name   string
	// Owner is empty for the connected role and otherwise preserves the
	// explicit catalog role across drop/recreate closures.
	Owner        string
	Definition   string
	Materialized bool
	// Populated records the WITH [NO] DATA state of a materialized view.
	// PostgreSQL does not expose this state for ordinary views.
	Populated bool
	// Options is the canonical reloptions set (for example security_barrier
	// and security_invoker). Retaining it avoids treating a view whose access
	// semantics changed as equal merely because its SELECT text is equal.
	Options []Option
	Comment *string
}

func (o View) ObjectID() ID {
	kind := KindView
	if o.Materialized {
		kind = KindMatView
	}
	return ID{Kind: kind, Schema: o.Schema, Name: o.Name}
}
func (View) object() {}

type Routine struct {
	Schema     string
	Name       string
	Signature  string
	Kind       string // function or procedure
	ReturnType string // PostgreSQL's canonical pg_get_function_result output; empty for procedures.
	Definition string
	Comment    *string
}

func (o Routine) ObjectID() ID {
	return ID{Kind: KindRoutine, Schema: o.Schema, Name: o.Name, Signature: o.Signature}
}
func (Routine) object() {}

// Trigger is an executable rule bound to an ordinary/partitioned table or an
// ordinary view. Table retains its historical field name but identifies the
// owning relation kind. Definition is PostgreSQL's own pg_get_triggerdef
// output; Routine is a typed edge to the invoked routine.
// Enabled uses PostgreSQL's tg_enabled codes (O, D, R, A) so a state change is
// not lost merely because its CREATE TRIGGER text is unchanged.
type Trigger struct {
	Table      ID
	Name       string
	Routine    ID
	Definition string
	Enabled    string
	Comment    *string
}

func (o Trigger) ObjectID() ID {
	return ID{Kind: KindTrigger, Schema: o.Table.Schema, Name: o.Table.Name, Part: o.Name}
}
func (Trigger) object() {}

// Policy is a row-level-security policy bound to one ordinary or partitioned
// table. Roles are catalog role names (with the distinguished PUBLIC role
// represented literally as "PUBLIC") and are canonicalized by the catalog
// loader. Expressions are PostgreSQL's deparsed SQL, not caller-built tokens.
type Policy struct {
	Table      ID
	Name       string
	Permissive bool
	Command    string // ALL, SELECT, INSERT, UPDATE, or DELETE.
	Roles      []string
	Using      *string
	Check      *string
}

func (o Policy) ObjectID() ID {
	return ID{Kind: KindPolicy, Schema: o.Table.Schema, Name: o.Table.Name, Part: o.Name}
}
func (Policy) object() {}

// RowSecurity is deliberately a separate node from Table. Its dependency on
// every policy for the table proves that policy creation precedes RLS enable,
// while reverse dependency order proves that RLS disable precedes policy
// removal. Forced may be true independently of Enabled in PostgreSQL catalogs.
type RowSecurity struct {
	Table   ID
	Enabled bool
	Forced  bool
}

func (o RowSecurity) ObjectID() ID {
	return ID{Kind: KindRowSecurity, Schema: o.Table.Schema, Name: o.Table.Name}
}
func (RowSecurity) object() {}

// ReplicaIdentity is separate from Table so a USING INDEX identity can depend
// on the exact index node. That edge guarantees index creation precedes the
// ALTER TABLE and that a reset to DEFAULT precedes removal of the old index.
// A node exists for every ordinary or partitioned table, including DEFAULT,
// so identity transitions are modifications rather than destructive drops.
type ReplicaIdentity struct {
	Table ID
	Mode  ReplicaIdentityMode
	Index *ID
}

type ReplicaIdentityMode string

const (
	ReplicaIdentityDefault ReplicaIdentityMode = "DEFAULT"
	ReplicaIdentityFull    ReplicaIdentityMode = "FULL"
	ReplicaIdentityNothing ReplicaIdentityMode = "NOTHING"
	ReplicaIdentityIndex   ReplicaIdentityMode = "INDEX"
)

func (o ReplicaIdentity) ObjectID() ID {
	return ID{Kind: KindReplicaIdentity, Schema: o.Table.Schema, Name: o.Table.Name}
}
func (ReplicaIdentity) object() {}

// TablePrivilege models one grantee/privilege pair on a table, view, or
// materialized view. The owning role's implicit rights are validated by the
// catalog loader rather than duplicated as nodes. Grantor is retained so a
// non-owner grant chain can never vanish from a snapshot; recreation uses an
// explicit SET LOCAL ROLE boundary for a non-owner grantor.
type TablePrivilege struct {
	Table     ID
	Grantee   string
	Grantor   string
	Privilege string
	Grantable bool
}

func (o TablePrivilege) ObjectID() ID {
	return ID{Kind: KindPrivilege, Schema: o.Table.Schema, Name: o.Table.Name, Part: o.Privilege + ":" + o.Grantee}
}
func (TablePrivilege) object() {}

// Snapshot stores a complete catalog view. Objects and dependencies are kept
// private so every write validates IDs and references at the boundary.
type Snapshot struct {
	objects     map[ID]Object
	deps        map[ID]map[ID]struct{}
	unsupported map[string]struct{}
	ignored     map[string]struct{}
}

func New() *Snapshot {
	return &Snapshot{
		objects: make(map[ID]Object), deps: make(map[ID]map[ID]struct{}),
		unsupported: make(map[string]struct{}), ignored: make(map[string]struct{}),
	}
}

func (s *Snapshot) Add(object Object) error {
	if column, ok := object.(Column); ok {
		column.Default = NormalizeDefault(column.Default)
		object = column
	}
	if policy, ok := object.(Policy); ok {
		policy.Roles = append([]string(nil), policy.Roles...)
		sort.Strings(policy.Roles)
		object = policy
	}
	id := object.ObjectID()
	if !id.Valid() {
		return fmt.Errorf("invalid object id %q", id.String())
	}
	if _, exists := s.objects[id]; exists {
		return fmt.Errorf("duplicate object %s", id)
	}
	s.objects[id] = object
	s.deps[id] = make(map[ID]struct{})
	return nil
}

// NormalizeDefault provides a deliberately narrow semantic normalization for
// PostgreSQL defaults that are provably equivalent without evaluating them.
// More complex expressions remain lossless catalog SQL and therefore cannot be
// silently treated as equal.
func NormalizeDefault(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	for hasOuterParens(normalized) {
		normalized = strings.TrimSpace(normalized[1 : len(normalized)-1])
	}
	switch strings.ToLower(normalized) {
	case "current_timestamp", "transaction_timestamp()", "now()":
		normalized = "now()"
	}
	return &normalized
}

func hasOuterParens(value string) bool {
	if len(value) < 2 || value[0] != '(' || value[len(value)-1] != ')' {
		return false
	}
	depth := 0
	for i, r := range value {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(value)-1 {
				return false
			}
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

func (s *Snapshot) AddDependency(object, dependency ID) error {
	if _, exists := s.objects[object]; !exists {
		return fmt.Errorf("dependency source %s is not in snapshot", object)
	}
	if _, exists := s.objects[dependency]; !exists {
		return fmt.Errorf("dependency target %s is not in snapshot", dependency)
	}
	s.deps[object][dependency] = struct{}{}
	return nil
}

func (s *Snapshot) Object(id ID) (Object, bool) {
	object, ok := s.objects[id]
	return object, ok
}

func (s *Snapshot) Objects() []Object {
	ids := s.IDs()
	objects := make([]Object, 0, len(ids))
	for _, id := range ids {
		objects = append(objects, s.objects[id])
	}
	return objects
}

func (s *Snapshot) IDs() []ID {
	ids := make([]ID, 0, len(s.objects))
	for id := range s.objects {
		ids = append(ids, id)
	}
	sortIDs(ids)
	return ids
}

// ValidateObjectOrder verifies that order is an exact permutation of the
// snapshot's object IDs. It is the boundary check for programmatically built
// unsorted schema-dump orders: partial, duplicate, stale, or invented IDs
// would otherwise silently fall back to a planner-selected order.
func ValidateObjectOrder(snapshot *Snapshot, order []ID) error {
	if snapshot == nil {
		return fmt.Errorf("object order requires a snapshot")
	}
	ids := snapshot.IDs()
	if len(order) != len(ids) {
		return fmt.Errorf("object order has %d IDs, want %d", len(order), len(ids))
	}
	known := make(map[ID]struct{}, len(ids))
	for _, id := range ids {
		known[id] = struct{}{}
	}
	seen := make(map[ID]struct{}, len(order))
	for _, id := range order {
		if !id.Valid() {
			return fmt.Errorf("object order contains invalid ID %q", id.String())
		}
		if _, exists := known[id]; !exists {
			return fmt.Errorf("object order contains unknown ID %s", id)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("object order contains duplicate ID %s", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (s *Snapshot) Dependencies(id ID) []ID {
	deps := make([]ID, 0, len(s.deps[id]))
	for dependency := range s.deps[id] {
		deps = append(deps, dependency)
	}
	sortIDs(deps)
	return deps
}

func (s *Snapshot) AddUnsupported(selector string) error {
	if selector == "" {
		return fmt.Errorf("unsupported selector is empty")
	}
	s.unsupported[selector] = struct{}{}
	return nil
}

func (s *Snapshot) AddIgnored(selector string) error {
	if selector == "" {
		return fmt.Errorf("ignored selector is empty")
	}
	s.ignored[selector] = struct{}{}
	return nil
}

func (s *Snapshot) Unsupported() []string { return sortedStrings(s.unsupported) }
func (s *Snapshot) Ignored() []string     { return sortedStrings(s.ignored) }

type canonicalNode struct {
	ID           ID     `json:"id"`
	Object       Object `json:"object"`
	Dependencies []ID   `json:"dependencies,omitempty"`
}

type canonicalSnapshot struct {
	Nodes       []canonicalNode `json:"nodes"`
	Unsupported []string        `json:"unsupported,omitempty"`
	Ignored     []string        `json:"ignored,omitempty"`
}

// CanonicalJSON is the stable representation used for fingerprints, source
// equivalence, persisted answers, and convergence checks. Node insertion and
// dependency insertion order do not affect the output.
func (s *Snapshot) CanonicalJSON() ([]byte, error) {
	nodes := make([]canonicalNode, 0, len(s.objects))
	for _, id := range s.IDs() {
		object := s.objects[id]
		if column, ok := object.(Column); ok {
			// Physical attnum remains available on the typed snapshot for
			// declaration order and diagnostics. It is not semantic schema
			// identity: ALTER TABLE ADD COLUMN cannot reproduce a declarative
			// source file's visual insertion point.
			column.Position = 0
			object = column
		}
		nodes = append(nodes, canonicalNode{ID: id, Object: object, Dependencies: s.Dependencies(id)})
	}
	return json.Marshal(canonicalSnapshot{Nodes: nodes, Unsupported: s.Unsupported(), Ignored: s.Ignored()})
}

func (s *Snapshot) Fingerprint() (string, error) {
	canonical, err := s.CanonicalJSON()
	if err != nil {
		return "", fmt.Errorf("encode canonical schema graph: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Batch is a dependency-safe group. Components with more than one object (or
// a self-edge) are cycles: a planner must explicitly detach a cycle-safe edge,
// such as a foreign key, rather than relying on map iteration order.
type Batch struct {
	Objects []Object
	Cyclic  bool
}

// Batches returns strongly connected components in dependency order. It is
// deterministic and does not silently flatten a cycle.
func (s *Snapshot) Batches() []Batch {
	components := s.components()
	componentOf := make(map[ID]int, len(s.objects))
	for i, component := range components {
		for _, id := range component {
			componentOf[id] = i
		}
	}

	dependents := make([]map[int]struct{}, len(components))
	remaining := make([]int, len(components))
	for i := range components {
		dependents[i] = make(map[int]struct{})
	}
	for id, dependencies := range s.deps {
		from := componentOf[id]
		for dependency := range dependencies {
			to := componentOf[dependency]
			if from == to {
				continue
			}
			if _, exists := dependents[to][from]; !exists {
				dependents[to][from] = struct{}{}
				remaining[from]++
			}
		}
	}

	ready := make([]int, 0, len(components))
	for i, count := range remaining {
		if count == 0 {
			ready = append(ready, i)
		}
	}
	var batches []Batch
	for len(ready) > 0 {
		sort.Slice(ready, func(i, j int) bool {
			return s.lessComponent(components[ready[i]], components[ready[j]])
		})
		componentIndex := ready[0]
		ready = ready[1:]
		component := components[componentIndex]
		objects := make([]Object, 0, len(component))
		for _, id := range component {
			objects = append(objects, s.objects[id])
		}
		batches = append(batches, Batch{Objects: objects, Cyclic: s.cyclic(component)})
		for dependent := range dependents[componentIndex] {
			remaining[dependent]--
			if remaining[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}
	return batches
}

// lessComponent preserves PostgreSQL column declaration order when otherwise
// independent columns of the same relation become ready together. Their
// graph edges correctly say that either may be created after the table, but
// ADD COLUMN order determines the physical attnum that catalog inspection
// observes and that onwardpg receipts. All other ties retain canonical object
// identity ordering.
func (s *Snapshot) lessComponent(left, right []ID) bool {
	if len(left) == 1 && len(right) == 1 && left[0].Kind == KindColumn && right[0].Kind == KindColumn &&
		left[0].Schema == right[0].Schema && left[0].Name == right[0].Name {
		leftColumn, leftOK := s.objects[left[0]].(Column)
		rightColumn, rightOK := s.objects[right[0]].(Column)
		if leftOK && rightOK && leftColumn.Position != rightColumn.Position {
			return leftColumn.Position < rightColumn.Position
		}
	}
	return lessID(left[0], right[0])
}

func (s *Snapshot) cyclic(component []ID) bool {
	if len(component) > 1 {
		return true
	}
	_, selfEdge := s.deps[component[0]][component[0]]
	return selfEdge
}

func (s *Snapshot) components() [][]ID {
	var (
		index      int
		stack      []ID
		onStack    = make(map[ID]bool, len(s.objects))
		indices    = make(map[ID]int, len(s.objects))
		lowlink    = make(map[ID]int, len(s.objects))
		components [][]ID
	)
	for _, id := range s.IDs() {
		indices[id] = -1
	}
	var visit func(ID)
	visit = func(id ID) {
		indices[id], lowlink[id] = index, index
		index++
		stack, onStack[id] = append(stack, id), true
		for _, dependency := range s.Dependencies(id) {
			if indices[dependency] == -1 {
				visit(dependency)
				lowlink[id] = min(lowlink[id], lowlink[dependency])
			} else if onStack[dependency] {
				lowlink[id] = min(lowlink[id], indices[dependency])
			}
		}
		if lowlink[id] != indices[id] {
			return
		}
		var component []ID
		for {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[last] = false
			component = append(component, last)
			if last == id {
				break
			}
		}
		sortIDs(component)
		components = append(components, component)
	}
	for _, id := range s.IDs() {
		if indices[id] == -1 {
			visit(id)
		}
	}
	return components
}

func sortIDs(ids []ID) {
	sort.Slice(ids, func(i, j int) bool { return lessID(ids[i], ids[j]) })
}

// lessID is the allocation-free canonical order for typed object identities.
// It matches the field order represented by ID.String without formatting an
// ID for every comparison in large graph sorts.
func lessID(left, right ID) bool {
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if left.Schema != right.Schema {
		return left.Schema < right.Schema
	}
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	if left.Part != right.Part {
		return left.Part < right.Part
	}
	return left.Signature < right.Signature
}

func sortedStrings(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
