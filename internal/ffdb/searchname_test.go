package ffdb

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/AlanKK/everythingx/internal/shared"
)

// Names that differ only in characters NFKC folds together.
const (
	nameFullComma = "十年目、帰還を諦めた転移者 v01.zip" // U+3001
	nameHalfComma = "十年目､帰還を諦めた転移者 v02.zip" // U+FF64
)

func searchNames(t *testing.T, term string) []string {
	t.Helper()
	results, err := PrefixSearch(term, 100)
	if err != nil {
		t.Fatalf("search %q: %v", term, err)
	}
	var names []string
	for _, r := range results {
		names = append(names, filepath.Base(r.Fullpath))
	}
	sort.Strings(names)
	return names
}

func assertNames(t *testing.T, term string, want ...string) {
	t.Helper()
	got := searchNames(t, term)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("search %q: got %q, want %q", term, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("search %q: got %q, want %q", term, got, want)
		}
	}
}

// assertFTSIntact checks the FTS index against the content table; it fails
// if a trigger deleted different tokens from the ones it inserted.
func assertFTSIntact(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("INSERT INTO files_fts(files_fts, rank) VALUES('integrity-check', 1)"); err != nil {
		t.Fatalf("FTS integrity check failed: %v", err)
	}
}

func TestSearchName(t *testing.T) {
	cases := map[string]string{
		"十年目､帰還":    "十年目、帰還",
		"ｶﾞﾝﾀﾞﾑ":    "ガンダム",
		"ＡＢＣ１２３":    "ABC123",
		"plain.zip": "plain.zip",
	}
	for in, want := range cases {
		if got := SearchName(in); got != want {
			t.Errorf("SearchName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearch_MatchesAcrossWidthVariants(t *testing.T) {
	db, err := CreateDB(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	events := []shared.EventRecord{
		{Filename: nameFullComma, Path: "/x/" + nameFullComma, ObjectType: shared.ItemIsFile, FoundOnScan: true},
		{Filename: nameHalfComma, Path: "/x/" + nameHalfComma, ObjectType: shared.ItemIsFile, FoundOnScan: true},
		{Filename: "ｶﾞﾝﾀﾞﾑ.zip", Path: "/x/ｶﾞﾝﾀﾞﾑ.zip", ObjectType: shared.ItemIsFile, FoundOnScan: true},
		{Filename: "ＡＢＣ.txt", Path: "/x/ＡＢＣ.txt", ObjectType: shared.ItemIsFile, FoundOnScan: true},
	}
	if err = BulkStoreEvents(db, &events); err != nil {
		t.Fatal(err)
	}

	assertNames(t, "十年目、帰還", nameFullComma, nameHalfComma)
	assertNames(t, "十年目､帰還", nameFullComma, nameHalfComma)
	assertNames(t, "ガンダム", "ｶﾞﾝﾀﾞﾑ.zip")
	assertNames(t, "abc", "ＡＢＣ.txt")
	assertNames(t, "AB", "ＡＢＣ.txt") // short term: prefix search
	// The stored filename keeps its original spelling for display.
	var stored string
	if err = db.QueryRow("SELECT filename FROM files WHERE fullpath = ?", "/x/"+nameHalfComma).Scan(&stored); err != nil || stored != nameHalfComma {
		t.Fatalf("stored filename = %q (%v), want %q", stored, err, nameHalfComma)
	}
	assertFTSIntact(t, db)
}

func TestSearch_DirRenameUpdatesSearchName(t *testing.T) {
	root := t.TempDir()
	db, err := CreateDB(filepath.Join(root, "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	oldDir := filepath.Join(root, "old_ｶﾞ")
	newDir := filepath.Join(root, "十年目､帰還を諦めた")
	scan := []shared.EventRecord{
		{Filename: filepath.Base(oldDir), Path: oldDir, ObjectType: shared.ItemIsDir, FoundOnScan: true},
		{Filename: "v01.zip", Path: oldDir + "/v01.zip", ObjectType: shared.ItemIsFile, FoundOnScan: true},
	}
	if err = BulkStoreEvents(db, &scan); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(newDir, 0o755); err != nil { // the rename has happened on disk
		t.Fatal(err)
	}
	rename := []shared.EventRecord{
		{Filename: filepath.Base(oldDir), Path: oldDir, ObjectType: shared.ItemIsDir, IsRename: true},
		{Filename: filepath.Base(newDir), Path: newDir, ObjectType: shared.ItemIsDir, IsRename: true},
	}
	if err = BulkStoreEvents(db, &rename); err != nil {
		t.Fatal(err)
	}

	assertNames(t, "十年目、帰還を", filepath.Base(newDir))
	assertNames(t, "old_ガ")
	assertNames(t, "v01.zip", "v01.zip")
	assertFTSIntact(t, db)
}

func TestSearch_MtimeRefreshKeepsIndex(t *testing.T) {
	db, err := CreateDB(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, mt := range []int64{1700000000, 1800000000} { // second pass refreshes mtime
		ev := []shared.EventRecord{{Filename: nameHalfComma, Path: "/x/" + nameHalfComma, ObjectType: shared.ItemIsFile, ModTime: mt, FoundOnScan: true}}
		if err = BulkStoreEvents(db, &ev); err != nil {
			t.Fatal(err)
		}
	}
	assertNames(t, "十年目、帰還", nameHalfComma)
	assertFTSIntact(t, db)
}

// createR9Schema builds the schema written by builds before search_name.
func createR9Schema(t *testing.T, path string, names ...string) {
	t.Helper()
	raw, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, s := range []string{
		"CREATE TABLE files(filename TEXT NOT NULL, fullpath TEXT NOT NULL UNIQUE, event_id INTEGER, object_type INTEGER, mtime INTEGER NOT NULL DEFAULT 0)",
		"CREATE INDEX idx_filename ON files(filename COLLATE BINARY, fullpath, object_type)",
		"CREATE VIRTUAL TABLE files_fts USING fts5(filename, content='files', content_rowid='rowid', tokenize='trigram')",
		"CREATE TRIGGER files_ai AFTER INSERT ON files BEGIN INSERT INTO files_fts(rowid, filename) VALUES (new.rowid, new.filename); END",
		"CREATE TRIGGER files_ad AFTER DELETE ON files BEGIN INSERT INTO files_fts(files_fts, rowid, filename) VALUES('delete', old.rowid, old.filename); END",
		"CREATE TRIGGER files_au AFTER UPDATE ON files BEGIN INSERT INTO files_fts(files_fts, rowid, filename) VALUES('delete', old.rowid, old.filename); INSERT INTO files_fts(rowid, filename) VALUES (new.rowid, new.filename); END",
	} {
		if _, err = raw.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range names {
		if _, err = raw.Exec("INSERT INTO files (filename, fullpath, object_type, mtime) VALUES (?, ?, 0, 123)", n, "/x/"+n); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateSearchName_FromR9Schema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r9.db")
	createR9Schema(t, path, nameFullComma, nameHalfComma, "plain.zip")

	// Before the daemon migrates, a read-only client still searches as before.
	ro, err := OpenDBReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if searchNameEnabled {
		t.Fatal("search_name enabled on an unmigrated database")
	}
	assertNames(t, "十年目、帰還", nameFullComma)
	ro.Close()

	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if !searchNameEnabled {
		t.Fatal("search_name not enabled after migration")
	}
	assertNames(t, "十年目、帰還", nameFullComma, nameHalfComma)
	assertNames(t, "plain", "plain.zip")
	var mtime int64
	if err = db.QueryRow("SELECT mtime FROM files WHERE filename = 'plain.zip'").Scan(&mtime); err != nil || mtime != 123 {
		t.Fatalf("mtime after migration = %d (%v), want 123", mtime, err)
	}
	assertFTSIntact(t, db)
	db.Close()

	// Opening again is a no-op, and a migrated database works read-only.
	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	assertFTSIntact(t, db)
	db.Close()
	ro, err = OpenDBReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	assertNames(t, "十年目､帰還", nameFullComma, nameHalfComma)
}

// A row written by an older binary after a rollback has an empty search_name;
// the next start fills it in.
func TestMigrateSearchName_BackfillsEmptyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	db, err := CreateDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO files (filename, fullpath, object_type) VALUES (?, ?, 0)", nameHalfComma, "/x/"+nameHalfComma); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertNames(t, "十年目、帰還", nameHalfComma)
	assertFTSIntact(t, db)
}
