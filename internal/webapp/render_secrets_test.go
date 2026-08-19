package webapp

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/secrets"
)

// getAs fetches as a signed-in member: the render route is membership-gated,
// and the badge is for the people who can already read the file.
func getAs(t *testing.T, srv *Server, h http.Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	authAs(t, srv, req)
	return doHTTP(h, req)
}

func renderDoc(t *testing.T, srv *Server, h http.Handler, url string) (struct {
	HTML     string            `json:"html"`
	Findings []secrets.Finding `json:"findings"`
}, *httptest.ResponseRecorder) {
	t.Helper()
	var doc struct {
		HTML     string            `json:"html"`
		Findings []secrets.Finding `json:"findings"`
	}
	rec := getAs(t, srv, h, url)
	if rec.Code != 200 {
		t.Fatalf("render %s: %d %s", url, rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode render body %q: %v", rec.Body, err)
	}
	return doc, rec
}

// TestRenderSecretFindings: the scan that refuses to publish the file also
// runs on the path every file takes, so the viewer can say what the share
// dialog would say (BEA-147).
func TestRenderSecretFindings(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	f.put("dev1", "deploy.md", "# Deploy\n\nexport AWS_ACCESS_KEY_ID="+planted+"\n")
	f.put("dev1", "clean.md", "# Clean\n\nnothing to see\n")
	base := "/api/p/" + p.ID + "/"

	doc, _ := renderDoc(t, srv, h, base+"render?path=deploy.md")
	want := []secrets.Finding{{Rule: "aws_access_key_id", Line: 3}}
	if !reflect.DeepEqual(doc.Findings, want) {
		t.Fatalf("findings = %+v, want %+v", doc.Findings, want)
	}
	// Advisory, not a redaction: the file still renders in full.
	if !strings.Contains(doc.HTML, "Deploy") {
		t.Fatalf("the render was suppressed: %q", doc.HTML)
	}

	// A clean file carries no field at all — omitted, not empty, so the
	// frontend's check is a plain truthiness test.
	doc, rec := renderDoc(t, srv, h, base+"render?path=clean.md")
	if len(doc.Findings) != 0 {
		t.Fatalf("clean file has findings: %+v", doc.Findings)
	}
	if strings.Contains(rec.Body.String(), "findings") {
		t.Fatalf("clean render sent an empty findings field: %s", rec.Body)
	}
}

// TestRenderSecretNeverEchoed mirrors TestShareSecretNeverEchoed for the new
// caller: rule ids and line numbers only, never the matched text.
func TestRenderSecretNeverEchoed(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	f.put("dev1", "creds.md", "key = "+planted+"\n")

	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	rec := getAs(t, srv, h, "/api/p/"+p.ID+"/render?path=creds.md")
	if rec.Code != 200 {
		t.Fatalf("render: %d %s", rec.Code, rec.Body)
	}
	// The rendered HTML is the file, so the key is in the body by design —
	// what must never appear is a SECOND copy carried by the finding.
	var doc struct {
		HTML     string          `json:"html"`
		Findings json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc.Findings), planted) {
		t.Errorf("the finding echoed the secret: %s", doc.Findings)
	}
	if strings.Contains(logs.String(), planted) {
		t.Errorf("the secret reached the log: %s", logs.String())
	}
}

// TestRenderSecretScanLimit: the 1 MiB boundary is a decision, pinned for
// minting in shares_test.go and pinned here so the two surfaces cannot
// disagree about the same file.
func TestRenderSecretScanLimit(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	f.put("dev1", "big.md", strings.Repeat("filler\n", secrets.ScanLimit/7+1)+planted+"\n")
	base := "/api/p/" + p.ID + "/"

	doc, _ := renderDoc(t, srv, h, base+"render?path=big.md")
	if len(doc.Findings) != 0 {
		t.Fatalf("a key past the scan limit was badged: %+v", doc.Findings)
	}
}

// TestRenderVersionSecretFindings: clicking into history on the file the
// badge was warning about must not make the warning disappear.
func TestRenderVersionSecretFindings(t *testing.T) {
	srv, p, _, f, h := shareHub(t)
	f.put("dev1", "deploy.md", "# Deploy\n\nexport AWS_ACCESS_KEY_ID="+planted+"\n")
	f.put("dev1", "deploy.md", "# Deploy\n\ncleaned up\n")
	base := "/api/p/" + p.ID + "/"

	rec := getAs(t, srv, h, base+"history?path=deploy.md")
	var hist struct {
		Entries []HistoryEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist.Entries) < 2 {
		t.Fatalf("history has %d entries, want the pre-cleanup version too", len(hist.Entries))
	}
	old := hist.Entries[len(hist.Entries)-1].Blob

	// Current version is clean...
	doc, _ := renderDoc(t, srv, h, base+"render?path=deploy.md")
	if len(doc.Findings) != 0 {
		t.Fatalf("cleaned-up file has findings: %+v", doc.Findings)
	}
	// ...the version that held the key still says so.
	doc, _ = renderDoc(t, srv, h, base+"render?path=deploy.md&sha="+old)
	want := []secrets.Finding{{Rule: "aws_access_key_id", Line: 3}}
	if !reflect.DeepEqual(doc.Findings, want) {
		t.Fatalf("version findings = %+v, want %+v", doc.Findings, want)
	}
}
