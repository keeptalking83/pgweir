// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/keeptalking83/pgweir/internal/branch"
	"github.com/keeptalking83/pgweir/internal/changeset"
	"github.com/keeptalking83/pgweir/internal/errs"
	"github.com/keeptalking83/pgweir/internal/meta"
	"github.com/keeptalking83/pgweir/internal/pg"
	"github.com/keeptalking83/pgweir/internal/schema"
)

// Options configure a capture run.
type Options struct {
	// IdleTimeout: capture stops after the branch sent no data for this long
	// once the target LSN has been reached (spec §7 step 2).
	IdleTimeout time.Duration
	Log         *slog.Logger
	// AfterPersist, if set, runs after each transaction is persisted on main
	// and before the slot is acknowledged. Tests use it to simulate a crash.
	AfterPersist func(commitLSN pglogrepl.LSN) error
}

// Result summarizes a capture run.
type Result struct {
	Transactions int
	Events       int
	TargetLSN    pglogrepl.LSN
	FlushedLSN   pglogrepl.LSN
}

const (
	pollInterval     = 500 * time.Millisecond
	statusInterval   = 10 * time.Second
	unresponsiveness = 60 * time.Second
	eventsPerInsert  = 2000
)

// Run drains the branch's slot up to the branch's current WAL position.
// mainConn must not be inside a transaction. tables are main's definitions
// of the tracked tables.
func Run(ctx context.Context, mainConn *pgx.Conn, b *branch.Branch, tables []*schema.Table, opts Options) (Result, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 2 * time.Second
	}
	var res Result

	var lastSeq int64
	var flushedText string
	err := mainConn.QueryRow(ctx, "SELECT last_seq, flushed_lsn::text FROM pgweir.capture_progress WHERE branch_id = $1::uuid", b.ID).Scan(&lastSeq, &flushedText)
	if err != nil {
		return res, fmt.Errorf("read capture progress: %w", err)
	}
	flushed, err := pglogrepl.ParseLSN(flushedText)
	if err != nil {
		return res, err
	}
	res.FlushedLSN = flushed

	branchConn, err := pg.Connect(ctx, b.DSN)
	if err != nil {
		return res, fmt.Errorf("connect to branch %q: %w", b.Name, err)
	}
	defer func() { _ = branchConn.Close(context.WithoutCancel(ctx)) }()
	var targetText string
	if err := branchConn.QueryRow(ctx, "SELECT pg_catalog.pg_current_wal_lsn()::text").Scan(&targetText); err != nil {
		return res, fmt.Errorf("read branch WAL position: %w", err)
	}
	target, err := pglogrepl.ParseLSN(targetText)
	if err != nil {
		return res, err
	}
	res.TargetLSN = target

	// Main and branch must still agree on every tracked table: events
	// captured earlier are merged against main's current definition.
	refs := make([]schema.Ref, len(tables))
	for i, t := range tables {
		refs[i] = t.Ref
	}
	branchTables, err := schema.Load(ctx, branchConn, refs)
	if err != nil {
		return res, errs.Wrap(errs.SchemaDrift, err, "tracked table missing on branch %q", b.Name)
	}
	for i, mt := range tables {
		if d := schema.Drift(mt, branchTables[i]); len(d) > 0 {
			return res, errs.New(errs.SchemaDrift, "table %s differs between main and branch %q (schema changes are never merged):\n  - %s",
				mt, b.Name, strings.Join(d, "\n  - "))
		}
	}

	resolver := typeResolver(ctx, branchConn)
	dec := NewDecoder(tables, resolver)

	repl, err := pg.ConnectReplication(ctx, b.DSN)
	if err != nil {
		return res, err
	}
	defer func() { _ = repl.Close(context.WithoutCancel(ctx)) }()

	err = pglogrepl.StartReplication(ctx, repl, b.SlotName, flushed, pglogrepl.StartReplicationOptions{
		Mode: pglogrepl.LogicalReplication,
		PluginArgs: []string{
			"proto_version '1'",
			"publication_names " + pg.Literal(meta.Publication),
			"messages 'true'",
		},
	})
	if err != nil {
		if pg.HasSQLState(err, pg.UndefinedObject) {
			_ = branch.SetStatus(ctx, mainConn, b.ID, branch.StatusFailed)
			return res, errs.Wrap(errs.SlotMissing, err,
				"replication slot %s no longer exists on the branch; changes since the last capture are lost and branch %q is now FAILED", b.SlotName, b.Name)
		}
		if pg.HasSQLState(err, pg.ObjectInUse) {
			return res, errs.Wrap(errs.BranchState, err, "another capture of branch %q is running", b.Name)
		}
		return res, fmt.Errorf("start replication: %w", err)
	}
	log.Debug("capture started", "branch", b.Name, "from", flushed, "target", target)

	st := &state{
		ctx: ctx, main: mainConn, repl: repl, branch: b, log: log, opts: opts,
		lastSeq: lastSeq, flushed: flushed, res: &res,
	}
	if err := st.loop(dec, target); err != nil {
		return res, err
	}
	res.FlushedLSN = st.flushed
	return res, nil
}

type state struct {
	ctx     context.Context
	main    *pgx.Conn
	repl    *pgconn.PgConn
	branch  *branch.Branch
	log     *slog.Logger
	opts    Options
	lastSeq int64
	flushed pglogrepl.LSN // last persisted commit
	acked   pglogrepl.LSN // highest position acknowledged to the slot
	res     *Result
}

func (s *state) loop(dec *Decoder, target pglogrepl.LSN) error {
	start := time.Now()
	lastData := start
	lastServerMsg := start
	nextStatus := start.Add(statusInterval)
	var reached pglogrepl.LSN // highest keepalive WAL end seen outside a transaction
	commitReached := false

	for {
		if !dec.InTx() {
			if commitReached {
				break
			}
			if reached >= target && time.Since(lastData) >= s.opts.IdleTimeout {
				break
			}
		}
		if time.Since(lastServerMsg) > unresponsiveness {
			return fmt.Errorf("branch %q stopped responding during capture", s.branch.Name)
		}
		if time.Now().After(nextStatus) {
			if err := s.sendStatus(false); err != nil {
				return err
			}
			nextStatus = time.Now().Add(statusInterval)
		}

		rctx, cancel := context.WithTimeout(s.ctx, pollInterval)
		msg, err := s.repl.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			if s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			if pgconn.Timeout(err) {
				// Ask the server where it is; the reply is a keepalive.
				if !dec.InTx() {
					if err := s.sendStatus(true); err != nil {
						return err
					}
				}
				continue
			}
			return fmt.Errorf("receive from branch: %w", err)
		}
		lastServerMsg = time.Now()

		switch m := msg.(type) {
		case *pgproto3.CopyData:
			if len(m.Data) == 0 {
				continue
			}
			switch m.Data[0] {
			case pglogrepl.PrimaryKeepaliveMessageByteID:
				ka, err := pglogrepl.ParsePrimaryKeepaliveMessage(m.Data[1:])
				if err != nil {
					return fmt.Errorf("parse keepalive: %w", err)
				}
				if !dec.InTx() && ka.ServerWALEnd > reached {
					reached = ka.ServerWALEnd
				}
				if ka.ReplyRequested {
					if err := s.sendStatus(false); err != nil {
						return err
					}
				}
			case pglogrepl.XLogDataByteID:
				xld, err := pglogrepl.ParseXLogData(m.Data[1:])
				if err != nil {
					return fmt.Errorf("parse XLogData: %w", err)
				}
				lastData = time.Now()
				txn, err := dec.Decode(xld.WALData)
				if err != nil {
					return err
				}
				if txn == nil {
					continue
				}
				if txn.CommitLSN <= s.flushed {
					// Already persisted by an earlier run (spec §7 step 10).
					continue
				}
				if err := s.persist(txn); err != nil {
					return err
				}
				lastServerMsg = time.Now() // persisting is our own work, not server silence
				if txn.CommitLSN >= target {
					commitReached = true
				}
			}
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("branch replication error: %w", pgconn.ErrorResponseToPgError(m))
		default:
			s.log.Debug("ignoring replication message", "type", fmt.Sprintf("%T", msg))
		}
	}

	// Every transaction that committed before the last keepalive position
	// has been delivered and persisted, so acknowledging that position lets
	// the server recycle WAL the slot no longer needs. flushed_lsn (and so a
	// merge's end_lsn) stays at the last captured commit, which keeps the
	// changeset identical across runs while the branch is idle.
	if reached > s.flushed {
		if err := s.ack(reached); err != nil {
			return err
		}
	}
	s.log.Debug("capture finished", "transactions", s.res.Transactions, "events", s.res.Events, "flushed", s.flushed)
	return nil
}

func (s *state) sendStatus(reply bool) error {
	pos := max(s.flushed, s.acked)
	err := pglogrepl.SendStandbyStatusUpdate(s.ctx, s.repl, pglogrepl.StandbyStatusUpdate{
		WALWritePosition: pos,
		WALFlushPosition: pos,
		WALApplyPosition: pos,
		ReplyRequested:   reply,
	})
	if err != nil {
		return fmt.Errorf("send status to branch: %w", err)
	}
	return nil
}

// persist stores a transaction's events on main in one transaction, then
// acknowledges its commit LSN to the slot (spec §7 step 8).
func (s *state) persist(txn *Txn) error {
	tx, err := s.main.Begin(s.ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(s.ctx) }()
	seq := s.lastSeq
	for start := 0; start < len(txn.Events); start += eventsPerInsert {
		end := min(start+eventsPerInsert, len(txn.Events))
		batch := make([]eventJSON, 0, end-start)
		for _, e := range txn.Events[start:end] {
			seq++
			batch = append(batch, eventJSON{Seq: seq, Op: e.Op, Schema: e.Schema, Table: e.Table, PK: e.PK, Old: e.Old, New: e.New, Run: e.Run})
		}
		payload, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		if start > 0 {
			// Keep the walsender alive (wal_sender_timeout) while a large
			// transaction is stored; this acknowledges nothing new.
			if err := s.sendStatus(false); err != nil {
				return err
			}
		}
		_, err = tx.Exec(s.ctx, `
INSERT INTO pgweir.events (branch_id, seq, xid, commit_lsn, op, table_schema, table_name, pk, old_row, new_row, run)
SELECT $1::uuid, (e->>'seq')::bigint, $2::bigint, $3::pg_lsn, e->>'op', e->>'schema', e->>'table', e->'pk',
       NULLIF(e->'old', 'null'::jsonb), NULLIF(e->'new', 'null'::jsonb), e->'run'
FROM jsonb_array_elements($4::jsonb) AS e`, s.branch.ID, int64(txn.Xid), txn.CommitLSN.String(), string(payload))
		if err != nil {
			return fmt.Errorf("persist events: %w", err)
		}
	}
	_, err = tx.Exec(s.ctx, "UPDATE pgweir.capture_progress SET last_seq = $2, flushed_lsn = $3::pg_lsn WHERE branch_id = $1::uuid",
		s.branch.ID, seq, txn.CommitLSN.String())
	if err != nil {
		return fmt.Errorf("persist capture progress: %w", err)
	}
	if err := tx.Commit(s.ctx); err != nil {
		return fmt.Errorf("persist events: %w", err)
	}
	s.lastSeq = seq
	s.flushed = txn.CommitLSN
	s.res.Transactions++
	s.res.Events += len(txn.Events)
	if s.opts.AfterPersist != nil {
		if err := s.opts.AfterPersist(txn.CommitLSN); err != nil {
			return err
		}
	}
	return s.sendStatus(false)
}

// ack acknowledges lsn to the slot without changing flushed_lsn.
func (s *state) ack(lsn pglogrepl.LSN) error {
	s.acked = lsn
	return s.sendStatus(false)
}

type eventJSON struct {
	Seq    int64           `json:"seq"`
	Op     string          `json:"op"`
	Schema string          `json:"schema"`
	Table  string          `json:"table"`
	PK     changeset.Row   `json:"pk"`
	Old    changeset.Row   `json:"old"`
	New    changeset.Row   `json:"new"`
	Run    json.RawMessage `json:"run"`
}

// typeResolver resolves type OIDs on the branch with format_type under
// search_path = pg_catalog (spec §7 step 7), caching results.
func typeResolver(ctx context.Context, conn *pgx.Conn) TypeResolver {
	type key struct {
		oid    uint32
		typmod int32
	}
	cache := map[key]string{}
	return func(oid uint32, typmod int32) (string, error) {
		k := key{oid, typmod}
		if s, ok := cache[k]; ok {
			return s, nil
		}
		var s string
		err := schema.WithCatalogPath(ctx, conn, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT format_type($1::oid, $2::int)", oid, typmod).Scan(&s)
		})
		if err != nil {
			return "", err
		}
		cache[k] = s
		return s, nil
	}
}
