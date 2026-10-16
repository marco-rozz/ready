package oauth

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/marco-rozz/ready/internal/config"
)

func TestFileStore_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := NewCredential(testServerKey)
	cred.ClientID = "abc"
	cred.AccessToken = "tok"
	cred.UpdatedAt = time.Now()

	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load(ctx, testServerKey)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got == nil || got.ClientID != "abc" || got.Version != credentialVersion {
		t.Fatalf("Load returned %+v", got)
	}
}

func TestFileStore_LoadAbsent(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	got, err := store.Load(ctx, "nope")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for absent record, got %+v", got)
	}
}

func TestFileStore_Delete(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := NewCredential(testServerKey)
	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err = store.Delete(ctx, testServerKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := store.Load(ctx, testServerKey)
	if err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil after delete, got %+v", got)
	}
}

func TestFileStore_DeleteAbsentIsNotError(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	if err = store.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("Delete of absent record should not error, got: %v", err)
	}
}

func TestFileStore_AtomicReplacementUnderConcurrentReader(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(ctx, dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := NewCredential(testServerKey)
	cred.AccessToken = "v0"
	if err := store.Save(ctx, cred); err != nil {
		t.Fatalf("initial Save: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}

			got, loadErr := store.Load(ctx, testServerKey)
			if loadErr != nil {
				t.Errorf("concurrent Load: %v", loadErr)

				return
			}
			if got == nil || got.AccessToken == "" {
				t.Errorf("concurrent Load returned incomplete record: %+v", got)

				return
			}
		}
	}()

	for i := range 50 {
		cred.AccessToken = "v" + string(rune('0'+i%10))
		if err := store.Save(ctx, cred); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}

	close(stop)
	wg.Wait()
}

func TestFileStore_PermissionBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits not applicable on windows")
	}

	ctx := context.Background()
	dir := t.TempDir()
	subdir := filepath.Join(dir, "oauth")

	store, err := NewFileStore(ctx, subdir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	dirInfo, err := os.Stat(subdir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %o, want 0700", perm)
	}

	cred := NewCredential(testServerKey)
	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fileInfo, err := os.Stat(filepath.Join(subdir, "srv.json"))
	if err != nil {
		t.Fatalf("Stat file: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}
}

func TestFileStore_PathTraversalInServerKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(ctx, dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	cred := NewCredential("../../etc/passwd")
	if err = store.Save(ctx, cred); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("unexpected directory entry %q created by traversal server key", e.Name())
		}
	}

	// Confirm nothing escaped dir: the parent of dir must not contain a
	// newly created "etc" directory.
	if _, err := os.Stat(filepath.Join(dir, "..", "..", "etc", "passwd")); err == nil {
		t.Fatal("path traversal server key escaped the store directory")
	}
}

func TestNewStore_UnsupportedTokenStore(t *testing.T) {
	_, err := NewStore(context.Background(), config.OAuthConfig{TokenStore: "keychain", Path: t.TempDir()})
	if err == nil {
		t.Fatal("expected error for unsupported token_store")
	}
}

func TestNewStore_File(t *testing.T) {
	s, err := NewStore(context.Background(), config.OAuthConfig{TokenStore: "file", Path: t.TempDir()})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, ok := s.(*FileStore); !ok {
		t.Fatalf("expected *FileStore, got %T", s)
	}
}
