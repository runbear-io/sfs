package webapp

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/runbear-io/beardrive/internal/remote"
)

// History folded every journal of a project on EVERY request — 8-10s on a real
// project, and paging made it worse rather than better, since each "load more"
// re-downloaded and re-parsed the lot (BEA-85). Journals only grow, so the
// (Size, Modified) that List already reports proves a parse is still current.
//
// These tests pin the cache from the outside: what matters is not that a map
// exists but that a warm request touches no journal bytes, and that the feed it
// produces is the one the uncached code produced.

// countBackend counts the reads, writes and lists the hub makes through it,
// per key.
type countBackend struct {
	remote.Backend
	mu    sync.Mutex
	gets  map[string]int
	puts  map[string]int
	lists int
}

func newCountBackend(be remote.Backend) *countBackend {
	return &countBackend{Backend: be, gets: map[string]int{}, puts: map[string]int{}}
}

func (b *countBackend) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	b.mu.Lock()
	b.puts[key]++
	b.mu.Unlock()
	return b.Backend.Put(ctx, key, r, size)
}

// putsTo is how many times one key has been written.
func (b *countBackend) putsTo(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.puts[key]
}

func (b *countBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	b.gets[key]++
	b.mu.Unlock()
	return b.Backend.Get(ctx, key)
}

func (b *countBackend) List(ctx context.Context, prefix string) ([]remote.Object, error) {
	b.mu.Lock()
	b.lists++
	b.mu.Unlock()
	return b.Backend.List(ctx, prefix)
}

// journalGets is how many times a journal — any journal — has been fetched.
func (b *countBackend) journalGets() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for k, c := range b.gets {
		if strings.Contains(k, "journal/") {
			n += c
		}
	}
	return n
}

func (b *countBackend) getsFor(dev string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for k, c := range b.gets {
		if strings.HasSuffix(k, "journal/"+dev+".jsonl") {
			n += c
		}
	}
	return n
}

func (b *countBackend) listCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lists
}

// cachedHub is a hub over a counting backend with a seeded three-device
// project, plus the handler and the base URL for its history route.
func cachedHub(t *testing.T) (*Server, *countBackend, *fakeRemote, http.Handler, string) {
	t.Helper()
	var cb *countBackend
	srv, p, root := newHub(t, false, func(be remote.Backend) remote.Backend {
		cb = newCountBackend(be)
		return cb
	})
	f := newFakeRemoteAt(t, filepath.Join(root, p.ID))
	f.putAs("dev1", "alice@x.io", "Alice", "notes/plan.md", "v1")
	f.putAs("dev1", "alice@x.io", "Alice", "notes/plan.md", "v2 longer")
	f.putAs("dev2", "bob@x.io", "Bob", "notes/other.md", "bob's file")
	f.del("dev2", "notes/other.md")
	f.putAs("dev3", "carol@x.io", "Carol", "readme.md", "top")
	return srv, cb, f, srv.Handler(), "/api/p/" + p.ID + "/"
}

func TestHistoryReusesParsedJournals(t *testing.T) {
	_, cb, _, h, base := cachedHub(t)

	if rec := do(t, h, "GET", base+"history", nil); rec.Code != 200 {
		t.Fatalf("cold history: %d %s", rec.Code, rec.Body)
	}
	cold, lists := cb.journalGets(), cb.listCount()
	if cold < 3 {
		t.Fatalf("cold load fetched %d journals, want at least 3", cold)
	}

	for i := 0; i < 3; i++ {
		if rec := do(t, h, "GET", base+"history", nil); rec.Code != 200 {
			t.Fatalf("warm history: %d %s", rec.Code, rec.Body)
		}
	}
	if got := cb.journalGets(); got != cold {
		t.Fatalf("warm requests fetched %d journals, want 0 beyond the cold %d", got-cold, cold)
	}
	// The List is the round trip a warm request still pays: it is what proves
	// nothing changed, so it must NOT be cached away.
	if cb.listCount() <= lists {
		t.Fatalf("list count %d did not advance past %d", cb.listCount(), lists)
	}
}

func TestJournalCacheInvalidatesOnePush(t *testing.T) {
	_, cb, f, h, base := cachedHub(t)
	do(t, h, "GET", base+"history", nil)
	before1, before2, before3 := cb.getsFor("dev1"), cb.getsFor("dev2"), cb.getsFor("dev3")

	f.putAs("dev2", "bob@x.io", "Bob", "notes/other.md", "bob is back")
	if rec := do(t, h, "GET", base+"history", nil); rec.Code != 200 {
		t.Fatalf("history after push: %d %s", rec.Code, rec.Body)
	}
	if got := cb.getsFor("dev2"); got != before2+1 {
		t.Fatalf("dev2 fetched %d times, want %d", got, before2+1)
	}
	if cb.getsFor("dev1") != before1 || cb.getsFor("dev3") != before3 {
		t.Fatalf("untouched journals re-fetched: dev1 %d→%d, dev3 %d→%d",
			before1, cb.getsFor("dev1"), before3, cb.getsFor("dev3"))
	}
}

func TestJournalCachePagingRefetchesNothing(t *testing.T) {
	_, cb, _, h, base := cachedHub(t)

	rec := do(t, h, "GET", base+"history?n=2", nil)
	if rec.Code != 200 {
		t.Fatalf("page 1: %d %s", rec.Code, rec.Body)
	}
	var page struct {
		Entries []HistoryEntry `json:"entries"`
		Next    string         `json:"next_cursor"`
	}
	mustJSON(t, rec, &page)
	if page.Next == "" {
		t.Fatalf("no cursor to page with: %s", rec.Body)
	}
	after := cb.journalGets()

	for cursor := page.Next; cursor != ""; cursor = page.Next {
		page.Next = ""
		mustJSON(t, do(t, h, "GET", base+"history?n=2&cursor="+url.QueryEscape(cursor), nil), &page)
	}
	if got := cb.journalGets(); got != after {
		t.Fatalf("paging re-fetched %d journals", got-after)
	}
}

// The criterion that matters most: the cache must be invisible in the output.
func TestHistoryOutputUnchangedByCache(t *testing.T) {
	srv, _, _, h, base := cachedHub(t)
	urls := []string{
		"history", "history?path=notes/plan.md", "history?prefix=notes",
		"history?n=2", "history?user=alice@x.io",
	}
	warm := make([]string, len(urls))
	for i, u := range urls {
		do(t, h, "GET", base+u, nil) // cold
		warm[i] = do(t, h, "GET", base+u, nil).Body.String()
	}

	// Drop everything the cache holds; the next request is the uncached code
	// path, byte for byte.
	_, v, err := srv.projectVolume(strings.TrimSuffix(strings.TrimPrefix(base, "/api/p/"), "/"))
	if err != nil {
		t.Fatal(err)
	}
	rs := v.source.(*RemoteSource)
	rs.jmu.Lock()
	rs.jcache, rs.jbytes = nil, 0
	rs.jmu.Unlock()

	for i, u := range urls {
		if got := do(t, h, "GET", base+u, nil).Body.String(); got != warm[i] {
			t.Fatalf("%s differs with and without the cache:\ncached: %s\nfresh:  %s", u, warm[i], got)
		}
	}
}

// A journal the remote no longer has must not stay resident, or a hub leaks a
// map entry per device that ever synced anything.
func TestJournalCacheDropsVanishedJournals(t *testing.T) {
	srv, _, f, h, base := cachedHub(t)
	do(t, h, "GET", base+"history", nil)

	_, v, err := srv.projectVolume(strings.TrimSuffix(strings.TrimPrefix(base, "/api/p/"), "/"))
	if err != nil {
		t.Fatal(err)
	}
	rs := v.source.(*RemoteSource)
	if err := os.Remove(filepath.Join(f.dir, "journal", "dev3.jsonl")); err != nil {
		t.Fatal(err)
	}

	do(t, h, "GET", base+"history", nil)
	rs.jmu.Lock()
	defer rs.jmu.Unlock()
	if _, ok := rs.jcache["journal/dev3.jsonl"]; ok {
		t.Fatalf("cache kept a journal the remote no longer has: %v", rs.jcache)
	}
	// The running total is what the ceiling is enforced against, so a prune or
	// a re-fetch that forgets to subtract would quietly disable the cache.
	var sum int64
	for _, c := range rs.jcache {
		sum += c.bytes
	}
	if rs.jbytes != sum {
		t.Fatalf("byte total %d, cached entries sum to %d", rs.jbytes, sum)
	}
}
