// Package remote abstracts the cloud object store a volume syncs through.
// beardrive is provider-agnostic: any backend that can put/get/list immutable
// objects works. Built-in schemes:
//
//	file:///abs/path      local or network-drive directory (also used in tests)
//	s3://bucket/prefix    Amazon S3 (or S3-compatible via AWS_ENDPOINT_URL)
//	gs://bucket/prefix    Google Cloud Storage
//	https://host:4173     a bdrive web server brokering one of the above —
//	                      the device needs no storage credentials at all
//
// Remote layout: blobs/<sha256> for content, journal/<device>.jsonl for op
// logs. Each device writes only its own journal, so there are no concurrent
// writers per object and no server-side coordination is needed.
package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// ErrForbidden marks a refusal by the hub's authorization — the device asked
// correctly and was told no, which is a different thing from being offline.
// The syncer keys its degraded states off it: a refused push means read-only
// (keep pulling), a refused pull means access is gone (pause, touch nothing).
var ErrForbidden = errors.New("forbidden")

type Object struct {
	Key  string
	Size int64
	// Modified is when the store last wrote this object, when the backend
	// reports it (S3, GCS) and the zero time when it does not. It is how the
	// hub decides an object can no longer change — see RemoteSource.verify.
	Modified time.Time
}

// SignedPut is a presigned direct-upload request: whoever holds the URL can
// PUT that one object until Expires, without ever seeing storage credentials.
type SignedPut struct {
	URL     string            // upload here
	Method  string            // always "PUT"
	Headers map[string]string // headers that must be sent verbatim (they are signed)
	Expires time.Time
}

// PutSigner is implemented by backends that can mint presigned upload URLs
// so clients write to storage directly. Backends without that capability
// (file://) simply don't implement it, and callers fall back to uploading
// through the server.
type PutSigner interface {
	SignPut(ctx context.Context, key string, size int64, ttl time.Duration) (*SignedPut, error)
}

// Deleter is the optional delete capability, in the PutSigner mold: the hub
// uses it to purge a deleted project's objects from storage. Deleting a key
// that does not exist is not an error. Sync clients never delete remote
// objects — blobs and journals are append-only from a device's point of view.
type Deleter interface {
	Delete(ctx context.Context, key string) error
}

type Backend interface {
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]Object, error)
	Exists(ctx context.Context, key string) (bool, error)
	Close() error
}

// ReadEvent is one agent file read reported to the hub for its read heatmap.
type ReadEvent struct {
	Path string `json:"path"`
	// Session is the agent session the read happened in, so the hub can join
	// a run's reads to the writes journal.Op.Session carries. A client string
	// — the hub pins each recorded row to the device it validated, never to
	// anything in this body (see handleReadReport).
	Session string    `json:"session,omitempty"`
	Time    time.Time `json:"time,omitzero"`
}

// ReadReporter is the optional read-telemetry capability, in the PutSigner
// mold: backends that sync through a hub report the device's agent reads so
// the heat view can split human from agent traffic. Object-store backends
// simply don't implement it — there is no hub to tell.
type ReadReporter interface {
	ReportReads(ctx context.Context, reads []ReadEvent) error
}

// Open creates a backend from a remote URL.
func Open(ctx context.Context, raw string) (Backend, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid remote %q: %w", raw, err)
	}
	switch u.Scheme {
	case "file":
		return newLocal(u.Path)
	case "s3":
		return newS3(ctx, u.Host, strings.Trim(u.Path, "/"))
	case "gs":
		return newGCS(ctx, u.Host, strings.Trim(u.Path, "/"))
	case "http", "https":
		return newHTTPBackend(raw)
	default:
		return nil, fmt.Errorf("unsupported remote scheme %q (supported: file://, s3://, gs://, https://)", u.Scheme)
	}
}
