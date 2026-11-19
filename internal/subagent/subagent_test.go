package subagent

import (
	"errors"
	"testing"
	"time"
)

const (
	tTypeExplore = "explore"
	tTypeGP      = "general-purpose"

	nameAuto1 = tTypeExplore + "-1"
	nameAuto2 = tTypeExplore + "-2"
	nameAudit = "audit"
)

func TestValidName(t *testing.T) {
	valid := []string{"auditor", nameAuto1, "a", "x9-y"}
	invalid := []string{"", "Auditor", "UPPER", "has space", "has_underscore", "a/b", "-lead", "main", "owner"}

	for _, n := range valid {
		if !ValidName(n) {
			t.Errorf("expected %q valid", n)
		}
	}

	for _, n := range invalid {
		if ValidName(n) {
			t.Errorf("expected %q invalid", n)
		}
	}
}

func TestDispatchSeq(t *testing.T) {
	if got := dispatchSeq("abc", 3); got != "abc-3" {
		t.Errorf("got %q", got)
	}
}

func TestRegistrySpawnCaps(t *testing.T) {
	reg := NewRegistry(Limits{MaxDepth: 1, MaxConcurrent: 2, MailboxBuffer: 4}, testLogger())
	owner := testOwner("s1", "cli")

	// Depth cap: a depth-2 spawn with max_depth 1 fails.
	if _, err := reg.Spawn(owner, SpawnOpts{Type: tTypeGP, Depth: 2}); !errors.Is(err, ErrDepthLimit) {
		t.Fatalf("expected ErrDepthLimit, got %v", err)
	}

	// Concurrency cap: the third spawn fails (max_concurrent 2).
	for i := 0; i < 2; i++ {
		if _, err := reg.Spawn(owner, SpawnOpts{Type: tTypeGP, Depth: 1}); err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
	}

	if _, err := reg.Spawn(owner, SpawnOpts{Type: tTypeGP, Depth: 1}); !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("expected ErrConcurrencyLimit, got %v", err)
	}

	// A different owner is unaffected (cap is per owner).
	owner2 := testOwner("s2", "telegram-1")
	if _, err := reg.Spawn(owner2, SpawnOpts{Type: tTypeGP, Depth: 1}); err != nil {
		t.Fatalf("owner2 spawn should succeed: %v", err)
	}

	// Releasing a slot re-enables spawning.
	st, _ := reg.Lookup("s1", tTypeGP+"-1")
	reg.ReleaseSlot("s1", st)
	if reg.RunningCount("s1") != 1 {
		t.Fatalf("running count after release = %d", reg.RunningCount("s1"))
	}

	if _, err := reg.Spawn(owner, SpawnOpts{Type: tTypeGP, Depth: 1}); err != nil {
		t.Fatalf("spawn after release: %v", err)
	}
}

func TestRegistryNames(t *testing.T) {
	reg := NewRegistry(Limits{}, testLogger())
	owner := testOwner("s1", "cli")

	st, err := reg.Spawn(owner, SpawnOpts{Type: tTypeExplore, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}

	if st.Name != nameAuto1 {
		t.Fatalf("auto name = %q", st.Name)
	}

	// Re-spawn of an existing name is a conflict.
	if _, err := reg.Spawn(owner, SpawnOpts{Type: tTypeExplore, Name: nameAuto1, Depth: 1}); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("expected ErrNameConflict, got %v", err)
	}

	// Reserved names refuse.
	for _, n := range []string{"main", "owner"} {
		_, spawnErr := reg.Spawn(owner, SpawnOpts{Type: tTypeExplore, Name: n, Depth: 1})
		if !errors.Is(spawnErr, ErrNameConflict) {
			t.Fatalf("reserved %q: expected ErrNameConflict, got %v", n, spawnErr)
		}
	}

	// Second auto name increments.
	st2, spawnErr := reg.Spawn(owner, SpawnOpts{Type: tTypeExplore, Depth: 1})
	if spawnErr != nil {
		t.Fatal(spawnErr)
	}

	if st2.Name != nameAuto2 {
		t.Fatalf("second auto name = %q", st2.Name)
	}
}

func TestRegistryLookupAndViews(t *testing.T) {
	reg := NewRegistry(Limits{}, testLogger())
	owner := testOwner("s1", "cli")

	st, err := reg.Spawn(owner, SpawnOpts{Type: tTypeExplore, Name: nameAudit, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}

	byName, err := reg.Lookup("s1", nameAudit)
	if err != nil || byName.ID != st.ID {
		t.Fatalf("lookup by name: %v", err)
	}

	byID, err := reg.Lookup("s1", st.ID)
	if err != nil || byID.Name != nameAudit {
		t.Fatalf("lookup by id: %v", err)
	}

	if _, err := reg.Lookup("s1", "missing"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("expected ErrUnknownAgent, got %v", err)
	}

	views := reg.Agents("s1")
	if len(views) != 1 || views[0].Name != nameAudit || views[0].QueuedMail != 0 {
		t.Fatalf("views = %+v", views)
	}
}

func TestRegistryMailboxBackpressure(t *testing.T) {
	reg := NewRegistry(Limits{MailboxBuffer: 2}, testLogger())
	owner := testOwner("s1", "cli")

	st, err := reg.Spawn(owner, SpawnOpts{Type: tTypeExplore, Depth: 1})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		pushErr := reg.QueueMail(st, Mail{Text: "m", Scanned: "m"})
		if pushErr != nil {
			t.Fatalf("mail %d: %v", i, pushErr)
		}
	}

	err = reg.QueueMail(st, Mail{Text: "m3", Scanned: "m3"})
	if !errors.Is(err, ErrMailboxFull) {
		t.Fatalf("expected ErrMailboxFull, got %v", err)
	}

	// Nothing was enqueued by the failed push.
	if st.Mail.Len() != 2 {
		t.Fatalf("mailbox len = %d", st.Mail.Len())
	}
}

func TestRegistryPendingHandbacks(t *testing.T) {
	reg := NewRegistry(Limits{}, testLogger())
	owner := testOwner("s1", "cli")

	if _, err := reg.Spawn(owner, SpawnOpts{Type: tTypeExplore, Depth: 1}); err != nil {
		t.Fatal(err)
	}

	if reg.PendingCount("s1") != 0 {
		t.Fatal("expected empty pending set")
	}

	reg.Pending("s1", Handback{HandbackID: "hb-1", CreatedAt: time.Now()})
	reg.Pending("s1", Handback{HandbackID: "hb-2", CreatedAt: time.Now()})

	list := reg.PendingList("s1")
	if len(list) != 2 || list[0].HandbackID != "hb-1" || list[1].HandbackID != "hb-2" {
		t.Fatalf("pending order broken: %+v", list)
	}

	if !reg.RemovePending("s1", "hb-1") {
		t.Fatal("remove hb-1 failed")
	}

	if reg.RemovePending("s1", "hb-1") {
		t.Fatal("hb-1 removed twice")
	}

	if got := reg.PendingCount("s1"); got != 1 {
		t.Fatalf("pending count = %d", got)
	}

	// Unknown owner: all operations degrade gracefully.
	if reg.PendingCount("nope") != 0 || reg.PendingList("nope") != nil {
		t.Fatal("unknown owner should read as empty")
	}
}

func TestRegistryJobsCap(t *testing.T) {
	reg := NewRegistry(Limits{MaxConcurrentJobs: 1}, testLogger())

	if err := reg.BeginJob(); err != nil {
		t.Fatal(err)
	}

	if err := reg.BeginJob(); !errors.Is(err, ErrJobsCap) {
		t.Fatalf("expected ErrJobsCap, got %v", err)
	}

	reg.EndJob()

	if err := reg.BeginJob(); err != nil {
		t.Fatalf("after EndJob: %v", err)
	}

	reg.EndJob()
	reg.EndJob() // idempotent
}

func TestMailboxFIFO(t *testing.T) {
	mb := newMailbox(3)

	for _, txt := range []string{"a", "b", "c"} {
		if err := mb.Push(Mail{Text: txt, Scanned: txt}); err != nil {
			t.Fatal(err)
		}
	}

	if err := mb.Push(Mail{Text: "d"}); !errors.Is(err, ErrMailboxFull) {
		t.Fatal("expected full")
	}

	for _, want := range []string{"a", "b", "c"} {
		m, err := mb.Pop()
		if err != nil || m.Text != want {
			t.Fatalf("pop = %q, %v", m.Text, err)
		}

		if m.QueuedAt.IsZero() {
			t.Fatal("QueuedAt not stamped")
		}
	}

	if _, err := mb.Pop(); !errors.Is(err, ErrMailboxEmpty) {
		t.Fatal("expected empty")
	}
}

// Shared test identifiers (the runtime tests use them too).
const (
	tHB1     = "hb-1"
	tJobName = "curator"
	tJobTask = "curate"
)
