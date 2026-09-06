package webapp

import (
	"errors"
	"time"
)

// Directory is where a hub's organizations live. The built-in one is this
// package's OrgDB (LocalDirectory); a deployment whose users, orgs and
// memberships are owned by an external identity system supplies its own,
// alongside its AuthProvider.
//
// Two rules shape this interface:
//
// Reads are on the request path. Role is called for every project request,
// including the /store/* sync endpoints a device hits every few seconds with
// a device token that carries no identity claims. An implementation backed by
// a remote system therefore has to answer Role from a local cache, and that
// cache is the implementation's business — the hub does not keep one, does not
// refresh one, and must never be written to from the side. (It used to be:
// the hub owned the mirror and the auth provider poked at it, which is how a
// hub-invented org that the identity system had never heard of could exist.)
//
// Writes are optional. A directory that does not own its data returns
// ErrManagedElsewhere and the handler answers 409 with ManageURL, so the hub
// never needs to know WHY it cannot write — only where the user should go.
type Directory interface {
	// ---- reads (request path) ----
	Role(orgID, email string) string
	Get(orgID string) (Org, bool)
	OrgsFor(email string) []Org
	ListInvites(orgID string) []OrgInvite
	ValidInvite(token string) bool
	// ManageURL is where this org is administered: a path within this hub
	// when it owns its orgs, an external page when it does not. The client
	// follows it and never has to know which kind of hub it is talking to.
	ManageURL(orgID string) string

	// ---- writes (ErrManagedElsewhere when the directory is read-only) ----
	Create(name, ownerEmail string) (Org, error)
	Rename(orgID, name string) error
	// Delete removes the org and its invites. Its projects are the hub's to
	// cascade (registry rows and storage) — the directory knows nothing of
	// project storage.
	Delete(orgID string) error
	AddMember(orgID, email, role string) error
	SetRole(orgID, email, role string) error
	RemoveMember(orgID, email string) error
	CreateInvite(orgID, creator string, ttl time.Duration) (OrgInvite, error)
	RevokeInvite(token string) bool
	Redeem(token string) (OrgInvite, bool)
	RecordInviteUse(token string)
}

// ErrManagedElsewhere is returned by a directory that does not own its
// organizations. Handlers turn it into 409 plus the org's ManageURL — the
// request was well-formed, it is the state of the world that makes it wrong.
var ErrManagedElsewhere = errors.New("this organization is managed outside this hub")

// LocalDirectory is the built-in directory: organizations owned by this hub,
// stored in its own metadata store. This is what every self-hosted install
// runs, and its behavior is exactly OrgDB's — the type exists to add the one
// thing an org store has no opinion about, which is where to send a browser
// to administer an org.
type LocalDirectory struct{ *OrgDB }

// ManageURL is the hub's own org page (a route in the frontend).
func (LocalDirectory) ManageURL(orgID string) string { return "/orgs/" + orgID }
