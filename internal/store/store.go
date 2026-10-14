// Package store persists managed configuration and account traffic checkpoints.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 3

var (
	ErrClockRollback       = errors.New("accounting period moved backwards")
	ErrInvalidSample       = errors.New("invalid traffic sample")
	ErrAdministratorExists = errors.New("administrator already exists")
	beijing                = time.FixedZone("Asia/Shanghai", 8*60*60)
)

type Store struct {
	DB *sql.DB
}

type Account struct {
	ID         string
	Name       string
	QuotaBytes int64
	Enabled    bool
	StatsFault bool
	Unlimited  bool
}

type Usage struct {
	AccountID string
	Period    string
	Upload    int64
	Download  int64
}

type AccountStatus struct {
	Account   Account
	Usage     Usage
	Allowed   bool
	Reason    string
	Unlimited bool
}

// Administrator.ID is the associated account's stable ledger ID. Its name is
// read from accounts and its password hash is never included in JSON output.
type Administrator struct {
	ID   string
	Name string
	Hash string `json:"-"`
}

// Period identifies a calendar month in Beijing time, independently of host TZ.
func Period(now time.Time) string {
	return now.In(beijing).Format("2006-01")
}

// Open requires a private state directory and initializes the schema atomically.
// It never changes permissions on an existing parent such as /tmp or /var/lib.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("state directory must be private (0700): %s", dir)
	}
	if info, err := os.Lstat(abs); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("database path must be a regular file, not a symbolic link")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open database file: %w", err)
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "synchronous(FULL)")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Set("_txlock", "immediate")
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection per Store also serializes calls within the management process.
	// The DSN applies PRAGMAs again if the pool replaces this connection.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) migrate() error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema %d is newer than supported schema %d", version, schemaVersion)
	}
	if version == 0 {
		if _, err := tx.Exec(schema); err != nil {
			return fmt.Errorf("initialize database schema: %w", err)
		}
	}
	if version == 1 {
		if _, err := tx.Exec("ALTER TABLE accounts ADD COLUMN unlimited INTEGER NOT NULL DEFAULT 0 CHECK (unlimited IN (0, 1))"); err != nil {
			return fmt.Errorf("migrate account schema from version 1: %w", err)
		}
	}
	if version == 1 || version == 2 {
		if _, err := tx.Exec(administratorSchema); err != nil {
			return fmt.Errorf("migrate administrator schema: %w", err)
		}
	}
	if version < schemaVersion {
		if _, err := tx.Exec("PRAGMA user_version = 3"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const schema = `
CREATE TABLE accounts (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL UNIQUE,
    quota_bytes INTEGER NOT NULL CHECK (quota_bytes >= 0),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    stats_fault INTEGER NOT NULL DEFAULT 0 CHECK (stats_fault IN (0, 1)),
    unlimited INTEGER NOT NULL DEFAULT 0 CHECK (unlimited IN (0, 1))
);
CREATE TABLE nodes (
    id TEXT PRIMARY KEY NOT NULL,
    name TEXT NOT NULL UNIQUE,
    template BLOB NOT NULL,
    legacy INTEGER NOT NULL DEFAULT 0 CHECK (legacy IN (0, 1))
);
CREATE TABLE bindings (
    id TEXT PRIMARY KEY NOT NULL,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    inbound BLOB NOT NULL,
    UNIQUE (account_id, node_id)
);
CREATE INDEX bindings_node ON bindings(node_id);
CREATE TABLE instances (
    account_id TEXT PRIMARY KEY NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    stats_port INTEGER NOT NULL UNIQUE CHECK (stats_port BETWEEN 1 AND 65535),
    generation TEXT NOT NULL DEFAULT '',
    process_id TEXT NOT NULL DEFAULT '',
    config_hash TEXT NOT NULL DEFAULT ''
);
CREATE TABLE settings (
    key TEXT PRIMARY KEY NOT NULL,
    value TEXT NOT NULL
);
CREATE TABLE usage (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    period TEXT NOT NULL,
    upload INTEGER NOT NULL DEFAULT 0 CHECK (upload >= 0),
    download INTEGER NOT NULL DEFAULT 0 CHECK (download >= 0),
    PRIMARY KEY (account_id, period)
);
CREATE TABLE samples (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    generation TEXT NOT NULL,
    name TEXT NOT NULL,
    value INTEGER NOT NULL CHECK (value >= 0),
    PRIMARY KEY (account_id, generation, name)
);
CREATE TABLE operations (
    id INTEGER PRIMARY KEY,
    event TEXT NOT NULL,
    created_at TEXT NOT NULL
);` + administratorSchema

const administratorSchema = `
CREATE TABLE administrators (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    account_id TEXT NOT NULL UNIQUE REFERENCES accounts(id) ON DELETE CASCADE,
    password_hash TEXT NOT NULL CHECK (length(password_hash) > 0)
);`

func (s *Store) CreateAccount(id, name string, quota int64) error {
	return s.createAccount(id, name, quota, false)
}

// CreateUnlimitedAccount retains the same explicit ID allocation as
// CreateAccount. Unlimited is an independent flag, not a quota-byte sentinel.
func (s *Store) CreateUnlimitedAccount(id, name string) error {
	return s.createAccount(id, name, 0, true)
}

func (s *Store) createAccount(id, name string, quota int64, unlimited bool) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" {
		return errors.New("account id and name are required")
	}
	if quota < 0 {
		return errors.New("quota cannot be negative")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO accounts(id, name, quota_bytes, unlimited) VALUES (?, ?, ?, ?)", id, name, quota, unlimited); err != nil {
		return err
	}
	if err := record(tx, "account_created:"+id, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateAdministrator creates the sole application administrator atomically.
// A matching account name reuses its ledger ID and all existing state, with
// only the Unlimited flag enabled. Password hashing belongs to the caller.
func (s *Store) CreateAdministrator(id, name, hash string) (string, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(hash) == "" {
		return "", errors.New("administrator id, name and password hash are required")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRow("SELECT account_id FROM administrators WHERE id = 1").Scan(&existing)
	if err == nil {
		return "", ErrAdministratorExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var accountID string
	err = tx.QueryRow("SELECT id FROM accounts WHERE name = ?", name).Scan(&accountID)
	switch {
	case err == nil:
		if _, err := tx.Exec("UPDATE accounts SET unlimited = 1 WHERE id = ?", accountID); err != nil {
			return "", err
		}
	case errors.Is(err, sql.ErrNoRows):
		accountID = id
		if _, err := tx.Exec("INSERT INTO accounts(id, name, quota_bytes, unlimited) VALUES (?, ?, 0, 1)", id, name); err != nil {
			return "", err
		}
		if err := record(tx, "account_created:"+id, time.Now()); err != nil {
			return "", err
		}
	default:
		return "", err
	}
	if _, err := tx.Exec("INSERT INTO administrators(id, account_id, password_hash) VALUES (1, ?, ?)", accountID, hash); err != nil {
		return "", err
	}
	if err := record(tx, "administrator_created:"+accountID, time.Now()); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return accountID, nil
}

func (s *Store) Administrator() (Administrator, error) {
	var a Administrator
	err := s.DB.QueryRow(`SELECT accounts.id, accounts.name, administrators.password_hash
        FROM administrators JOIN accounts ON accounts.id = administrators.account_id
        WHERE administrators.id = 1`).Scan(&a.ID, &a.Name, &a.Hash)
	return a, err
}

func (s *Store) SetAdministratorPassword(hash string) error {
	if strings.TrimSpace(hash) == "" {
		return errors.New("administrator password hash is required")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE administrators SET password_hash = ? WHERE id = 1", hash)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	if err := record(tx, "administrator_password_changed", time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RenameAdministrator(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("administrator name is required")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRow("SELECT account_id FROM administrators WHERE id = 1").Scan(&id); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE accounts SET name = ? WHERE id = ?", name, id); err != nil {
		return err
	}
	if err := record(tx, "administrator_renamed:"+id, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Account(id string) (Account, error) {
	var a Account
	err := s.DB.QueryRow("SELECT id, name, quota_bytes, enabled, stats_fault, unlimited FROM accounts WHERE id = ?", id).
		Scan(&a.ID, &a.Name, &a.QuotaBytes, &a.Enabled, &a.StatsFault, &a.Unlimited)
	return a, err
}

func (s *Store) Accounts() ([]Account, error) {
	rows, err := s.DB.Query("SELECT id, name, quota_bytes, enabled, stats_fault, unlimited FROM accounts ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := []Account{}
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Name, &a.QuotaBytes, &a.Enabled, &a.StatsFault, &a.Unlimited); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func (s *Store) SetQuota(id string, quota int64) error {
	if quota < 0 {
		return errors.New("quota cannot be negative")
	}
	return s.changeAccount(id, "quota_bytes = ?, unlimited = 0", quota, "quota_changed:")
}

func (s *Store) SetUnlimited(id string) error {
	return s.changeAccount(id, "unlimited = ?", true, "unlimited_enabled:")
}

func (s *Store) SetEnabled(id string, enabled bool) error {
	return s.changeAccount(id, "enabled = ?", enabled, "enabled_changed:")
}

func (s *Store) SetFault(id string, fault bool) error {
	return s.changeAccount(id, "stats_fault = ?", fault, "stats_fault_changed:")
}

// assignment and event come only from the fixed calls above, never user input.
func (s *Store) changeAccount(id, assignment string, value any, event string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("UPDATE accounts SET "+assignment+" WHERE id = ?", value, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	if err := record(tx, event+id, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) EnsurePeriod(now time.Time) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ensurePeriod(tx, now); err != nil {
		return err
	}
	return tx.Commit()
}

func ensurePeriod(tx *sql.Tx, now time.Time) error {
	period := Period(now)
	var previous string
	err := tx.QueryRow("SELECT value FROM settings WHERE key = 'active_period'").Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if previous != "" && period < previous {
		return fmt.Errorf("%w: current %s, recorded %s", ErrClockRollback, period, previous)
	}
	if previous != period {
		if _, err := tx.Exec("INSERT INTO settings(key, value) VALUES ('active_period', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", period); err != nil {
			return err
		}
		if err := record(tx, "period_started:"+period, now); err != nil {
			return err
		}
	}
	_, err = tx.Exec("INSERT INTO usage(account_id, period) SELECT id, ? FROM accounts WHERE true ON CONFLICT(account_id, period) DO NOTHING", period)
	return err
}

func (s *Store) Status(id string, now time.Time) (AccountStatus, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return AccountStatus{}, err
	}
	defer tx.Rollback()
	if err := ensurePeriod(tx, now); err != nil {
		return AccountStatus{}, err
	}
	status, err := statusAt(tx, id, Period(now))
	if err != nil {
		return AccountStatus{}, err
	}
	if err := tx.Commit(); err != nil {
		return AccountStatus{}, err
	}
	return status, nil
}

func statusAt(tx *sql.Tx, id, period string) (AccountStatus, error) {
	var st AccountStatus
	err := tx.QueryRow(`SELECT a.id, a.name, a.quota_bytes, a.enabled, a.stats_fault, a.unlimited,
        u.account_id, u.period, u.upload, u.download
        FROM accounts a JOIN usage u ON u.account_id = a.id
        WHERE a.id = ? AND u.period = ?`, id, period).Scan(
		&st.Account.ID, &st.Account.Name, &st.Account.QuotaBytes, &st.Account.Enabled, &st.Account.StatsFault, &st.Account.Unlimited,
		&st.Usage.AccountID, &st.Usage.Period, &st.Usage.Upload, &st.Usage.Download)
	if err != nil {
		return st, err
	}
	st.Unlimited = st.Account.Unlimited
	switch {
	case !st.Account.Enabled:
		st.Reason = "disabled"
	case st.Account.StatsFault:
		st.Reason = "stats_fault"
	case !st.Account.Unlimited && (st.Usage.Upload >= st.Account.QuotaBytes || st.Usage.Download >= st.Account.QuotaBytes-st.Usage.Upload):
		st.Reason = "quota_exceeded"
	default:
		st.Allowed = true
	}
	return st, nil
}

// ResetUsage changes only this month's ledger. Cumulative core checkpoints stay
// intact, so the next sample cannot charge the pre-reset bytes again.
func (s *Store) ResetUsage(id string, now time.Time) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := ensurePeriod(tx, now); err != nil {
		return err
	}
	result, err := tx.Exec("UPDATE usage SET upload = 0, download = 0 WHERE account_id = ? AND period = ?", id, Period(now))
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	if err := record(tx, "usage_reset:"+id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplySample atomically commits traffic deltas and their core watermarks.
// A new generation starts at zero; the runtime manager must issue a new
// generation only after observing an actual core-process replacement.
// Samples crossing a month boundary are attributed to the observed month. This
// is not byte-exact accounting of traffic between the two observation times.
func (s *Store) ApplySample(id, generation string, counters map[string]int64, now time.Time) (AccountStatus, error) {
	if strings.TrimSpace(generation) == "" {
		return AccountStatus{}, errors.New("sample generation is required")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return AccountStatus{}, err
	}
	defer tx.Rollback()
	if err := ensurePeriod(tx, now); err != nil {
		return AccountStatus{}, err
	}
	before, err := statusAt(tx, id, Period(now))
	if err != nil {
		return AccountStatus{}, err
	}
	names := make([]string, 0, len(counters))
	for name := range counters {
		names = append(names, name)
	}
	sort.Strings(names)
	var upload, download int64
	var invalid string
	for _, name := range names {
		value := counters[name]
		var previous int64
		err := tx.QueryRow("SELECT value FROM samples WHERE account_id = ? AND generation = ? AND name = ?", id, generation, name).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return AccountStatus{}, err
		}
		if value < 0 || value < previous {
			invalid = "counter is negative or moved backwards: " + name
			break
		}
		delta := value - previous
		switch {
		case strings.HasSuffix(name, ">>>traffic>>>uplink"):
			if delta > math.MaxInt64-upload {
				invalid = "upload counter sum overflow"
			} else {
				upload += delta
			}
		case strings.HasSuffix(name, ">>>traffic>>>downlink"):
			if delta > math.MaxInt64-download {
				invalid = "download counter sum overflow"
			} else {
				download += delta
			}
		default:
			invalid = "unsupported counter name: " + name
		}
		if invalid != "" {
			break
		}
	}
	if invalid == "" && (upload > math.MaxInt64-before.Usage.Upload || download > math.MaxInt64-before.Usage.Download) {
		invalid = "usage ledger overflow"
	}
	if invalid == "" && before.Usage.Upload+upload > math.MaxInt64-(before.Usage.Download+download) {
		invalid = "combined usage overflow"
	}
	if invalid != "" {
		if _, err := tx.Exec("UPDATE accounts SET stats_fault = 1 WHERE id = ?", id); err != nil {
			return AccountStatus{}, err
		}
		if !before.Account.StatsFault {
			if err := record(tx, "sample_fault:"+id, now); err != nil {
				return AccountStatus{}, err
			}
		}
		status, err := statusAt(tx, id, Period(now))
		if err != nil {
			return AccountStatus{}, err
		}
		if err := tx.Commit(); err != nil {
			return AccountStatus{}, err
		}
		return status, fmt.Errorf("%w: %s", ErrInvalidSample, invalid)
	}
	if _, err := tx.Exec("UPDATE usage SET upload = upload + ?, download = download + ? WHERE account_id = ? AND period = ?", upload, download, id, Period(now)); err != nil {
		return AccountStatus{}, err
	}
	for _, name := range names {
		if _, err := tx.Exec(`INSERT INTO samples(account_id, generation, name, value) VALUES (?, ?, ?, ?)
            ON CONFLICT(account_id, generation, name) DO UPDATE SET value = excluded.value`, id, generation, name, counters[name]); err != nil {
			return AccountStatus{}, err
		}
	}
	status, err := statusAt(tx, id, Period(now))
	if err != nil {
		return AccountStatus{}, err
	}
	if before.Allowed && status.Reason == "quota_exceeded" {
		if err := record(tx, "quota_exceeded:"+id, now); err != nil {
			return AccountStatus{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AccountStatus{}, err
	}
	return status, nil
}

func record(tx *sql.Tx, event string, now time.Time) error {
	_, err := tx.Exec("INSERT INTO operations(event, created_at) VALUES (?, ?)", event, now.UTC().Format(time.RFC3339Nano))
	return err
}
