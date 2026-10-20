package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/swiftdiaries/az-agent-platform-go/internal/defstore"
	"github.com/swiftdiaries/az-agent-platform-go/internal/journal"
	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

// copyTree copies the directory src into a fresh temp directory and returns it.
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func bundleFixture(t *testing.T) string { return copyTree(t, "../../configs") }

func activateEnv(pool *pgxpool.Pool) func(string) string {
	// pool.Config().ConnString() names the shared server's database; point it at the test's isolated one.
	u, err := neturl.Parse(pool.Config().ConnString())
	if err != nil {
		panic(err)
	}
	u.Path = "/" + pool.Config().ConnConfig.Database
	url := u.String()
	return func(name string) string {
		if name == envDatabaseURL {
			return url
		}
		return ""
	}
}

func runActivateWith(pool *pgxpool.Pool, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = runActivate(context.Background(), args, activateEnv(pool), &out, &errOut)
	return code, out.String(), errOut.String()
}

func currentDigest(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	digest, err := journal.New(pool).CurrentDefinition(context.Background())
	if err != nil {
		t.Fatalf("CurrentDefinition: %v", err)
	}
	return digest
}

func activated(t *testing.T, stdout string) string {
	t.Helper()
	return digestAfter(t, stdout, "activated ")
}

// digestAfter returns the 64-hex digest that follows prefix at the start of the single stdout line.
func digestAfter(t *testing.T, stdout, prefix string) string {
	t.Helper()
	line := strings.TrimSpace(stdout)
	rest, ok := strings.CutPrefix(line, prefix)
	if !ok || len(rest) < 64 || strings.Contains(line, "\n") {
		t.Fatalf("stdout = %q, want prefix %q", stdout, prefix)
	}
	return rest[:64]
}

// notActivated parses "not activated <digest>: activated before (use --rollback); current <current>".
func notActivated(t *testing.T, stdout string) (digest, current string) {
	t.Helper()
	digest = digestAfter(t, stdout, "not activated ")
	want := "not activated " + digest + ": activated before (use --rollback); current "
	rest, ok := strings.CutPrefix(strings.TrimSpace(stdout), want)
	if !ok || len(rest) != 64 {
		t.Fatalf("stdout = %q", stdout)
	}
	return digest, rest
}

func setPrompt(t *testing.T, dir, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "conversation.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestActivateFreshDatabase(t *testing.T) {
	pool := pgtest.Database(t)
	code, stdout, stderr := runActivateWith(pool, "--bundle", bundleFixture(t))
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	digest := activated(t, stdout)
	if got := currentDigest(t, pool); got != digest {
		t.Fatalf("current = %q, printed %q", got, digest)
	}
	files, err := journal.New(pool).DefinitionSet(context.Background(), digest)
	if err != nil || len(files) == 0 {
		t.Fatalf("stored set: %d files, %v", len(files), err)
	}
}

func TestActivateRepeat(t *testing.T) {
	pool := pgtest.Database(t)
	dir := bundleFixture(t)
	code, first, stderr := runActivateWith(pool, "--bundle", dir)
	if code != 0 {
		t.Fatalf("first: %d %q", code, stderr)
	}
	code, second, stderr := runActivateWith(pool, "--bundle", dir)
	if code != 0 {
		t.Fatalf("second: %d %q", code, stderr)
	}
	if activated(t, first) != digestAfter(t, second, "already current ") || currentDigest(t, pool) != activated(t, first) {
		t.Fatalf("digests differ: %q %q", first, second)
	}
}

func TestActivateConcurrent(t *testing.T) {
	pool := pgtest.Database(t)
	dir := bundleFixture(t)
	const workers = 8
	codes := make([]int, workers)
	outs := make([]string, workers)
	errs := make([]string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], outs[i], errs[i] = runActivateWith(pool, "--bundle", dir)
		}()
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 0 {
			t.Fatalf("worker %d: code=%d stderr=%q", i, codes[i], errs[i])
		}
		prefix := "activated "
		if strings.HasPrefix(outs[i], "already current ") {
			prefix = "already current "
		}
		if digestAfter(t, outs[i], prefix) != currentDigest(t, pool) {
			t.Fatalf("worker %d printed %q", i, outs[i])
		}
	}
}

func TestActivateNextCandidate(t *testing.T) {
	pool := pgtest.Database(t)
	dir := bundleFixture(t)
	code, out1, stderr := runActivateWith(pool, "--bundle", dir)
	if code != 0 {
		t.Fatalf("d1: %d %q", code, stderr)
	}
	setPrompt(t, dir, "A different conversation prompt.\n")
	code, out2, stderr := runActivateWith(pool, "--bundle", dir)
	if code != 0 {
		t.Fatalf("d2: %d %q", code, stderr)
	}
	d1, d2 := activated(t, out1), activated(t, out2)
	if d1 == d2 || currentDigest(t, pool) != d2 {
		t.Fatalf("d1=%s d2=%s current=%s", d1, d2, currentDigest(t, pool))
	}
}

func TestActivateReRunEarlierIsNoop(t *testing.T) {
	pool := pgtest.Database(t)
	dir := bundleFixture(t)
	_, out1, _ := runActivateWith(pool, "--bundle", dir)
	d1 := activated(t, out1)
	d1dir := copyTree(t, dir)
	setPrompt(t, dir, "Second prompt.\n")
	code, out2, stderr := runActivateWith(pool, "--bundle", dir)
	if code != 0 {
		t.Fatalf("d2: %d %q", code, stderr)
	}
	d2 := activated(t, out2)
	code, out, stderr := runActivateWith(pool, "--bundle", d1dir)
	if code != 0 || stderr != "" {
		t.Fatalf("re-run d1: code=%d stderr=%q", code, stderr)
	}
	if got, cur := notActivated(t, out); got != d1 || cur != d2 {
		t.Fatalf("stdout = %q", out)
	}
	if currentDigest(t, pool) != d2 {
		t.Fatalf("current moved to %s", currentDigest(t, pool))
	}
	if files, err := journal.New(pool).DefinitionSet(context.Background(), d1); err != nil || len(files) == 0 {
		t.Fatalf("d1 not stored: %d %v", len(files), err)
	}
}

func TestActivateRollback(t *testing.T) {
	pool := pgtest.Database(t)
	dir := bundleFixture(t)
	_, out1, _ := runActivateWith(pool, "--bundle", dir)
	d1 := activated(t, out1)
	d1dir := copyTree(t, dir)
	setPrompt(t, dir, "Second prompt.\n")
	d2dir := copyTree(t, dir)
	_, out2, _ := runActivateWith(pool, "--bundle", dir)
	d2 := activated(t, out2)
	code, out, stderr := runActivateWith(pool, "--rollback", "--bundle", d1dir)
	if code != 0 || stderr != "" || activated(t, out) != d1 || currentDigest(t, pool) != d1 {
		t.Fatalf("rollback: code=%d out=%q stderr=%q current=%s", code, out, stderr, currentDigest(t, pool))
	}
	code, out, stderr = runActivateWith(pool, "--bundle", d2dir)
	if code != 0 || stderr != "" {
		t.Fatalf("re-run d2: code=%d stderr=%q", code, stderr)
	}
	if got, cur := notActivated(t, out); got != d2 || cur != d1 || currentDigest(t, pool) != d1 {
		t.Fatalf("stdout = %q current=%s", out, currentDigest(t, pool))
	}
}

func TestActivateConcurrentOldAndNew(t *testing.T) {
	pool := pgtest.Database(t)
	dir := bundleFixture(t)
	d1dir := copyTree(t, dir)
	_, out1, _ := runActivateWith(pool, "--bundle", dir)
	activated(t, out1)
	setPrompt(t, dir, "Second prompt.\n")
	const workers = 8
	codes := make([]int, workers)
	errs := make([]string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := dir
			if i%2 == 0 {
				target = d1dir
			}
			codes[i], _, errs[i] = runActivateWith(pool, "--bundle", target)
		}()
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 0 {
			t.Fatalf("worker %d: code=%d stderr=%q", i, codes[i], errs[i])
		}
	}
	_, out, _ := runActivateWith(pool, "--bundle", dir)
	if got := digestAfter(t, out, "already current "); got != currentDigest(t, pool) {
		t.Fatalf("current = %s, d2 = %s", currentDigest(t, pool), got)
	}
}

func TestActivateCommentOnlyEdit(t *testing.T) {
	pool := pgtest.Database(t)
	dir := bundleFixture(t)
	path := filepath.Join(dir, "journeys.yaml")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, out1, _ := runActivateWith(pool, "--bundle", dir)
	d1 := activated(t, out1)
	if err := os.WriteFile(path, append([]byte("# a comment only edit\n"), original...), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out2, stderr := runActivateWith(pool, "--bundle", dir)
	if code != 0 || stderr != "" || digestAfter(t, out2, "already current ") != d1 {
		t.Fatalf("code=%d out=%q stderr=%q", code, out2, stderr)
	}
	files, err := journal.New(pool).DefinitionSet(context.Background(), d1)
	if err != nil || !bytes.Equal(files["journeys.yaml"], original) {
		t.Fatalf("stored journeys.yaml changed: %v", err)
	}
}

func TestActivateErrorCategories(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("read bundle: %w: %w", defstore.ErrBundle, errors.New("x")), "bundle"},
		{fmt.Errorf("migrate: %w: %w", defstore.ErrDatabase, errors.New("x")), "database"},
		{fmt.Errorf("%w: set cannot be served", journal.ErrDefinition), "unavailable definition"},
		{errors.New("read bundle: looks like a bundle error but is untyped"), "other"},
	}
	for _, c := range cases {
		if got := activateCategory(c.err); got != c.want {
			t.Errorf("activateCategory(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	pool := pgtest.Database(t)
	empty := t.TempDir()
	if code, _, stderr := runActivateWith(pool, "--bundle", empty); code != 1 || stderr != "agent platform activation failed: bundle\n" {
		t.Errorf("bundle: %d %q", code, stderr)
	}
	if code, _, stderr := runActivateWith(pool); code != 1 || stderr != "agent platform activation failed: usage\n" {
		t.Errorf("usage: %d %q", code, stderr)
	}
	var out, errOut bytes.Buffer
	if code := runActivate(context.Background(), []string{"--bundle", empty}, func(string) string { return "" }, &out, &errOut); code != 1 || errOut.String() != "agent platform activation failed: configuration\n" {
		t.Errorf("configuration: %d %q", code, errOut.String())
	}
	errOut.Reset()
	url := "postgres://user:pw@127.0.0.1:1/none?sslmode=disable"
	if code := runActivate(context.Background(), []string{"--bundle", empty}, func(string) string { return url }, &out, &errOut); code != 1 || errOut.String() != "agent platform activation failed: database\n" {
		t.Errorf("database: %d %q", code, errOut.String())
	}
}

func TestActivateReferencedDigestDoesNotCompile(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.Database(t)
	if err := journal.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := journal.New(pool)
	x := strings.Repeat("ab", 32)
	const sentinel = "GARBAGE-SENTINEL-CONTENT"
	if err := store.PutDefinitionSet(ctx, x, map[string][]byte{"journeys.yaml": []byte(sentinel + ": [")}); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateDefinition(ctx, "", x, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runActivateWith(pool, "--bundle", bundleFixture(t))
	if code == 0 || stdout != "" {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
	if got := currentDigest(t, pool); got != x {
		t.Fatalf("current = %q, want %q", got, x)
	}
	if strings.Contains(stderr, sentinel) || strings.Count(strings.TrimSpace(stderr), "\n") != 0 {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestActivateUsageAndConfig(t *testing.T) {
	pool := pgtest.Database(t)
	code, _, stderr := runActivateWith(pool)
	if code == 0 || stderr != "agent platform activation failed: usage\n" {
		t.Fatalf("missing --bundle: %d %q", code, stderr)
	}
	code, _, stderr = runActivateWith(pool, "--bogus")
	if code == 0 || !strings.HasSuffix(strings.TrimSpace(stderr), "usage") {
		t.Fatalf("bad flag: %d %q", code, stderr)
	}
	var out, errOut bytes.Buffer
	code = runActivate(context.Background(), []string{"--bundle", bundleFixture(t)}, func(string) string { return "" }, &out, &errOut)
	if code == 0 || errOut.String() != "agent platform activation failed: configuration\n" {
		t.Fatalf("missing DATABASE_URL: %d %q", code, errOut.String())
	}
}

func TestActivateErrorsOmitContent(t *testing.T) {
	pool := pgtest.Database(t)
	dir := bundleFixture(t)
	const sentinel = "SECRET-SENTINEL-VALUE"
	if err := os.WriteFile(filepath.Join(dir, "journeys.yaml"), []byte("journeys:\n  - id: "+sentinel+"\n    bogus_key: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runActivateWith(pool, "--bundle", dir)
	if code == 0 || stdout != "" || strings.Contains(stderr, sentinel) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.HasPrefix(stderr, "agent platform activation failed: ") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestActivateDatabaseFailureOmitsURL(t *testing.T) {
	const url = "postgres://user:hunter2@127.0.0.1:1/none?sslmode=disable"
	var out, errOut bytes.Buffer
	code := runActivate(context.Background(), []string{"--bundle", bundleFixture(t)}, func(string) string { return url }, &out, &errOut)
	if code == 0 || strings.Contains(errOut.String(), "hunter2") || !strings.Contains(errOut.String(), "database") {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
}

func TestLoadConfigDoesNotRequireDefinitionPath(t *testing.T) {
	values := validEnvironment()
	if _, ok := values["AGENT_PLATFORM_CONFIG"]; ok {
		t.Fatal("fixture still sets AGENT_PLATFORM_CONFIG")
	}
	if _, err := loadConfig(missingValue(values)); err != nil {
		t.Fatal(err)
	}
}
