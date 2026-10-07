package graphplan

import (
	"reflect"
	"strings"

	"github.com/jokull/onwardpg/internal/change"
	"github.com/jokull/onwardpg/internal/protocol"
	"github.com/jokull/onwardpg/pgschema"
)

// HazardSlowerQueries marks the removal of an index that enforces nothing:
// queries that used the index can get slower. No row and no guarantee is lost.
const HazardSlowerQueries = "slower_queries_possible"

// HazardPartitionedIndexNotConcurrent marks an index statement on a
// partitioned table that the concurrent index mode could not make concurrent.
// PostgreSQL has no concurrent form for it.
const HazardPartitionedIndexNotConcurrent = "partitioned_index_not_concurrent"

// indexOnPartitionedTable reports whether the index belongs to a partitioned
// table. PostgreSQL rejects CREATE INDEX CONCURRENTLY and DROP INDEX
// CONCURRENTLY for such an index.
func indexOnPartitionedTable(snapshot *pgschema.Snapshot, index pgschema.Index) bool {
	if snapshot == nil {
		return false
	}
	object, exists := snapshot.Object(index.Table)
	table, ok := object.(pgschema.Table)
	return exists && ok && table.Partition != nil
}

func withHazards(statements []protocol.Statement, hazards ...string) []protocol.Statement {
	result := make([]protocol.Statement, len(statements))
	for index, item := range statements {
		item.Hazards = append(append([]string(nil), item.Hazards...), hazards...)
		result[index] = item
	}
	return result
}

// renderOnlinePartitionedIndexCreate builds a new non-unique index on an
// existing partitioned table without a lock that blocks writes for the time
// of the build: an invalid ON ONLY index on each partitioned table, a
// concurrent build on each leaf partition, and an attach of each child. The
// parent index becomes valid with the last attach. The desired snapshot names
// every child index, so the result has the same catalog as a plain CREATE
// INDEX on the parent.
//
// It returns false for every case that it does not prove: a unique index
// (its enforcement follows the contract rules of one statement), a partition
// hierarchy that the same plan changes, and a child index that already
// exists. The caller then emits the plain statement.
func renderOnlinePartitionedIndexCreate(index pgschema.Index, current, desired *pgschema.Snapshot) ([]protocol.Statement, bool) {
	if index.Parent != nil || index.Unique || index.Primary || index.Exclusion || index.Constraint != "" ||
		!indexOnPartitionedTable(desired, index) || !sameTable(current, desired, index.Table) {
		return nil, false
	}
	shell, unsupported := renderPartitionedIndexShell(index)
	if unsupported != "" {
		return nil, false
	}
	children, ok := onlinePartitionedIndexChildren(index, current, desired)
	// A table with no partition has nothing to build: the plain statement is
	// immediate and leaves a valid index.
	if !ok || len(children) == 0 {
		return nil, false
	}
	const label = "online_partitioned_index_build"
	statements := []protocol.Statement{
		withTimeoutGuidance(statement(strings.TrimSuffix(shell, ";")+";", protocol.PhaseExpand, "review", true, label, "partitioned_index_shell", "brief_lock"), 30000, 3000),
	}
	if index.Comment != nil {
		statements = append(statements, statement("COMMENT ON INDEX "+qualified(index.Table.Schema, index.Name)+" IS "+literal(*index.Comment)+";", protocol.PhaseExpand, "safe", true))
	}
	for _, child := range children {
		built, unsupported := renderPartitionIndexReplacement(child, index)
		if unsupported != "" {
			return nil, false
		}
		statements = append(statements, built...)
	}
	for position := range statements {
		for hazard := range statements[position].Hazards {
			if statements[position].Hazards[hazard] == "continuous_partitioned_index_replacement" {
				statements[position].Hazards[hazard] = label
			}
		}
	}
	return statements, true
}

// sameTable reports whether the plan leaves the table as it is: the same
// partition key and the same place in its hierarchy.
func sameTable(current, desired *pgschema.Snapshot, id pgschema.ID) bool {
	if current == nil || desired == nil {
		return false
	}
	before, existed := current.Object(id)
	after, exists := desired.Object(id)
	previous, wasTable := before.(pgschema.Table)
	next, isTable := after.(pgschema.Table)
	return existed && exists && wasTable && isTable &&
		reflect.DeepEqual(previous.Partition, next.Partition) && reflect.DeepEqual(previous.PartitionOf, next.PartitionOf)
}

// partitionsOf lists the tables that are attached to the partitioned table.
func partitionsOf(snapshot *pgschema.Snapshot, parent pgschema.ID) []pgschema.ID {
	var result []pgschema.ID
	for _, object := range snapshot.Objects() {
		if table, ok := object.(pgschema.Table); ok && table.PartitionOf != nil && table.PartitionOf.Parent == parent {
			result = append(result, table.ObjectID())
		}
	}
	return result
}

func onlinePartitionedIndexChildren(parent pgschema.Index, current, desired *pgschema.Snapshot) ([]partitionIndexReplacement, bool) {
	// The parent index becomes valid only when every attached partition has
	// its child index. A partition that the plan adds or removes is attached
	// for a part of the plan only, so the same set must exist on both sides,
	// and the desired snapshot must name one child index for each partition.
	children := partitionIndexChildren(desired, parent.ObjectID())
	if partitions := partitionsOf(desired, parent.Table); !reflect.DeepEqual(partitionsOf(current, parent.Table), partitions) || len(partitions) != len(children) {
		return nil, false
	}
	var result []partitionIndexReplacement
	for _, child := range children {
		object, exists := desired.Object(child.Table)
		table, ok := object.(pgschema.Table)
		if !exists || !ok || child.Constraint != "" || child.Primary || child.Exclusion || !sameTable(current, desired, child.Table) {
			return nil, false
		}
		// A name that another relation has now would need the drop of that
		// relation first; the plain statement has that ordering.
		if _, present := current.Object(child.ObjectID()); present || relationNameExists(current, child.Table.Schema, child.Name) {
			return nil, false
		}
		node := partitionIndexReplacement{after: child, partitioned: table.Partition != nil}
		nested, ok := onlinePartitionedIndexChildren(child, current, desired)
		if !ok || !node.partitioned && len(nested) != 0 {
			return nil, false
		}
		node.children = nested
		result = append(result, node)
	}
	return result, true
}

// dropNeedsNoDecision reports whether a drop loses neither rows nor a
// guarantee. Only one kind of object qualifies: a standalone index that
// enforces nothing. Its removal is already explicit in the desired schema.
// Every unique index and every constraint keeps its decision: uniqueness has
// too many other uses (foreign-key targets, ON CONFLICT, replica identity) to
// prove that a second index makes one redundant.
func dropNeedsNoDecision(item change.Change, current *pgschema.Snapshot) bool {
	index, ok := item.Before.(pgschema.Index)
	if item.Kind != change.Drop || !ok {
		return false
	}
	if index.Parent != nil || index.Unique || index.Primary || index.Exclusion || index.Constraint != "" {
		return false
	}
	// An index that the table is clustered on, or that is its replica
	// identity, is more than a performance index.
	return !isReplicaIdentityIndex(current, index) && !isClusteredIndex(current, index)
}

// isReplicaIdentityIndex reports whether logical replication identifies the
// rows of the table by this index.
func isReplicaIdentityIndex(snapshot *pgschema.Snapshot, index pgschema.Index) bool {
	if snapshot == nil {
		return false
	}
	object, exists := snapshot.Object(pgschema.ReplicaIdentity{Table: index.Table}.ObjectID())
	identity, ok := object.(pgschema.ReplicaIdentity)
	return exists && ok && identity.Index != nil && *identity.Index == index.ObjectID()
}

// isClusteredIndex reports whether the catalog reader marked the index as the
// one its table is clustered on. The planner does not model clustering: the
// reader reports it as the selector clustered_index:SCHEMA.INDEX, with
// PostgreSQL identifier quotes where a name needs them.
func isClusteredIndex(snapshot *pgschema.Snapshot, index pgschema.Index) bool {
	if snapshot == nil {
		return false
	}
	for _, selector := range append(snapshot.Unsupported(), snapshot.Ignored()...) {
		name, found := strings.CutPrefix(selector, "clustered_index:")
		if !found {
			continue
		}
		for _, schema := range []string{index.Table.Schema, quote(index.Table.Schema)} {
			if name == schema+"."+index.Name || name == schema+"."+quote(index.Name) {
				return true
			}
		}
	}
	return false
}

// DropNeedsNoDecision reports whether the plan removes the named current
// object without a confirmation question. A caller uses it to accept a drop
// hint that an earlier planner version asked for.
func DropNeedsNoDecision(id pgschema.ID, current, desired *pgschema.Snapshot) bool {
	if current == nil || desired == nil {
		return false
	}
	before, exists := current.Object(id)
	if !exists {
		return false
	}
	if _, stays := desired.Object(id); stays {
		return false
	}
	return dropNeedsNoDecision(change.Change{Kind: change.Drop, ID: id, Before: before}, current)
}

// DropDecisionHazards names what the confirmation of a drop gives up. Only a
// drop of an object that owns rows is data loss. current is the snapshot that
// holds the object; it tells whether an index has a second role.
func DropDecisionHazards(current *pgschema.Snapshot, object pgschema.Object) []string {
	switch object := object.(type) {
	case pgschema.Index:
		hazards := []string{HazardSlowerQueries}
		if object.Unique {
			hazards = []string{"unique_index_enforcement_removed", "duplicate_rows_possible"}
		}
		if isReplicaIdentityIndex(current, object) {
			hazards = append(hazards, "replica_identity_removed", "logical_replication_change")
		}
		if isClusteredIndex(current, object) {
			hazards = append(hazards, "clustered_index_removed")
		}
		return hazards
	case pgschema.Constraint:
		return constraintEnforcementHazards(object)
	}
	return []string{"data_loss"}
}
