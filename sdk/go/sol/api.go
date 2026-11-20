// Package sol is the Go client and app-side toolkit for the Sol app API (design/API.md).
//
// Apps depend on the API interface, never on a concrete client: in production it is a *Client
// talking to the daemon (FromEnv), in tests it is a memsol fake. The package also holds what an
// app needs to be called: Serve, Caller, the Actions router with JSON Schema validation, and
// the app.json Manifest. Standard library only.
//
// Invariants: errors from the API are *Error values matched with errors.Is against the Err*
// sentinels; apps decide who a caller is from Caller (headers), never from request bodies.
package sol

import (
	"context"
	"io"
	"net"
)

// API is the whole app API. Both *Client and memsol's fake implement it.
type API interface {
	// WhoAmI returns the person, device and app behind this connection.
	WhoAmI(ctx context.Context) (WhoAmI, error)
	// People lists paired contacts; Devices lists my own devices.
	People(ctx context.Context) ([]Contact, error)
	Devices(ctx context.Context) ([]Device, error)

	// Append adds body (any JSON value) to my feed named feed and returns the stored entry.
	Append(ctx context.Context, feed string, body any) (Entry, error)
	// Read returns entries of the feed address with pos >= from (0 means 1), in pos order, at most limit
	// (0 means the default 500).
	Read(ctx context.Context, address string, from uint64, limit int) ([]Entry, error)
	// Feeds lists readable feeds of this app owned by owner ("" means every owner).
	Feeds(ctx context.Context, owner string) ([]FeedInfo, error)
	// Events subscribes to server-sent events. The channel closes when ctx ends; a terminal
	// failure arrives as an Event of type EventError first.
	Events(ctx context.Context, opts EventOptions) (<-chan Event, error)

	// PutBlob stores the bytes read from r. name is an optional hint.
	PutBlob(ctx context.Context, r io.Reader, name string) (BlobInfo, error)
	// GetBlob streams a blob (or a byte range of it). The caller closes the result.
	GetBlob(ctx context.Context, hash string, opts BlobOptions) (io.ReadCloser, error)
	// StatBlob returns a blob's size and whether it is local.
	StatBlob(ctx context.Context, hash string, opts BlobOptions) (BlobStat, error)
	Pin(ctx context.Context, hashes []string) error
	Unpin(ctx context.Context, hashes []string) error

	Share(ctx context.Context, req ShareRequest) error
	Unshare(ctx context.Context, req ShareRequest) error
	// Shares lists who one of my feeds is shared with.
	Shares(ctx context.Context, feed string) ([]Share, error)

	// CallAction runs an action (mine or, with req.Person, a peer's).
	CallAction(ctx context.Context, req ActionCall) (ActionResult, error)
	// Actions lists the declared actions of an app ("" means all).
	Actions(ctx context.Context, app string) ([]ActionSpec, error)

	// Dial opens a live byte stream to a peer's app.
	Dial(ctx context.Context, req DialRequest) (net.Conn, error)
	// PostInbox posts a notice or an approval request.
	PostInbox(ctx context.Context, post InboxPost) (InboxResult, error)
	// Inbox lists this app's inbox items, newest first, with their answers and results.
	Inbox(ctx context.Context) ([]InboxItem, error)
	// Activity returns the most recent activity log rows (limit 0 means the default).
	Activity(ctx context.Context, limit int) ([]ActivityEntry, error)
}

var _ API = (*Client)(nil)
