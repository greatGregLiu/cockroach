// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: XisiHuang (cockhuangxh@163.com)

package sql

import (
	"fmt"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/privilege"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/pkg/errors"
)

type dropDatabaseNode struct {
	p      *planner
	n      *parser.DropDatabase
	dbDesc *sqlbase.DatabaseDescriptor
	td     []*sqlbase.TableDescriptor
}

// DropDatabase drops a database.
// Privileges: DROP on database.
//   Notes: postgres allows only the database owner to DROP a database.
//          mysql requires the DROP privileges on the database.
// TODO(XisiHuang): our DROP DATABASE is like the postgres DROP SCHEMA
// (cockroach database == postgres schema). the postgres default of not
// dropping the schema if there are dependent objects is more sensible
// (see the RESTRICT and CASCADE options).
func (p *planner) DropDatabase(n *parser.DropDatabase) (planNode, error) {
	if n.Name == "" {
		return nil, errEmptyDatabaseName
	}

	// Check that the database exists.
	dbDesc, err := p.getDatabaseDesc(string(n.Name))
	if err != nil {
		return nil, err
	}
	if dbDesc == nil {
		if n.IfExists {
			// Noop.
			return &emptyNode{}, nil
		}
		return nil, sqlbase.NewUndefinedDatabaseError(string(n.Name))
	}

	if err := p.checkPrivilege(dbDesc, privilege.DROP); err != nil {
		return nil, err
	}

	tbNames, err := p.getTableNames(dbDesc)
	if err != nil {
		return nil, err
	}

	td := make([]*sqlbase.TableDescriptor, len(tbNames))
	for i := range tbNames {
		tbDesc, err := p.dropTableOrViewPrepare(&tbNames[i])
		if err != nil {
			return nil, err
		}
		if tbDesc == nil {
			// Database claims to have this table, but it does not exist.
			return nil, errors.Errorf("table %q was described by database %q, but does not exist",
				tbNames[i].String(), n.Name)
		}
		// Recursively check permissions on all dependent views, since some may
		// be in different databases.
		for _, ref := range tbDesc.DependedOnBy {
			if err := p.canRemoveDependentView(tbDesc, ref, parser.DropCascade); err != nil {
				return nil, err
			}
		}
		td[i] = tbDesc
	}

	td, err = p.filterCascadedTables(td)
	if err != nil {
		return nil, err
	}

	return &dropDatabaseNode{n: n, p: p, dbDesc: dbDesc, td: td}, nil
}

// filterCascadedTables takes a list of table descriptors and removes any
// descriptors from the list that are dependent on other descriptors in the
// list (e.g. if view v1 depends on table t1, then v1 will be filtered from
// the list).
func (p *planner) filterCascadedTables(
	tables []*sqlbase.TableDescriptor,
) ([]*sqlbase.TableDescriptor, error) {
	// Accumulate the set of all tables/views that will be deleted by cascade
	// behavior so that we can filter them out of the list.
	cascadedTables := make(map[sqlbase.ID]bool)
	for _, desc := range tables {
		if err := p.accumulateDependentTables(cascadedTables, desc); err != nil {
			return nil, err
		}
	}
	filteredTableList := make([]*sqlbase.TableDescriptor, 0, len(tables))
	for _, desc := range tables {
		if !cascadedTables[desc.ID] {
			filteredTableList = append(filteredTableList, desc)
		}
	}
	return filteredTableList, nil
}

func (p *planner) accumulateDependentTables(
	dependentTables map[sqlbase.ID]bool, desc *sqlbase.TableDescriptor,
) error {
	for _, ref := range desc.DependedOnBy {
		dependentTables[ref.ID] = true
		dependentDesc, err := sqlbase.GetTableDescFromID(p.txn, ref.ID)
		if err != nil {
			return err
		}
		if err := p.accumulateDependentTables(dependentTables, dependentDesc); err != nil {
			return err
		}
	}
	return nil
}

func (n *dropDatabaseNode) expandPlan() error {
	return nil
}

func (n *dropDatabaseNode) Start() error {
	tbNameStrings := make([]string, 0, len(n.td))
	for _, tbDesc := range n.td {
		if tbDesc.IsView() {
			cascadedViews, err := n.p.dropViewImpl(tbDesc, parser.DropCascade)
			if err != nil {
				return err
			}
			tbNameStrings = append(tbNameStrings, cascadedViews...)
		} else {
			cascadedViews, err := n.p.dropTableImpl(tbDesc)
			if err != nil {
				return err
			}
			tbNameStrings = append(tbNameStrings, cascadedViews...)
		}
		tbNameStrings = append(tbNameStrings, tbDesc.Name)
	}

	zoneKey, nameKey, descKey := getKeysForDatabaseDescriptor(n.dbDesc)

	b := &client.Batch{}
	if log.V(2) {
		log.Infof(n.p.ctx(), "Del %s", descKey)
		log.Infof(n.p.ctx(), "Del %s", nameKey)
		log.Infof(n.p.ctx(), "Del %s", zoneKey)
	}
	b.Del(descKey)
	b.Del(nameKey)
	// Delete the zone config entry for this database.
	b.Del(zoneKey)

	n.p.setTestingVerifyMetadata(func(systemConfig config.SystemConfig) error {
		for _, key := range [...]roachpb.Key{descKey, nameKey, zoneKey} {
			if err := expectDeleted(systemConfig, key); err != nil {
				return err
			}
		}
		return nil
	})

	if err := n.p.txn.Run(b); err != nil {
		return err
	}

	// Log Drop Database event. This is an auditable log event and is recorded
	// in the same transaction as the table descriptor update.
	if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
		EventLogDropDatabase,
		int32(n.dbDesc.ID),
		int32(n.p.evalCtx.NodeID),
		struct {
			DatabaseName          string
			Statement             string
			User                  string
			DroppedTablesAndViews []string
		}{n.n.Name.String(), n.n.String(), n.p.session.User, tbNameStrings},
	); err != nil {
		return err
	}
	return nil
}

func (n *dropDatabaseNode) Next() (bool, error)                 { return false, nil }
func (n *dropDatabaseNode) Close()                              {}
func (n *dropDatabaseNode) Columns() ResultColumns              { return make(ResultColumns, 0) }
func (n *dropDatabaseNode) Ordering() orderingInfo              { return orderingInfo{} }
func (n *dropDatabaseNode) Values() parser.DTuple               { return parser.DTuple{} }
func (n *dropDatabaseNode) DebugValues() debugValues            { return debugValues{} }
func (n *dropDatabaseNode) ExplainTypes(_ func(string, string)) {}
func (n *dropDatabaseNode) SetLimitHint(_ int64, _ bool)        {}
func (n *dropDatabaseNode) setNeededColumns(_ []bool)           {}
func (n *dropDatabaseNode) MarkDebug(mode explainMode)          {}
func (n *dropDatabaseNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "drop database", "", nil
}

type dropIndexNode struct {
	p        *planner
	n        *parser.DropIndex
	idxNames []fullIndexName
}

type fullIndexName struct {
	tn      *parser.TableName
	idxName parser.Name
}

// DropIndex drops an index.
// Privileges: CREATE on table.
//   Notes: postgres allows only the index owner to DROP an index.
//          mysql requires the INDEX privilege on the table.
func (p *planner) DropIndex(n *parser.DropIndex) (planNode, error) {
	idxNames := make([]fullIndexName, len(n.IndexList))
	for i, index := range n.IndexList {
		tn, err := p.expandIndexName(index)
		if err != nil {
			return nil, err
		}

		tableDesc, err := p.mustGetTableDesc(tn)
		if err != nil {
			return nil, err
		}

		if err := p.checkPrivilege(tableDesc, privilege.CREATE); err != nil {
			return nil, err
		}

		idxNames[i].tn = tn
		idxNames[i].idxName = index.Index
	}
	return &dropIndexNode{n: n, p: p, idxNames: idxNames}, nil
}

func (n *dropIndexNode) expandPlan() error {
	return nil
}

func (n *dropIndexNode) Start() error {
	for _, index := range n.idxNames {
		// Need to retrieve the descriptor again for each index name in
		// the list: when two or more index names refer to the same table,
		// the mutation list and new version number created by the first
		// drop need to be visible to the second drop.
		tableDesc, err := n.p.getTableDesc(index.tn)
		if err != nil || tableDesc == nil {
			// newPlan() and Start() ultimately run within the same
			// transaction. If we got a descriptor during newPlan(), we
			// must have it here too.
			panic(fmt.Sprintf("table descriptor for %s became unavailable within same txn", index.tn))
		}
		status, i, err := tableDesc.FindIndexByName(index.idxName)
		if err != nil {
			if n.n.IfExists {
				// Noop.
				continue
			}
			// Index does not exist, but we want it to: error out.
			return err
		}
		// Queue the mutation.
		var droppedViews []string
		switch status {
		case sqlbase.DescriptorActive:
			idx := tableDesc.Indexes[i]

			if idx.ForeignKey.IsSet() {
				if n.n.DropBehavior != parser.DropCascade {
					return fmt.Errorf("index %q is in use as a foreign key constraint", idx.Name)
				}
				if err := n.p.removeFKBackReference(tableDesc, idx); err != nil {
					return err
				}
			}
			if len(idx.Interleave.Ancestors) > 0 {
				if err := n.p.removeInterleaveBackReference(tableDesc, idx); err != nil {
					return err
				}
			}

			for _, ref := range idx.ReferencedBy {
				fetched, err := n.p.canRemoveFK(idx.Name, ref, n.n.DropBehavior)
				if err != nil {
					return err
				}
				if err := n.p.removeFK(ref, fetched); err != nil {
					return err
				}
			}
			for _, ref := range idx.InterleavedBy {
				if err := n.p.removeInterleave(ref); err != nil {
					return err
				}
			}

			for _, tableRef := range tableDesc.DependedOnBy {
				if tableRef.IndexID == idx.ID {
					// Ensure that we have DROP privilege on all dependent views
					err := n.p.canRemoveDependentViewGeneric(
						"index", idx.Name, tableDesc.ParentID, tableRef, n.n.DropBehavior)
					if err != nil {
						return err
					}
					viewDesc, err := n.p.getViewDescForCascade(
						"index", idx.Name, tableDesc.ParentID, tableRef.ID, n.n.DropBehavior)
					if err != nil {
						return err
					}
					cascadedViews, err := n.p.removeDependentView(tableDesc, viewDesc)
					if err != nil {
						return err
					}
					droppedViews = append(droppedViews, viewDesc.Name)
					droppedViews = append(droppedViews, cascadedViews...)
				}
			}

			tableDesc.AddIndexMutation(tableDesc.Indexes[i], sqlbase.DescriptorMutation_DROP)
			tableDesc.Indexes = append(tableDesc.Indexes[:i], tableDesc.Indexes[i+1:]...)

		case sqlbase.DescriptorIncomplete:
			switch tableDesc.Mutations[i].Direction {
			case sqlbase.DescriptorMutation_ADD:
				return fmt.Errorf("index %q in the middle of being added, try again later", index.idxName)

			case sqlbase.DescriptorMutation_DROP:
				continue
			}
		}
		mutationID, err := tableDesc.FinalizeMutation()
		if err != nil {
			return err
		}
		if err := tableDesc.Validate(n.p.txn); err != nil {
			return err
		}
		if err := n.p.writeTableDesc(tableDesc); err != nil {
			return err
		}
		// Record index drop in the event log. This is an auditable log event
		// and is recorded in the same transaction as the table descriptor
		// update.
		if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
			EventLogDropIndex,
			int32(tableDesc.ID),
			int32(n.p.evalCtx.NodeID),
			struct {
				TableName           string
				IndexName           string
				Statement           string
				User                string
				MutationID          uint32
				CascadeDroppedViews []string
			}{tableDesc.Name, string(index.idxName), n.n.String(), n.p.session.User, uint32(mutationID),
				droppedViews},
		); err != nil {
			return err
		}
		n.p.notifySchemaChange(tableDesc.ID, mutationID)
	}
	return nil
}

func (n *dropIndexNode) Next() (bool, error)                 { return false, nil }
func (n *dropIndexNode) Close()                              {}
func (n *dropIndexNode) Columns() ResultColumns              { return make(ResultColumns, 0) }
func (n *dropIndexNode) Ordering() orderingInfo              { return orderingInfo{} }
func (n *dropIndexNode) Values() parser.DTuple               { return parser.DTuple{} }
func (n *dropIndexNode) DebugValues() debugValues            { return debugValues{} }
func (n *dropIndexNode) ExplainTypes(_ func(string, string)) {}
func (n *dropIndexNode) SetLimitHint(_ int64, _ bool)        {}
func (n *dropIndexNode) setNeededColumns(_ []bool)           {}
func (n *dropIndexNode) MarkDebug(mode explainMode)          {}
func (n *dropIndexNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "drop index", "", nil
}

type dropViewNode struct {
	p  *planner
	n  *parser.DropView
	td []*sqlbase.TableDescriptor
}

// DropView drops a view.
// Privileges: DROP on view.
//   Notes: postgres allows only the view owner to DROP a view.
//          mysql requires the DROP privilege on the view.
func (p *planner) DropView(n *parser.DropView) (planNode, error) {
	td := make([]*sqlbase.TableDescriptor, 0, len(n.Names))
	for _, name := range n.Names {
		tn, err := name.NormalizeTableName()
		if err != nil {
			return nil, err
		}
		if err := tn.QualifyWithDatabase(p.session.Database); err != nil {
			return nil, err
		}

		droppedDesc, err := p.dropTableOrViewPrepare(tn)
		if err != nil {
			return nil, err
		}
		if droppedDesc == nil {
			if n.IfExists {
				continue
			}
			// View does not exist, but we want it to: error out.
			return nil, sqlbase.NewUndefinedViewError(name.String())
		}
		if !droppedDesc.IsView() {
			return nil, sqlbase.NewWrongObjectTypeError(name.String(), "view")
		}

		td = append(td, droppedDesc)
	}

	// Ensure this view isn't depended on by any other views, or that if it is
	// then `cascade` was specified or it was also explicitly specified in the
	// DROP VIEW command.
	for _, droppedDesc := range td {
		for _, ref := range droppedDesc.DependedOnBy {
			// Don't verify that we can remove a dependent view if that dependent
			// view was explicitly specified in the DROP VIEW command.
			if descInSlice(ref.ID, td) {
				continue
			}
			if err := p.canRemoveDependentView(droppedDesc, ref, n.DropBehavior); err != nil {
				return nil, err
			}
		}
	}

	if len(td) == 0 {
		return &emptyNode{}, nil
	}
	return &dropViewNode{p: p, n: n, td: td}, nil
}

func descInSlice(descID sqlbase.ID, td []*sqlbase.TableDescriptor) bool {
	for _, desc := range td {
		if descID == desc.ID {
			return true
		}
	}
	return false
}

func (n *dropViewNode) expandPlan() error {
	return nil
}

func (n *dropViewNode) Start() error {
	for _, droppedDesc := range n.td {
		if droppedDesc == nil {
			continue
		}
		cascadeDroppedViews, err := n.p.dropViewImpl(droppedDesc, n.n.DropBehavior)
		if err != nil {
			return err
		}
		// Log a Drop View event for this table. This is an auditable log event
		// and is recorded in the same transaction as the table descriptor
		// update.
		if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
			EventLogDropView,
			int32(droppedDesc.ID),
			int32(n.p.evalCtx.NodeID),
			struct {
				ViewName            string
				Statement           string
				User                string
				CascadeDroppedViews []string
			}{droppedDesc.Name, n.n.String(), n.p.session.User, cascadeDroppedViews},
		); err != nil {
			return err
		}
	}
	return nil
}

func (n *dropViewNode) Next() (bool, error)                 { return false, nil }
func (n *dropViewNode) Close()                              {}
func (n *dropViewNode) Columns() ResultColumns              { return make(ResultColumns, 0) }
func (n *dropViewNode) Ordering() orderingInfo              { return orderingInfo{} }
func (n *dropViewNode) Values() parser.DTuple               { return parser.DTuple{} }
func (n *dropViewNode) ExplainTypes(_ func(string, string)) {}
func (n *dropViewNode) DebugValues() debugValues            { return debugValues{} }
func (n *dropViewNode) SetLimitHint(_ int64, _ bool)        {}
func (n *dropViewNode) setNeededColumns(_ []bool)           {}
func (n *dropViewNode) MarkDebug(mode explainMode)          {}
func (n *dropViewNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "drop view", "", nil
}

type dropTableNode struct {
	p  *planner
	n  *parser.DropTable
	td []*sqlbase.TableDescriptor
}

// DropTable drops a table.
// Privileges: DROP on table.
//   Notes: postgres allows only the table owner to DROP a table.
//          mysql requires the DROP privilege on the table.
func (p *planner) DropTable(n *parser.DropTable) (planNode, error) {
	td := make([]*sqlbase.TableDescriptor, 0, len(n.Names))
	for _, name := range n.Names {
		tn, err := name.NormalizeTableName()
		if err != nil {
			return nil, err
		}
		if err := tn.QualifyWithDatabase(p.session.Database); err != nil {
			return nil, err
		}

		droppedDesc, err := p.dropTableOrViewPrepare(tn)
		if err != nil {
			return nil, err
		}
		if droppedDesc == nil {
			if n.IfExists {
				continue
			}
			// Table does not exist, but we want it to: error out.
			return nil, sqlbase.NewUndefinedTableError(name.String())
		}
		if !droppedDesc.IsTable() {
			return nil, sqlbase.NewWrongObjectTypeError(name.String(), "table")
		}

		for _, idx := range droppedDesc.AllNonDropIndexes() {
			for _, ref := range idx.ReferencedBy {
				if _, err := p.canRemoveFK(droppedDesc.Name, ref, n.DropBehavior); err != nil {
					return nil, err
				}
			}
			for _, ref := range idx.InterleavedBy {
				if err := p.canRemoveInterleave(droppedDesc.Name, ref, n.DropBehavior); err != nil {
					return nil, err
				}
			}
		}
		for _, ref := range droppedDesc.DependedOnBy {
			if err := p.canRemoveDependentView(droppedDesc, ref, n.DropBehavior); err != nil {
				return nil, err
			}
		}
		td = append(td, droppedDesc)
	}

	if len(td) == 0 {
		return &emptyNode{}, nil
	}
	return &dropTableNode{p: p, n: n, td: td}, nil
}

func (n *dropTableNode) expandPlan() error {
	return nil
}

func (p *planner) canRemoveFK(
	from string, ref sqlbase.ForeignKeyReference, behavior parser.DropBehavior,
) (*sqlbase.TableDescriptor, error) {
	table, err := sqlbase.GetTableDescFromID(p.txn, ref.Table)
	if err != nil {
		return nil, err
	}
	if behavior != parser.DropCascade {
		return nil, fmt.Errorf("%q is referenced by foreign key from table %q", from, table.Name)
	}
	if err := p.checkPrivilege(table, privilege.CREATE); err != nil {
		return nil, err
	}
	return table, nil
}

func (p *planner) canRemoveInterleave(
	from string, ref sqlbase.ForeignKeyReference, behavior parser.DropBehavior,
) error {
	table, err := sqlbase.GetTableDescFromID(p.txn, ref.Table)
	if err != nil {
		return err
	}
	// TODO(dan): It's possible to DROP a table that has a child interleave, but
	// some loose ends would have to be addresssed. The zone would have to be
	// kept and deleted when the last table in it is removed. Also, the dropped
	// table's descriptor would have to be kept around in some Dropped but
	// non-public state for referential integrity of the `InterleaveDescriptor`
	// pointers.
	if behavior != parser.DropCascade {
		return util.UnimplementedWithIssueErrorf(
			8036, "%q is interleaved by table %q", from, table.Name)
	}
	if err := p.checkPrivilege(table, privilege.CREATE); err != nil {
		return err
	}
	return nil
}

func (p *planner) canRemoveDependentView(
	from *sqlbase.TableDescriptor,
	ref sqlbase.TableDescriptor_Reference,
	behavior parser.DropBehavior,
) error {
	return p.canRemoveDependentViewGeneric(from.TypeName(), from.Name, from.ParentID, ref, behavior)
}

func (p *planner) canRemoveDependentViewGeneric(
	typeName string,
	objName string,
	parentID sqlbase.ID,
	ref sqlbase.TableDescriptor_Reference,
	behavior parser.DropBehavior,
) error {
	viewDesc, err := p.getViewDescForCascade(typeName, objName, parentID, ref.ID, behavior)
	if err != nil {
		return err
	}
	if err := p.checkPrivilege(viewDesc, privilege.DROP); err != nil {
		return err
	}
	// If this view is depended on by other views, we have to check them as well.
	for _, ref := range viewDesc.DependedOnBy {
		if err := p.canRemoveDependentView(viewDesc, ref, behavior); err != nil {
			return err
		}
	}
	return nil
}

func (p *planner) removeFK(ref sqlbase.ForeignKeyReference, table *sqlbase.TableDescriptor) error {
	if table == nil {
		var err error
		table, err = sqlbase.GetTableDescFromID(p.txn, ref.Table)
		if err != nil {
			return err
		}
	}
	idx, err := table.FindIndexByID(ref.Index)
	if err != nil {
		return err
	}
	idx.ForeignKey = sqlbase.ForeignKeyReference{}
	return p.saveNonmutationAndNotify(table)
}

func (p *planner) removeInterleave(ref sqlbase.ForeignKeyReference) error {
	table, err := sqlbase.GetTableDescFromID(p.txn, ref.Table)
	if err != nil {
		return err
	}
	idx, err := table.FindIndexByID(ref.Index)
	if err != nil {
		return err
	}
	idx.Interleave.Ancestors = nil
	return p.saveNonmutationAndNotify(table)
}

// Drops the view and any additional views that depend on it.
// Returns the names of any additional views that were also dropped
// due to `cascade` behavior.
func (p *planner) removeDependentView(
	tableDesc, viewDesc *sqlbase.TableDescriptor,
) ([]string, error) {
	// In the table whose index is being removed, filter out all back-references
	// that refer to the view that's being removed.
	tableDesc.DependedOnBy = removeMatchingReferences(tableDesc.DependedOnBy, viewDesc.ID)
	// Then proceed to actually drop the view and log an event for it.
	return p.dropViewImpl(viewDesc, parser.DropCascade)
}

func (n *dropTableNode) Start() error {
	for _, droppedDesc := range n.td {
		if droppedDesc == nil {
			continue
		}
		droppedViews, err := n.p.dropTableImpl(droppedDesc)
		if err != nil {
			return err
		}
		// Log a Drop Table event for this table. This is an auditable log event
		// and is recorded in the same transaction as the table descriptor
		// update.
		if err := MakeEventLogger(n.p.leaseMgr).InsertEventRecord(n.p.txn,
			EventLogDropTable,
			int32(droppedDesc.ID),
			int32(n.p.evalCtx.NodeID),
			struct {
				TableName           string
				Statement           string
				User                string
				CascadeDroppedViews []string
			}{droppedDesc.Name, n.n.String(), n.p.session.User, droppedViews},
		); err != nil {
			return err
		}
	}
	return nil
}

func (n *dropTableNode) Next() (bool, error)                 { return false, nil }
func (n *dropTableNode) Close()                              {}
func (n *dropTableNode) Columns() ResultColumns              { return make(ResultColumns, 0) }
func (n *dropTableNode) Ordering() orderingInfo              { return orderingInfo{} }
func (n *dropTableNode) Values() parser.DTuple               { return parser.DTuple{} }
func (n *dropTableNode) ExplainTypes(_ func(string, string)) {}
func (n *dropTableNode) DebugValues() debugValues            { return debugValues{} }
func (n *dropTableNode) SetLimitHint(_ int64, _ bool)        {}
func (n *dropTableNode) setNeededColumns(_ []bool)           {}
func (n *dropTableNode) MarkDebug(mode explainMode)          {}
func (n *dropTableNode) ExplainPlan(v bool) (string, string, []planNode) {
	return "drop table", "", nil
}

// dropTableOrViewPrepare/dropTableImpl is used to drop a single table by
// name, which can result from either a DROP TABLE or DROP DATABASE
// statement. This method returns the dropped table descriptor, to be
// used for the purpose of logging the event.  The table is not
// actually truncated or deleted synchronously. Instead, it is marked
// as deleted (meaning up_version is set and deleted is set) and the
// actual deletion happens async in a schema changer. Note that,
// courtesy of up_version, the actual truncation and dropping will
// only happen once every node ACKs the version of the descriptor with
// the deleted bit set, meaning the lease manager will not hand out
// new leases for it and existing leases are released).
// If the table does not exist, this function returns a nil descriptor.
func (p *planner) dropTableOrViewPrepare(name *parser.TableName) (*sqlbase.TableDescriptor, error) {
	tableDesc, err := p.getTableOrViewDesc(name)
	if err != nil {
		return nil, err
	}
	if tableDesc == nil {
		return nil, err
	}

	if err := p.checkPrivilege(tableDesc, privilege.DROP); err != nil {
		return nil, err
	}
	return tableDesc, nil
}

// dropTableImpl does the work of dropping a table (and everything that depends
// on it if `cascade` is enabled). It returns a list of view names that were
// dropped due to `cascade` behavior.
func (p *planner) dropTableImpl(tableDesc *sqlbase.TableDescriptor) ([]string, error) {
	var droppedViews []string

	// Remove FK and interleave relationships.
	for _, idx := range tableDesc.AllNonDropIndexes() {
		if idx.ForeignKey.IsSet() {
			if err := p.removeFKBackReference(tableDesc, idx); err != nil {
				return droppedViews, err
			}
		}
		if len(idx.Interleave.Ancestors) > 0 {
			if err := p.removeInterleaveBackReference(tableDesc, idx); err != nil {
				return droppedViews, err
			}
		}
		for _, ref := range idx.ReferencedBy {
			// Nil forces re-fetching tables, since they may have been modified.
			if err := p.removeFK(ref, nil); err != nil {
				return droppedViews, err
			}
		}
		for _, ref := range idx.InterleavedBy {
			if err := p.removeInterleave(ref); err != nil {
				return droppedViews, err
			}
		}
	}

	// Drop all views that depend on this table, assuming that we wouldn't have
	// made it to this point if `cascade` wasn't enabled.
	for _, ref := range tableDesc.DependedOnBy {
		viewDesc, err := p.getViewDescForCascade(
			tableDesc.TypeName(), tableDesc.Name, tableDesc.ParentID, ref.ID, parser.DropCascade)
		if err != nil {
			return droppedViews, err
		}
		cascadedViews, err := p.dropViewImpl(viewDesc, parser.DropCascade)
		if err != nil {
			return droppedViews, err
		}
		droppedViews = append(droppedViews, cascadedViews...)
		droppedViews = append(droppedViews, viewDesc.Name)
	}

	if err := p.initiateDropTable(tableDesc); err != nil {
		return droppedViews, err
	}

	p.setTestingVerifyMetadata(func(systemConfig config.SystemConfig) error {
		return verifyDropTableMetadata(systemConfig, tableDesc.ID, "table")
	})
	return droppedViews, nil
}

func (p *planner) initiateDropTable(tableDesc *sqlbase.TableDescriptor) error {
	if err := tableDesc.SetUpVersion(); err != nil {
		return err
	}
	tableDesc.State = sqlbase.TableDescriptor_DROP
	if err := p.writeTableDesc(tableDesc); err != nil {
		return err
	}
	p.notifySchemaChange(tableDesc.ID, sqlbase.InvalidMutationID)
	return nil
}

func (p *planner) removeFKBackReference(
	tableDesc *sqlbase.TableDescriptor, idx sqlbase.IndexDescriptor,
) error {
	t, err := sqlbase.GetTableDescFromID(p.txn, idx.ForeignKey.Table)
	if err != nil {
		return errors.Errorf("error resolving referenced table ID %d: %v", idx.ForeignKey.Table, err)
	}
	targetIdx, err := t.FindIndexByID(idx.ForeignKey.Index)
	if err != nil {
		return err
	}
	for k, ref := range targetIdx.ReferencedBy {
		if ref.Table == tableDesc.ID && ref.Index == idx.ID {
			targetIdx.ReferencedBy = append(targetIdx.ReferencedBy[:k], targetIdx.ReferencedBy[k+1:]...)
		}
	}
	return p.saveNonmutationAndNotify(t)
}

func (p *planner) removeInterleaveBackReference(
	tableDesc *sqlbase.TableDescriptor, idx sqlbase.IndexDescriptor,
) error {
	if len(idx.Interleave.Ancestors) == 0 {
		return nil
	}
	ancestor := idx.Interleave.Ancestors[len(idx.Interleave.Ancestors)-1]
	t, err := sqlbase.GetTableDescFromID(p.txn, ancestor.TableID)
	if err != nil {
		return errors.Errorf("error resolving referenced table ID %d: %v", ancestor.TableID, err)
	}
	targetIdx, err := t.FindIndexByID(ancestor.IndexID)
	if err != nil {
		return err
	}
	for k, ref := range targetIdx.InterleavedBy {
		if ref.Table == tableDesc.ID && ref.Index == idx.ID {
			targetIdx.InterleavedBy = append(targetIdx.InterleavedBy[:k], targetIdx.InterleavedBy[k+1:]...)
		}
	}
	return p.saveNonmutationAndNotify(t)
}

func verifyDropTableMetadata(
	systemConfig config.SystemConfig, tableID sqlbase.ID, objType string,
) error {
	desc, err := GetTableDesc(systemConfig, tableID)
	if err != nil {
		return err
	}
	if desc == nil {
		return errors.Errorf("%s %d missing", objType, tableID)
	}
	if desc.Dropped() {
		return nil
	}
	return errors.Errorf("expected %s %d to be marked as deleted", objType, tableID)
}

// dropViewImpl does the work of dropping a view (and views that depend on it
// if `cascade is specified`). Returns the names of any additional views that
// were also dropped due to `cascade` behavior.
func (p *planner) dropViewImpl(
	viewDesc *sqlbase.TableDescriptor, behavior parser.DropBehavior,
) ([]string, error) {
	var cascadeDroppedViews []string

	// Remove back-references from the tables/views this view depends on.
	for _, depID := range viewDesc.DependsOn {
		dependencyDesc, err := sqlbase.GetTableDescFromID(p.txn, depID)
		if err != nil {
			return cascadeDroppedViews,
				errors.Errorf("error resolving dependency relation ID %d: %v", depID, err)
		}
		// The dependency is also being deleted, so we don't have to remove the
		// references.
		if dependencyDesc.Dropped() {
			continue
		}
		dependencyDesc.DependedOnBy = removeMatchingReferences(dependencyDesc.DependedOnBy, viewDesc.ID)
		if err := p.saveNonmutationAndNotify(dependencyDesc); err != nil {
			return cascadeDroppedViews, err
		}
	}
	viewDesc.DependsOn = nil

	if behavior == parser.DropCascade {
		for _, ref := range viewDesc.DependedOnBy {
			dependentDesc, err := p.getViewDescForCascade(
				viewDesc.TypeName(), viewDesc.Name, viewDesc.ParentID, ref.ID, behavior)
			if err != nil {
				return cascadeDroppedViews, err
			}
			cascadedViews, err := p.dropViewImpl(dependentDesc, behavior)
			if err != nil {
				return cascadeDroppedViews, err
			}
			cascadeDroppedViews = append(cascadeDroppedViews, cascadedViews...)
			cascadeDroppedViews = append(cascadeDroppedViews, dependentDesc.Name)
		}
	}

	if err := p.initiateDropTable(viewDesc); err != nil {
		return cascadeDroppedViews, err
	}

	p.setTestingVerifyMetadata(func(systemConfig config.SystemConfig) error {
		return verifyDropTableMetadata(systemConfig, viewDesc.ID, "view")
	})
	return cascadeDroppedViews, nil
}

// truncateAndDropTable batches all the commands required for truncating and
// deleting the table descriptor. It is called from a mutation, async wrt the
// DROP statement. Before this method is called, the table has already been
// marked for deletion and has been purged from the descriptor cache on all
// nodes. No node is reading/writing data on the table at this stage,
// therefore the entire table can be deleted with no concern for conflicts (we
// can even eliminate the need to use a transaction for each chunk at a later
// stage if it proves inefficient).
func truncateAndDropTable(
	ctx context.Context, tableDesc *sqlbase.TableDescriptor, db *client.DB,
) error {
	if err := truncateTableInChunks(ctx, tableDesc, db); err != nil {
		return err
	}

	// Finished deleting all the table data, now delete the table meta data.
	return db.Txn(ctx, func(txn *client.Txn) error {
		zoneKey, nameKey, descKey := getKeysForTableDescriptor(tableDesc)
		// Delete table descriptor
		b := &client.Batch{}
		b.Del(descKey)
		b.Del(nameKey)
		// Delete the zone config entry for this table.
		b.Del(zoneKey)
		txn.SetSystemConfigTrigger()
		return txn.Run(b)
	})
}

// removeMatchingReferences removes all refs from the provided slice that
// match the provided ID, returning the modified slice.
func removeMatchingReferences(
	refs []sqlbase.TableDescriptor_Reference, id sqlbase.ID,
) []sqlbase.TableDescriptor_Reference {
	updatedRefs := refs[:0]
	for _, ref := range refs {
		if ref.ID != id {
			updatedRefs = append(updatedRefs, ref)
		}
	}
	return updatedRefs
}

func (p *planner) getViewDescForCascade(
	typeName string, objName string, parentID, viewID sqlbase.ID, behavior parser.DropBehavior,
) (*sqlbase.TableDescriptor, error) {
	viewDesc, err := sqlbase.GetTableDescFromID(p.txn, viewID)
	if err != nil {
		log.Warningf(p.ctx(), "unable to retrieve descriptor for view %d: %v", viewID, err)
		return nil, errors.Wrapf(err, "error resolving dependent view ID %d", viewID)
	}
	if behavior != parser.DropCascade {
		viewName := viewDesc.Name
		if viewDesc.ParentID != parentID {
			var err error
			viewName, err = p.getQualifiedTableName(viewDesc)
			if err != nil {
				log.Warningf(p.ctx(), "unable to retrieve qualified name of view %d: %v", viewID, err)
				return nil, sqlbase.NewDependentObjectError(
					"cannot drop %s %q because a view depends on it", typeName, objName)
			}
		}
		return nil, sqlbase.NewDependentObjectError("cannot drop %s %q because view %q depends on it",
			typeName, objName, viewName)
	}
	return viewDesc, nil
}
