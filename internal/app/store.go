package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	DB       *sql.DB
	mediaDir string
}

func Open(ctx context.Context, path, media string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(media, 0700); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db, media}
	if _, err = db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err == nil {
		err = s.migrate(ctx)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) migrate(ctx context.Context) error {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(entries) {
		return errors.New("database schema is newer than this binary")
	}
	for i := version; i < len(entries); i++ {
		source, err := migrations.ReadFile("migrations/" + entries[i].Name())
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(source)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", i+1)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func audit(ctx context.Context, tx *sql.Tx, actor, operation, subject string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO audit(at,actor,operation,subject) VALUES(?,?,?,?)", time.Now().UnixMilli(), actor, operation, subject)
	return err
}
func fingerprint(r Request) string {
	b, _ := json.Marshal(r)
	x := sha256.Sum256(b)
	return hex.EncodeToString(x[:])
}
func (s *Store) existing(ctx context.Context, p Principal, r Request) (string, error) {
	var campaign, hash string
	err := s.DB.QueryRowContext(ctx, "SELECT id,fingerprint FROM campaigns WHERE actor=? AND request_key=?", p.ID, r.Key).Scan(&campaign, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err == nil && hash != fingerprint(r) {
		return "", ErrConflict
	}
	return campaign, err
}
func (s *Store) create(ctx context.Context, p Principal, r Request, at time.Time, zone string, accounts map[string]Account) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	campaign, now := id(), time.Now().UnixMilli()
	hash := fingerprint(r)
	res, err := tx.ExecContext(ctx, "INSERT INTO campaigns VALUES(?,?,?,?,?) ON CONFLICT(actor,request_key) DO NOTHING", campaign, p.ID, r.Key, hash, now)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n == 0 {
		var current string
		if err = tx.QueryRowContext(ctx, "SELECT id,fingerprint FROM campaigns WHERE actor=? AND request_key=?", p.ID, r.Key).Scan(&campaign, &current); err != nil {
			return "", err
		}
		if current != hash {
			return "", ErrConflict
		}
		return campaign, tx.Commit()
	}
	for _, t := range r.Targets {
		blob, _ := json.Marshal(t)
		_, err = tx.ExecContext(ctx, "INSERT INTO deliveries(id,campaign,platform,account,binding,payload,zone,scheduled,due,state,updated) VALUES(?,?,?,?,?,?,?,?,?,'scheduled',?)", id(), campaign, t.Platform, t.Account, accounts[t.Account].binding(), blob, zone, at.UnixMilli(), at.UnixMilli(), now)
		if err != nil {
			return "", err
		}
	}
	if err = audit(ctx, tx, p.ID, "create", campaign); err != nil {
		return "", err
	}
	return campaign, tx.Commit()
}

type scanner interface{ Scan(...any) error }

const columns = "id,campaign,binding,payload,zone,scheduled,due,state,tries,external_id,external_url,failure"

func scanDelivery(row scanner) (Delivery, error) {
	var d Delivery
	var payload []byte
	var scheduled, due int64
	err := row.Scan(&d.ID, &d.Campaign, &d.Binding, &payload, &d.Zone, &scheduled, &due, &d.State, &d.Tries, &d.ExternalID, &d.ExternalURL, &d.Failure)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrMissing
	}
	if err != nil {
		return d, err
	}
	d.Scheduled = time.UnixMilli(scheduled).UTC()
	d.Due = time.UnixMilli(due).UTC()
	if loc, err := time.LoadLocation(d.Zone); err == nil {
		d.Local = d.Scheduled.In(loc).Format(time.RFC3339)
	}
	return d, json.Unmarshal(payload, &d.Target)
}
func allowedRead(p Principal, d Delivery) bool { return p.allows(d.Target) }
func (s *Store) get(ctx context.Context, p Principal, key string) ([]Delivery, error) {
	ds, err := s.query(ctx, "SELECT "+columns+" FROM deliveries WHERE (campaign=? OR id=?) AND campaign IN (SELECT id FROM campaigns WHERE actor=? OR ?) ORDER BY id", key, key, p.ID, p.Admin)
	if err != nil {
		return nil, err
	}
	out := []Delivery{}
	for _, d := range ds {
		if allowedRead(p, d) {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, ErrMissing
	}
	return out, nil
}
func (s *Store) query(ctx context.Context, query string, args ...any) ([]Delivery, error) {
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ds := []Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		ds = append(ds, d)
	}
	return ds, rows.Err()
}
func (s *Store) list(ctx context.Context, p Principal, f Filter) ([]Delivery, error) {
	if f.Limit == 0 {
		f.Limit = 50
	}
	if f.Limit < 1 || f.Limit > 200 {
		return nil, invalid("limit must be 1 through 200")
	}
	sqlText := "SELECT " + columns + " FROM deliveries WHERE campaign IN (SELECT id FROM campaigns WHERE actor=? OR ?)"
	args := []any{p.ID, p.Admin}
	if f.State == "pending" {
		sqlText += " AND state IN ('scheduled','retry_wait')"
	} else if f.State != "" {
		sqlText += " AND state=?"
		args = append(args, f.State)
	}
	if f.Platform != "" {
		sqlText += " AND platform=?"
		args = append(args, f.Platform)
	}
	if f.Account != "" {
		sqlText += " AND account=?"
		args = append(args, f.Account)
	}
	if !f.From.IsZero() {
		sqlText += " AND scheduled>=?"
		args = append(args, f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		sqlText += " AND scheduled<?"
		args = append(args, f.To.UnixMilli())
	}
	if f.BeforeID != "" {
		sqlText += " AND (scheduled,id)<(SELECT scheduled,id FROM deliveries WHERE id=?)"
		args = append(args, f.BeforeID)
	}
	rows, err := s.DB.QueryContext(ctx, sqlText+" ORDER BY scheduled DESC,id DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for len(out) < f.Limit && rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		if allowedRead(p, d) {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}
func (s *Store) attempts(ctx context.Context, key string) ([]Attempt, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT number,started,finished,COALESCE(outcome,''),COALESCE(detail,'') FROM attempts WHERE delivery=? ORDER BY number", key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		var a Attempt
		var start int64
		var finish sql.NullInt64
		if err := rows.Scan(&a.Number, &start, &finish, &a.Outcome, &a.Detail); err != nil {
			return nil, err
		}
		a.Start = time.UnixMilli(start).UTC()
		if finish.Valid {
			t := time.UnixMilli(finish.Int64).UTC()
			a.Finish = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) mutate(ctx context.Context, p Principal, d Delivery, t Target, at time.Time, zone string, cancel bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var res sql.Result
	now := time.Now().UnixMilli()
	operation := "update"
	if cancel {
		operation = "cancel"
		res, err = tx.ExecContext(ctx, "UPDATE deliveries SET state='cancelled',updated=? WHERE id=? AND state IN ('scheduled','retry_wait')", now, d.ID)
	} else {
		blob, _ := json.Marshal(t)
		previous, _ := json.Marshal(d.Target)
		res, err = tx.ExecContext(ctx, "UPDATE deliveries SET payload=?,zone=?,scheduled=?,due=?,state='scheduled',updated=? WHERE id=? AND state IN ('scheduled','retry_wait') AND tries=? AND scheduled=? AND payload=? AND zone=? AND due=?", blob, zone, at.UnixMilli(), at.UnixMilli(), now, d.ID, d.Tries, d.Scheduled.UnixMilli(), previous, d.Zone, d.Due.UnixMilli())
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	if err = audit(ctx, tx, p.ID, operation, d.ID); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) claim(ctx context.Context, now time.Time, lease time.Duration) (Delivery, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Delivery{}, err
	}
	defer tx.Rollback()
	stamp := now.UnixMilli()
	detail := "worker lease expired; publication outcome unknown"
	_, err = tx.ExecContext(ctx, "UPDATE attempts SET finished=?,outcome='uncertain',detail=? WHERE finished IS NULL AND delivery IN (SELECT id FROM deliveries WHERE state='processing' AND lease<=?)", stamp, detail, stamp)
	if err != nil {
		return Delivery{}, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE deliveries SET state='uncertain',failure=?,lease=0,updated=? WHERE state='processing' AND lease<=?", detail, stamp, stamp)
	if err != nil {
		return Delivery{}, err
	}
	d, err := scanDelivery(tx.QueryRowContext(ctx, "UPDATE deliveries SET state='processing',lease=?,tries=tries+1,updated=? WHERE id=(SELECT id FROM deliveries WHERE state IN ('scheduled','retry_wait') AND due<=? ORDER BY due,id LIMIT 1) RETURNING "+columns, now.Add(lease).UnixMilli(), stamp, stamp))
	if errors.Is(err, ErrMissing) {
		if commitErr := tx.Commit(); commitErr != nil {
			return d, commitErr
		}
		return d, err
	}
	if err != nil {
		return d, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO attempts(delivery,number,started) VALUES(?,?,?)", d.ID, d.Tries, stamp); err != nil {
		return d, err
	}
	return d, tx.Commit()
}
func (s *Store) finish(ctx context.Context, d Delivery, result Result, state, kind, message string, due time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, "UPDATE deliveries SET state=?,due=?,lease=0,external_id=?,external_url=?,failure=?,updated=? WHERE id=? AND state='processing' AND tries=?", state, due.UnixMilli(), result.ID, result.URL, message, now, d.ID, d.Tries)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, "UPDATE attempts SET finished=?,outcome=?,detail=? WHERE delivery=? AND number=?", now, kind, message, d.ID, d.Tries); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) backup(ctx context.Context, dst string) error {
	if !filepath.IsAbs(dst) || strings.ContainsRune(dst, 0) {
		return invalid("backup path must be absolute")
	}
	if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
		return ErrConflict
	}
	return s.backupSnapshot(ctx, dst)
}
func (s *Store) backupSnapshot(ctx context.Context, dst string) error {
	_, err := s.DB.ExecContext(ctx, "VACUUM INTO ?", dst)
	return err
}

func (s *Store) QuarantineRestored(ctx context.Context) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	detail := "snapshot restored; publication may have occurred after the backup"
	if _, err = tx.ExecContext(ctx, "UPDATE attempts SET finished=?,outcome='uncertain',detail=? WHERE finished IS NULL AND delivery IN (SELECT id FROM deliveries WHERE state='processing')", now, detail); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE deliveries SET state='uncertain',failure=?,lease=0,updated=? WHERE state IN ('scheduled','retry_wait','processing')", detail, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM oauth_state; DELETE FROM oauth_links"); err != nil {
		return err
	}
	if err = audit(ctx, tx, "operator", "snapshot_restored", "all_unfinished_deliveries"); err != nil {
		return err
	}
	return tx.Commit()
}
