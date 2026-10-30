// SPDX-License-Identifier: Apache-2.0

// Package capture consumes a branch's logical replication slot, decodes
// pgoutput into row events tagged with the agent run in effect, and persists
// them on main before acknowledging the slot (spec §7).
package capture

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pglogrepl"

	"github.com/keeptalking83/pgweir/internal/changeset"
	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/meta"
	"github.com/keeptalking83/pgweir/internal/schema"
)

// TypeResolver returns format_type(oid, typmod) as computed on the branch
// with search_path = pg_catalog.
type TypeResolver func(oid uint32, typmod int32) (string, error)

// Txn is one decoded branch transaction.
type Txn struct {
	Xid       uint32
	CommitLSN pglogrepl.LSN
	EndLSN    pglogrepl.LSN
	Events    []changeset.Event
}

type relation struct {
	msg   *pglogrepl.RelationMessage
	table *schema.Table
}

// Decoder turns pgoutput messages into transactions. It holds no I/O except
// the type resolver used by the schema drift check.
type Decoder struct {
	tables    map[schema.Ref]*schema.Table
	resolve   TypeResolver
	relations map[uint32]*relation
	checked   map[uint32]bool

	inTx bool
	cur  Txn
	run  json.RawMessage
}

var untaggedRun = json.RawMessage(`{"run_id":"untagged"}`)

// NewDecoder returns a decoder for the given tracked tables (main's
// definitions).
func NewDecoder(tables []*schema.Table, resolve TypeResolver) *Decoder {
	d := &Decoder{
		tables:    map[schema.Ref]*schema.Table{},
		resolve:   resolve,
		relations: map[uint32]*relation{},
		checked:   map[uint32]bool{},
	}
	for _, t := range tables {
		d.tables[t.Ref] = t
	}
	return d
}

// InTx reports whether the decoder is between Begin and Commit.
func (d *Decoder) InTx() bool { return d.inTx }

// Decode processes one pgoutput message. It returns a transaction when msg
// is a Commit.
func (d *Decoder) Decode(data []byte) (*Txn, error) {
	msg, err := pglogrepl.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse pgoutput message: %w", err)
	}
	switch m := msg.(type) {
	case *pglogrepl.BeginMessage:
		d.inTx = true
		d.cur = Txn{Xid: m.Xid}
		d.run = untaggedRun
	case *pglogrepl.RelationMessage:
		return nil, d.relation(m)
	case *pglogrepl.TypeMessage, *pglogrepl.OriginMessage:
		// Type metadata and origins are not needed: values arrive as text.
	case *pglogrepl.LogicalDecodingMessage:
		return nil, d.message(m)
	case *pglogrepl.InsertMessage:
		return nil, d.insert(m)
	case *pglogrepl.UpdateMessage:
		return nil, d.update(m)
	case *pglogrepl.DeleteMessage:
		return nil, d.delete(m)
	case *pglogrepl.TruncateMessage:
		var names []string
		for _, id := range m.RelationIDs {
			if r, ok := d.relations[id]; ok {
				names = append(names, r.table.String())
			}
		}
		return nil, errs.New(errs.TruncateNotSupported,
			"TRUNCATE on the branch (%s) cannot be merged; drop the branch and start a new one", strings.Join(names, ", "))
	case *pglogrepl.CommitMessage:
		if !d.inTx {
			return nil, fmt.Errorf("commit without begin at %s", m.CommitLSN)
		}
		d.inTx = false
		t := d.cur
		t.CommitLSN = m.CommitLSN
		t.EndLSN = m.TransactionEndLSN
		d.cur = Txn{}
		return &t, nil
	default:
		return nil, fmt.Errorf("unexpected pgoutput message %T", msg)
	}
	return nil, nil
}

// relation records a Relation message and checks it against main: column
// names and types in order (by name, never by OID), and replica identity.
func (d *Decoder) relation(m *pglogrepl.RelationMessage) error {
	ref := schema.Ref{Schema: m.Namespace, Name: m.RelationName}
	t, ok := d.tables[ref]
	if !ok {
		return errs.New(errs.UntrackedTable, "change to untracked table %s on the branch", ref)
	}
	if m.ReplicaIdentity != 'f' {
		return errs.New(errs.ReplicaIdentityNotFull, "table %s on the branch does not have REPLICA IDENTITY FULL", ref)
	}
	cols := make([]schema.Column, len(m.Columns))
	for i, c := range m.Columns {
		typ, err := d.resolve(c.DataType, c.TypeModifier)
		if err != nil {
			return fmt.Errorf("resolve type of %s.%s: %w", ref, c.Name, err)
		}
		cols[i] = schema.Column{Name: c.Name, Type: typ}
	}
	if diffs := schema.CompareColumns(t, cols); len(diffs) > 0 {
		return errs.New(errs.SchemaDrift, "table %s differs between main and branch:\n  - %s", ref, strings.Join(diffs, "\n  - "))
	}
	d.relations[m.RelationID] = &relation{msg: m, table: t}
	return nil
}

// message applies a pgweir tag: it becomes the run for the rest of the
// transaction. Messages with other prefixes are ignored.
func (d *Decoder) message(m *pglogrepl.LogicalDecodingMessage) error {
	if m.Prefix != meta.TagPrefix || !m.Transactional || !d.inTx {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(m.Content, &obj); err != nil {
		return errs.New(errs.InvalidTag, "tag in transaction %d is not a JSON object: %s", d.cur.Xid, truncate(m.Content))
	}
	id, ok := obj["run_id"].(string)
	if !ok || id == "" {
		return errs.New(errs.InvalidTag, "tag in transaction %d has no string run_id: %s", d.cur.Xid, truncate(m.Content))
	}
	canon, err := changeset.Canonical(obj)
	if err != nil {
		return err
	}
	d.run = canon
	return nil
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

func (d *Decoder) rel(id uint32) (*relation, error) {
	r, ok := d.relations[id]
	if !ok {
		return nil, fmt.Errorf("change for unknown relation id %d", id)
	}
	return r, nil
}

func (d *Decoder) insert(m *pglogrepl.InsertMessage) error {
	r, err := d.rel(m.RelationID)
	if err != nil {
		return err
	}
	row, err := decodeTuple(r, m.Tuple, nil)
	if err != nil {
		return err
	}
	d.add(r, changeset.OpInsert, nil, row)
	return nil
}

func (d *Decoder) update(m *pglogrepl.UpdateMessage) error {
	r, err := d.rel(m.RelationID)
	if err != nil {
		return err
	}
	if m.OldTupleType != pglogrepl.UpdateMessageTupleTypeOld || m.OldTuple == nil {
		return errs.New(errs.ReplicaIdentityNotFull, "update on %s arrived without the full old row", r.table)
	}
	oldRow, err := decodeTuple(r, m.OldTuple, nil)
	if err != nil {
		return err
	}
	newRow, err := decodeTuple(r, m.NewTuple, oldRow)
	if err != nil {
		return err
	}
	pk := r.table.PK
	if !oldRow.Project(pk).Equal(newRow.Project(pk)) {
		// A primary-key change is a delete of the old key and an insert of
		// the new one (spec §8.1).
		d.add(r, changeset.OpDelete, oldRow, nil)
		d.add(r, changeset.OpInsert, nil, newRow)
		return nil
	}
	d.add(r, changeset.OpUpdate, oldRow, newRow)
	return nil
}

func (d *Decoder) delete(m *pglogrepl.DeleteMessage) error {
	r, err := d.rel(m.RelationID)
	if err != nil {
		return err
	}
	if m.OldTupleType != pglogrepl.UpdateMessageTupleTypeOld || m.OldTuple == nil {
		return errs.New(errs.ReplicaIdentityNotFull, "delete on %s arrived without the full old row", r.table)
	}
	oldRow, err := decodeTuple(r, m.OldTuple, nil)
	if err != nil {
		return err
	}
	d.add(r, changeset.OpDelete, oldRow, nil)
	return nil
}

func (d *Decoder) add(r *relation, op string, oldRow, newRow changeset.Row) {
	src := newRow
	if src == nil {
		src = oldRow
	}
	d.cur.Events = append(d.cur.Events, changeset.Event{
		Xid:    int64(d.cur.Xid),
		Op:     op,
		Schema: r.table.Schema,
		Table:  r.table.Name,
		PK:     src.Project(r.table.PK),
		Old:    oldRow,
		New:    newRow,
		Run:    d.run,
	})
}

// decodeTuple converts tuple data to a Row. Unchanged TOAST values ('u') are
// taken from old, which REPLICA IDENTITY FULL guarantees for updates.
func decodeTuple(r *relation, td *pglogrepl.TupleData, old changeset.Row) (changeset.Row, error) {
	if td == nil {
		return nil, fmt.Errorf("missing tuple data for %s", r.table)
	}
	cols := r.msg.Columns
	if len(td.Columns) != len(cols) {
		return nil, errs.New(errs.SchemaDrift, "tuple for %s has %d columns, relation has %d", r.table, len(td.Columns), len(cols))
	}
	row := make(changeset.Row, len(cols))
	for i, c := range td.Columns {
		name := cols[i].Name
		switch c.DataType {
		case pglogrepl.TupleDataTypeNull:
			row[name] = nil
		case pglogrepl.TupleDataTypeText:
			s := string(c.Data) // copies: the receive buffer is reused
			row[name] = &s
		case pglogrepl.TupleDataTypeToast:
			v, ok := old[name]
			if old == nil || !ok {
				return nil, errs.New(errs.ToastWithoutOld, "unchanged TOAST value of %s.%s without an old row", r.table, name)
			}
			row[name] = v
		case pglogrepl.TupleDataTypeBinary:
			return nil, errs.New(errs.BinaryNotSupported, "binary tuple data for %s.%s", r.table, name)
		default:
			return nil, fmt.Errorf("unknown tuple data type %q for %s.%s", c.DataType, r.table, name)
		}
	}
	return row, nil
}
