package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// Export/import move a whole project between hubs — the "you can always
// leave" story. The archive is simply the remote store layout
// (journal/<device>.jsonl + blobs/<sha256>) in a tar.gz, plus a manifest, so
// an export is a full-fidelity copy of the project: every device's history,
// authorship, and every retained blob. Import replays it verbatim into a
// fresh project on whichever hub the device is logged into; original devices
// that later connect to the imported project resume exactly where they were,
// because their journals are byte-identical.

const manifestName = "beardrive-export.json"

var (
	blobKeyRe    = regexp.MustCompile(`^blobs/[0-9a-f]{64}$`)
	journalKeyRe = regexp.MustCompile(`^journal/[A-Za-z0-9._-]+\.jsonl$`)
	// Delta-sync key classes (docs/delta-sync-prd.md). A chunk key is its own
	// content hash, checked on import exactly like a blob; a manifest key is
	// the whole file's sha, verifiable only by reassembly, so it is carried
	// verbatim like a journal.
	chunkKeyRe    = regexp.MustCompile(`^chunks/[0-9a-f]{64}$`)
	manifestKeyRe = regexp.MustCompile(`^manifests/[0-9a-f]{64}$`)
)

type exportManifest struct {
	Project    string    `json:"project"`
	Remote     string    `json:"remote"`
	ExportedAt time.Time `json:"exported_at"`
}

func exportCmd() *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:   "export [folder]",
		Short: "Export this project — full history, every device — to an archive",
		Long: `Export the project's complete store (all devices' journals and all content
blobs, i.e. full history and authorship) from its hub into a portable
tar.gz. Import it into any other hub — self-hosted or cloud — with
bdrive import.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder, err := absFolder(args)
			if err != nil {
				return err
			}
			sess, proj, err := openSession(cmd.Context(), folder, true)
			if err != nil {
				return err
			}
			defer closeSession(sess)
			if sess.Backend == nil {
				return fmt.Errorf("the project's hub is unreachable — export copies from the hub, not this folder")
			}
			if st, err := sess.Store.LoadSync(); err == nil {
				if ops, err := sess.Store.DeviceOps(sess.Device.ID); err == nil && int64(len(ops)) > st.PushedOps {
					fmt.Fprintf(os.Stderr, "warning: %d local change(s) not pushed yet — run `bdrive sync` first for a complete export\n", int64(len(ops))-st.PushedOps)
				}
			}
			if out == "" {
				// proj.Volume is read verbatim from .bdrive/config.json, and
				// init writes it from the hub's project name — any org
				// member's string. It reaches os.Create, so a name like
				// "../../pwned" chose where every teammate's export landed and
				// truncated whatever was there. A default destination is a
				// FILE NAME in the working directory; -o is how a human asks
				// for anywhere else.
				out = exportFileName(proj.Volume, time.Now())
			}
			f, err := os.Create(out)
			if err != nil {
				return err
			}
			defer f.Close()
			man := exportManifest{Project: proj.Volume, Remote: proj.Remote, ExportedAt: time.Now().UTC()}
			blobs, journals, size, err := exportStore(cmd.Context(), sess.Backend, f, man)
			if err != nil {
				os.Remove(out)
				return err
			}
			fmt.Printf("exported %q: %d journal(s), %d blob(s), %s → %s\n", proj.Volume, journals, blobs, humanBytes(size), out)
			fmt.Println("import it elsewhere with: bdrive login <other-server> && bdrive import " + filepath.Base(out))
			return nil
		},
	}
	c.Flags().StringVarP(&out, "output", "o", "", "archive path (default <project>-export-<date>.tar.gz)")
	return c
}

func importCmd() *cobra.Command {
	var name string
	var allowIncomplete bool
	c := &cobra.Command{
		Use:   "import <archive.tar.gz>",
		Short: "Import an exported project into the hub you're logged into",
		Long: `Import a bdrive export archive as a new project on the current hub (the one
from bdrive login), preserving full history and authorship. The target
project must be empty. Afterwards, connect folders to it with
bdrive init --project <id>.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			settings, _, err := ensureLogin("") // no --server here: nothing to strand
			if err != nil {
				return err
			}
			gz, err := gzip.NewReader(f)
			if err != nil {
				return fmt.Errorf("not a bdrive export archive: %w", err)
			}
			tr := tar.NewReader(gz)
			man, first, err := readManifest(tr)
			if err != nil {
				return err
			}
			if name == "" {
				name = man.Project
			}
			if name == "" {
				return fmt.Errorf("archive has no manifest; pass --name")
			}
			p, created, err := createProject(settings.Server, settings.Token, name, "")
			if err != nil {
				return fmt.Errorf("cannot create project on %s: %w", settings.Server, err)
			}
			// The name may come from inside the archive, and POST /api/projects
			// is create-or-JOIN-by-name — so a hostile archive could pick which
			// of the importer's existing projects it landed in, and the "must be
			// empty" check below happily passed for a project created in the UI
			// and never synced. A manifest may PROPOSE a name; only the user may
			// select an existing project.
			if !created {
				return fmt.Errorf("a project named %q already exists on %s — import only ever creates a new "+
					"project; pass --name <fresh name> (the archive proposed this one)", name, settings.Server)
			}
			be, err := remote.Open(cmd.Context(), settings.Server+"/p/"+p.ID)
			if err != nil {
				return err
			}
			defer be.Close()
			if existing, err := be.List(cmd.Context(), "journal/"); err != nil {
				return fmt.Errorf("cannot read target project: %w", err)
			} else if len(existing) > 0 {
				return fmt.Errorf("project %q (%s) on %s already has content — import needs an empty project (pass --name to create a fresh one)", p.Name, p.ID, settings.Server)
			}
			blobs, journals, size, err := importStore(cmd.Context(), be, tr, first, allowIncomplete)
			if err != nil {
				return err
			}
			verb := "created"
			if !created {
				verb = "joined"
			}
			fmt.Printf("imported into %q (%s, %s on %s): %d journal(s), %d blob(s), %s\n",
				p.Name, p.ID, verb, settings.Server, journals, blobs, humanBytes(size))
			fmt.Printf("\nconnect a folder to it:  bdrive init --project %s\n", p.ID)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "project name on the target hub (default: name from the archive)")
	c.Flags().BoolVar(&allowIncomplete, "allow-incomplete", false,
		"import even when the journal references content the archive does not hold (missing files are listed and stay missing)")
	c.Flags().Int64Var(&maxImportBlob, "max-blob", maxImportBlob, "largest single file (bytes) an archive member may spool to disk")
	return c
}

// exportStore streams every journal and blob from the backend into a tar.gz,
// manifest first.
func exportStore(ctx context.Context, be remote.Backend, w io.Writer, man exportManifest) (blobs, journals int, size int64, err error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return 0, 0, 0, err
	}
	if err := writeTarFile(tw, manifestName, mb); err != nil {
		return 0, 0, 0, err
	}
	for _, prefix := range []string{"journal/", "blobs/", "chunks/", "manifests/"} {
		objs, err := be.List(ctx, prefix)
		if err != nil {
			return blobs, journals, size, fmt.Errorf("list %s: %w", prefix, err)
		}
		for _, o := range objs {
			// The hub named these keys and they become tar member names in a
			// file the export's own advice tells the user to pass around.
			// `bdrive import` refuses a member outside the store layout;
			// `tar xzf` does not. Same allowlist, applied on the way out.
			if !journalKeyRe.MatchString(o.Key) && !blobKeyRe.MatchString(o.Key) &&
				!chunkKeyRe.MatchString(o.Key) && !manifestKeyRe.MatchString(o.Key) {
				continue
			}
			rc, err := be.Get(ctx, o.Key)
			if err != nil {
				return blobs, journals, size, fmt.Errorf("get %s: %w", o.Key, err)
			}
			hdr := &tar.Header{Name: o.Key, Mode: 0o644, Size: o.Size, ModTime: man.ExportedAt}
			if err := tw.WriteHeader(hdr); err != nil {
				rc.Close()
				return blobs, journals, size, err
			}
			n, err := io.Copy(tw, rc)
			rc.Close()
			if err != nil {
				return blobs, journals, size, fmt.Errorf("copy %s: %w", o.Key, err)
			}
			size += n
			if prefix == "journal/" {
				journals++
			} else {
				blobs++
			}
		}
	}
	if err := tw.Close(); err != nil {
		return blobs, journals, size, err
	}
	return blobs, journals, size, gz.Close()
}

// readManifest reads the archive's first entry. If it is the manifest it is
// consumed and (manifest, nil) returns; otherwise the header is handed back
// as first so importStore starts with it.
func readManifest(tr *tar.Reader) (exportManifest, *tar.Header, error) {
	var man exportManifest
	hdr, err := tr.Next()
	if err == io.EOF {
		return man, nil, fmt.Errorf("archive is empty")
	}
	if err != nil {
		return man, nil, fmt.Errorf("not a bdrive export archive: %w", err)
	}
	if hdr.Name != manifestName {
		return man, hdr, nil
	}
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&man); err != nil {
		return man, nil, fmt.Errorf("bad manifest: %w", err)
	}
	return man, nil, nil
}

// importStore uploads every archive entry to the backend, verifying blob
// content against its content-addressed key. first, when non-nil, is an
// already-read header to process before advancing the reader.
func importStore(ctx context.Context, be remote.Backend, tr *tar.Reader, first *tar.Header, allowIncomplete bool) (blobs, journals int, size int64, err error) {
	// Every blob a journal references must resolve to content in this same
	// archive — blobs/<sha> or manifests/<sha>. A pre-delta `bdrive export`
	// run against a delta-sync hub enumerates only journal/ and blobs/, so
	// its archive silently omits every chunked large file while looking
	// complete; importing it succeeded and the files were just gone on the
	// destination. Import is the anti-lock-in door — it refuses loudly
	// instead.
	content := map[string]bool{}       // sha → present as blobs/ or manifests/
	referenced := map[string]string{}  // sha → a path that references it
	chunks := map[string]bool{}        // chunk sha → present in the archive
	manChunks := map[string][]string{} // manifest sha → chunk shas it names
	hdr := first
	for {
		if hdr == nil {
			hdr, err = tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return blobs, journals, size, err
			}
		}
		key := hdr.Name
		switch {
		case key == manifestName || hdr.Typeflag == tar.TypeDir:
			// skip
		case journalKeyRe.MatchString(key):
			// Spooled rather than streamed so the ops can be read: the
			// archive-completeness check below needs every Op.Blob.
			jtmp, n, _, err := spoolBlob(tr)
			if err != nil {
				return blobs, journals, size, err
			}
			if ops, err := readJournalOps(jtmp); err == nil {
				for _, op := range ops {
					if op.Kind == journal.KindPut && op.Blob != "" {
						referenced[op.Blob] = op.Path
					}
				}
			}
			err = be.Put(ctx, key, jtmp, n)
			jtmp.Close()
			os.Remove(jtmp.Name())
			if err != nil {
				return blobs, journals, size, fmt.Errorf("put %s: %w", key, err)
			}
			journals++
			size += hdr.Size
		case manifestKeyRe.MatchString(key):
			// A manifest's key is the whole file's sha — only reassembly can
			// verify the CONTENT, so it travels verbatim — but the chunks it
			// names must be in this same archive, checked after the loop
			// (tar member order is not ours to assume).
			mtmp, n, _, err := spoolBlob(tr)
			if err != nil {
				return blobs, journals, size, err
			}
			var man struct {
				Chunks []struct {
					H string `json:"h"`
				} `json:"chunks"`
			}
			sha := strings.TrimPrefix(key, "manifests/")
			if derr := json.NewDecoder(mtmp).Decode(&man); derr == nil {
				for _, c := range man.Chunks {
					manChunks[sha] = append(manChunks[sha], c.H)
				}
			}
			if _, err := mtmp.Seek(0, io.SeekStart); err != nil {
				mtmp.Close()
				os.Remove(mtmp.Name())
				return blobs, journals, size, err
			}
			err = be.Put(ctx, key, mtmp, n)
			mtmp.Close()
			os.Remove(mtmp.Name())
			if err != nil {
				return blobs, journals, size, fmt.Errorf("put %s: %w", key, err)
			}
			content[sha] = true
			blobs++
			size += hdr.Size
		case blobKeyRe.MatchString(key), chunkKeyRe.MatchString(key):
			// Spool first, store second: hashing while streaming into Put
			// notices the mismatch only after the object is already in the
			// target store, under a content address promising different
			// content — next to the journals that reference it, which are
			// written first. Every device that later connects then fails its
			// pull with "blob corrupt on remote" and never recovers.
			tmp, n, got, err := spoolBlob(tr)
			if err != nil {
				return blobs, journals, size, err
			}
			if got != key[strings.IndexByte(key, '/')+1:] {
				tmp.Close()
				os.Remove(tmp.Name())
				return blobs, journals, size, fmt.Errorf("corrupt archive: %s has content hash %s", key, got)
			}
			err = be.Put(ctx, key, tmp, n)
			tmp.Close()
			os.Remove(tmp.Name())
			if err != nil {
				return blobs, journals, size, fmt.Errorf("put %s: %w", key, err)
			}
			if strings.HasPrefix(key, "blobs/") {
				content[strings.TrimPrefix(key, "blobs/")] = true
			} else {
				chunks[strings.TrimPrefix(key, "chunks/")] = true
			}
			blobs++
			size += hdr.Size
		default:
			return blobs, journals, size, fmt.Errorf("unexpected entry %q in archive (not a bdrive export?)", key)
		}
		hdr = nil
	}
	if journals == 0 {
		return blobs, journals, size, fmt.Errorf("archive contains no journals — nothing to import")
	}
	// A manifest in the archive must bring its chunks along — a manifest
	// whose chunks are absent is exactly as incomplete as a missing blob,
	// just one indirection deeper.
	for sha, named := range manChunks {
		for _, h := range named {
			if !chunks[h] {
				if allowIncomplete {
					fmt.Fprintf(os.Stderr, "warning: manifest %.12s… names chunk %.12s… the archive does not hold\n", sha, h)
					continue
				}
				return blobs, journals, size, fmt.Errorf(
					"incomplete archive: manifest %.12s… names chunk %.12s… but the archive holds no content for it — "+
						"re-export with a current bdrive, or pass --allow-incomplete (missing files stay missing)", sha, h)
			}
		}
	}
	for sha, path := range referenced {
		if !content[sha] {
			if allowIncomplete {
				fmt.Fprintf(os.Stderr, "warning: archive holds no content for %q (blob %.12s…) — imported without it\n", path, sha)
				continue
			}
			// One dangling reference — an old export against a newer hub, or
			// one forged journal line — must not permanently close the
			// anti-lock-in door: the refusal names the escape hatch.
			return blobs, journals, size, fmt.Errorf(
				"incomplete archive: the journal names %q (blob %.12s…) but the archive holds no content for it — "+
					"if this was exported by an older bdrive against a newer hub, re-export with a current bdrive, "+
					"or pass --allow-incomplete to import anyway (missing files stay missing)", path, sha)
		}
	}
	return blobs, journals, size, nil
}

// readJournalOps parses the ops in a spooled journal body the way every
// device does (journal.Parse), leaving the file rewound for the store.
func readJournalOps(f *os.File) ([]journal.Op, error) {
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	ops, _ := journal.Parse(b)
	return ops, nil
}

// maxImportBlob bounds what a single archive member may write to local disk.
// Generous for real projects and far below what a compression bomb wants;
// --max-blob raises it, so an honest export of a very large file is never
// unimportable (this archive is the product's anti-lock-in path).
var maxImportBlob int64 = 256 << 20

// spoolBlob copies one archive member to a temp file, returning it rewound
// with its size and sha256 — so the caller can decide whether the bytes belong
// under their key BEFORE anything is stored. The caller closes and removes it.
func spoolBlob(r io.Reader) (*os.File, int64, string, error) {
	tmp, err := os.CreateTemp("", "bdrive-import-*")
	if err != nil {
		return nil, 0, "", err
	}
	h := sha256.New()
	// Bounded: the member's declared size is the archive author's number too,
	// and the archive is a gzip stream, so a small file that looks exactly
	// like a bdrive export can spool a thousand times its own size to the
	// importer's disk before the sha check — which by construction runs after
	// the copy — can reject a byte of it.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, maxImportBlob+1))
	if err == nil && n > maxImportBlob {
		err = fmt.Errorf("archive member is larger than %s; re-run with --max-blob if the export really holds a file that big",
			humanBytes(maxImportBlob))
	}
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, "", err
	}
	return tmp, n, hex.EncodeToString(h.Sum(nil)), nil
}

func writeTarFile(tw *tar.Writer, name string, b []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b))}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

// exportFileName builds the default archive name from an untrusted project
// name: one path element, no separators, no control characters, bounded.
func exportFileName(project string, now time.Time) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			return -1
		case r == '/', r == '\\', r == ':':
			return '-'
		}
		return r
	}, project)
	name = strings.Trim(name, ". ")
	if name == "" {
		name = "project"
	}
	if len(name) > 64 {
		name = strings.ToValidUTF8(name[:64], "")
	}
	return fmt.Sprintf("%s-export-%s.tar.gz", name, now.Format("20060102"))
}
