// Package store keeps reviews, issues and incidents in one SQLite file per repository. The driver is
// pure Go, so the binary still needs no C compiler.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the database.
type Store struct{ db *sql.DB }

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS reviews (
	sha        TEXT PRIMARY KEY,
	status     TEXT NOT NULL,          -- queued, running, done, failed
	model      TEXT NOT NULL DEFAULT '',
	note       TEXT NOT NULL DEFAULT '', -- what was skipped or why it failed
	started    INTEGER NOT NULL DEFAULT 0,
	finished   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS findings (
	id         INTEGER PRIMARY KEY,
	sha        TEXT NOT NULL REFERENCES reviews(sha) ON DELETE CASCADE,
	path       TEXT NOT NULL,
	line       INTEGER NOT NULL,
	end_line   INTEGER NOT NULL,
	severity   TEXT NOT NULL,
	source     TEXT NOT NULL,          -- rule or model
	rule       TEXT NOT NULL DEFAULT '',
	title      TEXT NOT NULL,
	detail     TEXT NOT NULL DEFAULT '',
	suggestion TEXT NOT NULL DEFAULT '',
	snippet    TEXT NOT NULL DEFAULT '', -- the flagged line, to follow the issue across commits
	doubt      TEXT NOT NULL DEFAULT '', -- why Codist could not back a model claim
	key        TEXT NOT NULL             -- identifies the same issue in later commits
);
CREATE INDEX IF NOT EXISTS findings_sha ON findings(sha);
CREATE INDEX IF NOT EXISTS findings_key ON findings(key);
CREATE TABLE IF NOT EXISTS issues (
	key        TEXT PRIMARY KEY,
	path       TEXT NOT NULL,
	first_sha  TEXT NOT NULL,
	rule       TEXT NOT NULL DEFAULT '', -- the code check that found it, or '' for the model
	snippet    TEXT NOT NULL DEFAULT '', -- the flagged line
	state      TEXT NOT NULL,          -- open, resolved
	fixed_sha  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS incidents (
	id         INTEGER PRIMARY KEY,
	created    INTEGER NOT NULL,
	title      TEXT NOT NULL,
	logs       TEXT NOT NULL,
	analysis   TEXT NOT NULL DEFAULT '{}'
);
`

// Open opens or creates the database for one repository under dir.
func Open(dir, repo string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, repo+".db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite writes one at a time; one connection keeps it simple and safe
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("database: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Review is the state of one commit's review.
type Review struct {
	SHA, Status, Model, Note string
	Started, Finished        time.Time
}

// Finding is one problem in one commit.
type Finding struct {
	ID                            int64
	SHA, Path                     string
	Line, EndLine                 int
	Severity, Source, Rule, Title string
	Detail, Suggestion, Snippet   string
	Doubt, Key                    string
	State                         string // filled when read: open or resolved
	FirstSHA                      string // the commit the issue started in, when not this one (the first-run scan)
}

// SetReview creates or updates a review.
func (s *Store) SetReview(r Review) error {
	_, err := s.db.Exec(`INSERT INTO reviews (sha, status, model, note, started, finished) VALUES (?,?,?,?,?,?)
		ON CONFLICT(sha) DO UPDATE SET status=excluded.status, model=excluded.model, note=excluded.note,
		started=excluded.started, finished=excluded.finished`,
		r.SHA, r.Status, r.Model, r.Note, unix(r.Started), unix(r.Finished))
	return err
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromUnix(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// GetReview reads a review; ok is false if the commit was never reviewed.
func (s *Store) GetReview(sha string) (Review, bool) {
	var r Review
	var started, finished int64
	err := s.db.QueryRow(`SELECT sha, status, model, note, started, finished FROM reviews WHERE sha=?`, sha).
		Scan(&r.SHA, &r.Status, &r.Model, &r.Note, &started, &finished)
	if err != nil {
		return Review{}, false
	}
	r.Started, r.Finished = fromUnix(started), fromUnix(finished)
	return r, true
}

// Reviews reads the status of many commits at once.
func (s *Store) Reviews() map[string]Review {
	out := map[string]Review{}
	rows, err := s.db.Query(`SELECT sha, status, model, note, started, finished FROM reviews`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var r Review
		var started, finished int64
		if rows.Scan(&r.SHA, &r.Status, &r.Model, &r.Note, &started, &finished) == nil {
			r.Started, r.Finished = fromUnix(started), fromUnix(finished)
			out[r.SHA] = r
		}
	}
	return out
}

// ReplaceFindings stores a commit's findings, replacing any from an earlier run, and records each
// as an issue the first time its key is seen.
func (s *Store) ReplaceFindings(sha string, fs []Finding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM findings WHERE sha=?`, sha); err != nil {
		return err
	}
	for _, f := range fs {
		if _, err := tx.Exec(`INSERT INTO findings (sha, path, line, end_line, severity, source, rule, title, detail, suggestion, snippet, doubt, key)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, sha, f.Path, f.Line, f.EndLine, f.Severity, f.Source, f.Rule, f.Title,
			f.Detail, f.Suggestion, f.Snippet, f.Doubt, f.Key); err != nil {
			return err
		}
		first := sha
		if f.FirstSHA != "" {
			first = f.FirstSHA
		}
		if _, err := tx.Exec(`INSERT INTO issues (key, path, first_sha, rule, snippet, state) VALUES (?,?,?,?,?, 'open') ON CONFLICT(key) DO NOTHING`,
			f.Key, f.Path, first, f.Rule, f.Snippet); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Findings reads one commit's findings with their issue state, most severe first.
func (s *Store) Findings(sha string) []Finding {
	rows, err := s.db.Query(`SELECT f.id, f.sha, f.path, f.line, f.end_line, f.severity, f.source, f.rule, f.title, f.detail,
		f.suggestion, f.snippet, f.doubt, f.key, COALESCE(i.state, 'open')
		FROM findings f LEFT JOIN issues i ON i.key = f.key WHERE f.sha=?
		ORDER BY CASE f.severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 WHEN 'low' THEN 3 ELSE 4 END, f.path, f.line`, sha)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Finding
	for rows.Next() {
		var f Finding
		if rows.Scan(&f.ID, &f.SHA, &f.Path, &f.Line, &f.EndLine, &f.Severity, &f.Source, &f.Rule, &f.Title, &f.Detail,
			&f.Suggestion, &f.Snippet, &f.Doubt, &f.Key, &f.State) == nil {
			out = append(out, f)
		}
	}
	return out
}

// Counts is how many findings each reviewed commit has, by severity.
func (s *Store) Counts() map[string]map[string]int {
	out := map[string]map[string]int{}
	rows, err := s.db.Query(`SELECT sha, severity, COUNT(*) FROM findings GROUP BY sha, severity`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var sha, sev string
		var n int
		if rows.Scan(&sha, &sev, &n) == nil {
			if out[sha] == nil {
				out[sha] = map[string]int{}
			}
			out[sha][sev] = n
		}
	}
	return out
}

// OpenIssue is an issue that is still open.
type OpenIssue struct {
	Key, Rule, Snippet string
}

// OpenIssues lists the open issues in one file.
func (s *Store) OpenIssues(path string) []OpenIssue {
	var out []OpenIssue
	rows, err := s.db.Query(`SELECT key, rule, snippet FROM issues WHERE path=? AND state='open'`, path)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var o OpenIssue
		if rows.Scan(&o.Key, &o.Rule, &o.Snippet) == nil {
			out = append(out, o)
		}
	}
	return out
}

// Resolve marks issues as fixed by a commit.
func (s *Store) Resolve(keys []string, sha string) error {
	for _, k := range keys {
		if _, err := s.db.Exec(`UPDATE issues SET state='resolved', fixed_sha=? WHERE key=? AND state='open'`, sha, k); err != nil {
			return err
		}
	}
	return nil
}

// OpenByFile counts open issues per file.
func (s *Store) OpenByFile() map[string]int {
	out := map[string]int{}
	rows, err := s.db.Query(`SELECT path, COUNT(*) FROM issues WHERE state='open' GROUP BY path`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var n int
		if rows.Scan(&p, &n) == nil {
			out[p] = n
		}
	}
	return out
}

// Incident is a production problem a person pasted logs for.
type Incident struct {
	ID       int64
	Created  time.Time
	Title    string
	Logs     string
	Analysis json.RawMessage
}

// AddIncident stores a new incident and returns its id.
func (s *Store) AddIncident(in Incident) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO incidents (created, title, logs, analysis) VALUES (?,?,?,?)`,
		time.Now().UnixMilli(), in.Title, in.Logs, string(orEmpty(in.Analysis)))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetAnalysis saves an incident's analysis.
func (s *Store) SetAnalysis(id int64, analysis json.RawMessage) error {
	_, err := s.db.Exec(`UPDATE incidents SET analysis=? WHERE id=?`, string(orEmpty(analysis)), id)
	return err
}

func orEmpty(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("{}")
	}
	return b
}

// Incident reads one incident.
func (s *Store) Incident(id int64) (Incident, error) {
	var in Incident
	var created int64
	var analysis string
	err := s.db.QueryRow(`SELECT id, created, title, logs, analysis FROM incidents WHERE id=?`, id).
		Scan(&in.ID, &created, &in.Title, &in.Logs, &analysis)
	if errors.Is(err, sql.ErrNoRows) {
		return in, fmt.Errorf("no incident %d", id)
	}
	in.Created, in.Analysis = fromUnix(created), json.RawMessage(analysis)
	return in, err
}

// Incidents lists incidents, newest first.
func (s *Store) Incidents() []Incident {
	rows, err := s.db.Query(`SELECT id, created, title FROM incidents ORDER BY id DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		var in Incident
		var created int64
		if rows.Scan(&in.ID, &created, &in.Title) == nil {
			in.Created = fromUnix(created)
			out = append(out, in)
		}
	}
	return out
}

// Issue is an open issue with where it was last seen.
type Issue struct {
	Key, Path, Severity, Title, Source, Rule, FirstSHA string
	Line                                               int
}

// OpenIssueList lists every open issue, most severe first, each with its latest finding.
func (s *Store) OpenIssueList() []Issue {
	rows, err := s.db.Query(`SELECT i.key, i.path, f.line, f.severity, f.title, f.source, f.rule, i.first_sha
		FROM issues i JOIN findings f ON f.id = (SELECT MAX(id) FROM findings WHERE key = i.key)
		WHERE i.state = 'open'
		ORDER BY CASE f.severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 WHEN 'low' THEN 3 ELSE 4 END, i.path, f.line`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		var is Issue
		if rows.Scan(&is.Key, &is.Path, &is.Line, &is.Severity, &is.Title, &is.Source, &is.Rule, &is.FirstSHA) == nil {
			out = append(out, is)
		}
	}
	return out
}

// TrackedIssue is an issue in any state, with its latest finding and the commits that reported it.
type TrackedIssue struct {
	Key, Path, State, FirstSHA, FixedSHA string
	Latest                               Finding
	Commits                              []string
}

// AllIssues lists every issue, most severe first.
func (s *Store) AllIssues() []TrackedIssue {
	rows, err := s.db.Query(`SELECT i.key, i.path, i.state, i.first_sha, i.fixed_sha,
		f.id, f.sha, f.line, f.end_line, f.severity, f.source, f.rule, f.title, f.detail, f.suggestion, f.snippet, f.doubt
		FROM issues i JOIN findings f ON f.id = (SELECT MAX(id) FROM findings WHERE key = i.key)
		ORDER BY CASE f.severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 WHEN 'low' THEN 3 ELSE 4 END, i.path, f.line`)
	if err != nil {
		return nil
	}
	var out []TrackedIssue
	byKey := map[string]int{}
	for rows.Next() {
		var t TrackedIssue
		f := &t.Latest
		if rows.Scan(&t.Key, &t.Path, &t.State, &t.FirstSHA, &t.FixedSHA, &f.ID, &f.SHA, &f.Line, &f.EndLine, &f.Severity, &f.Source,
			&f.Rule, &f.Title, &f.Detail, &f.Suggestion, &f.Snippet, &f.Doubt) == nil {
			f.Key, f.Path, f.State = t.Key, t.Path, t.State
			byKey[t.Key] = len(out)
			out = append(out, t)
		}
	}
	rows.Close()
	if shas, err := s.db.Query(`SELECT DISTINCT key, sha FROM findings`); err == nil {
		defer shas.Close()
		for shas.Next() {
			var k, sha string
			if shas.Scan(&k, &sha) == nil {
				if i, ok := byKey[k]; ok {
					out[i].Commits = append(out[i].Commits, sha)
				}
			}
		}
	}
	return out
}

// SetIssueState marks an issue open or resolved by hand. A manual fix has no commit.
func (s *Store) SetIssueState(key, state string) error {
	fixed := ""
	if state == "resolved" {
		fixed = "manual"
	}
	res, err := s.db.Exec(`UPDATE issues SET state=?, fixed_sha=? WHERE key=?`, state, fixed, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no issue %s", key)
	}
	return nil
}

// DeleteIncident removes an incident.
func (s *Store) DeleteIncident(id int64) error {
	res, err := s.db.Exec(`DELETE FROM incidents WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no incident %d", id)
	}
	return nil
}

// SetIncidentTitle renames an incident.
func (s *Store) SetIncidentTitle(id int64, title string) error {
	_, err := s.db.Exec(`UPDATE incidents SET title=? WHERE id=?`, title, id)
	return err
}
