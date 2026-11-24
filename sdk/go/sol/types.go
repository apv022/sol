package sol

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// PersonInfo names a person.
type PersonInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Device is one device of a person.
type Device struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
	Anchor bool   `json:"anchor"`
}

// AppInfo is the calling app as the daemon sees it.
type AppInfo struct {
	ID     string   `json:"id"`
	Scopes []string `json:"scopes"`
}

// WhoAmI is the reply of GET /v1/whoami.
type WhoAmI struct {
	Person PersonInfo `json:"person"`
	Device Device     `json:"device"`
	App    AppInfo    `json:"app"`
}

// Contact is a paired person as returned by GET /v1/people.
type Contact struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Petname string `json:"petname"`
	// Renamed is true when the person gave this contact a petname themselves; otherwise Petname
	// is the default address label (lowercase, DNS-safe) derived from Name. Show DisplayName.
	Renamed      bool     `json:"renamed,omitempty"`
	Online       bool     `json:"online"`
	Devices      []Device `json:"devices"`
	SharesWithMe []string `json:"shares_with_me"`
	IShare       []string `json:"i_share"`
}

// Entry is one signed feed entry. Seq counts per (feed, device) and Prev chains to that
// device's previous entry; Pos is the reader's local cursor (1, 2, 3... in arrival order) and is
// not covered by Hash.
type Entry struct {
	Feed   string          `json:"feed"`
	Seq    uint64          `json:"seq"`
	Pos    uint64          `json:"pos"`
	Device string          `json:"device"`
	Time   time.Time       `json:"time"`
	Body   json.RawMessage `json:"body"`
	Hash   string          `json:"hash"`
	Prev   string          `json:"prev"`
}

// Decode unmarshals the entry body into v.
func (e Entry) Decode(v any) error { return json.Unmarshal(e.Body, v) }

// MaxBodySize is the largest entry body in bytes (64 KiB); larger data goes in a blob.
const MaxBodySize = 64 << 10

// FeedInfo is one row of GET /v1/feeds.
type FeedInfo struct {
	Feed string `json:"feed"`
	Pos  uint64 `json:"pos"`
}

// EventOptions selects what Events delivers.
type EventOptions struct {
	// Feeds are feed addresses to subscribe to. Empty means every readable feed of the app.
	Feeds []string
	// People subscribes to "person" events, Inbox to "inbox" events.
	People, Inbox bool
	// LastEventID resumes after that event id (the client also tracks it across reconnects).
	LastEventID string
}

// Event types.
const (
	EventEntry  = "entry"
	EventFeed   = "feed"
	EventPerson = "person"
	EventInbox  = "inbox"
	// EventTheme reports that the active theme or the appearance settings changed
	// (design/UI.md "Themes"); its data is Event.Theme.
	EventTheme = "theme"
	// EventError is synthesized by the client when the stream fails for good; Event.Err is set
	// and the channel closes right after.
	EventError = "error"
)

// FeedChange is the data of a "feed" event.
type FeedChange struct {
	Feed     string `json:"feed"`
	Readable bool   `json:"readable"`
}

// InboxEvent is the data of an "inbox" event: an item changed. Answer is "approved", "denied"
// or empty.
type InboxEvent struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Answer string `json:"answer,omitempty"`
}

// ThemeEvent is the data of a "theme" event (ui/kit/TOKENS.md): the active theme's name and
// the appearance settings, which UIs apply before swapping /_sol/theme.css.
type ThemeEvent struct {
	Name     string            `json:"name"`
	Settings map[string]string `json:"settings"`
}

// Event is one server-sent event; exactly one of Entry, Feed, Person, Inbox, Theme is set, by
// Type.
type Event struct {
	ID     string
	Type   string
	Entry  *Entry
	Feed   *FeedChange
	Person *Contact
	Inbox  *InboxEvent
	Theme  *ThemeEvent
	Err    string
}

// BlobInfo is the reply of PutBlob.
type BlobInfo struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// BlobOptions control GetBlob and StatBlob.
type BlobOptions struct {
	// From is the person to fetch from when the blob is not local (optional).
	From string
	// Offset and Length select a byte range; Length 0 means to the end.
	Offset, Length int64
}

// BlobStat is the reply of StatBlob (HEAD).
type BlobStat struct {
	Size  int64
	Local bool
}

// Share modes.
const (
	ModeRead  = "read"
	ModeWrite = "write"
)

// ShareRequest is the body of POST/DELETE /v1/shares.
type ShareRequest struct {
	Feed   string   `json:"feed"`
	People []string `json:"people"`
	Mode   string   `json:"mode,omitempty"`
}

// Share is one row of GET /v1/shares: a feed shared with one person.
type Share struct {
	Feed   string `json:"feed"`
	Person string `json:"person"`
	Mode   string `json:"mode"`
}

// ActionCall is the body of POST /v1/actions/call.
type ActionCall struct {
	Action string          `json:"action"`
	Args   json.RawMessage `json:"args,omitempty"`
	Person string          `json:"person,omitempty"`
	DryRun bool            `json:"dry_run,omitempty"`
}

// ActionResult is what an action call returns: the action's Output, or an Approval id when the
// call needs a person's approval first.
type ActionResult struct {
	Output   json.RawMessage
	Approval string
}

// DialRequest is the body of POST /v1/dial.
type DialRequest struct {
	Person string `json:"person"`
	App    string `json:"app"`
	Device string `json:"device,omitempty"`
	Path   string `json:"path"`
}

// InboxPost is the body of POST /v1/inbox.
type InboxPost struct {
	Kind   string      `json:"kind"` // "notice" or "approval"
	Title  string      `json:"title"`
	Body   string      `json:"body,omitempty"`
	To     string      `json:"to,omitempty"`
	Action *ActionCall `json:"action,omitempty"`
}

// InboxItem is one row of GET /v1/inbox: a notice or an approval of this app, with its answer
// and, once an approved action ran, its outcome (API.md section 8 says what is kept, and for
// how long).
type InboxItem struct {
	ID    string    `json:"id"`
	App   string    `json:"app"`
	Kind  string    `json:"kind"` // "notice" or "approval"
	Title string    `json:"title"`
	Body  string    `json:"body,omitempty"`
	From  string    `json:"from,omitempty"` // a contact's person ID; empty for my own apps
	Time  time.Time `json:"time"`
	// Answer is "approved", "denied", "expired" (the daemon restarted before an approval with
	// secret args was answered) or empty while open.
	Answer string `json:"answer,omitempty"`
	// Action is the call to approve. Its args are there only while the approval is open, with
	// secret values shown as Redacted; once answered only the action name and person remain.
	Action *ActionCall `json:"action,omitempty"`
	// ArgsDigest is "sha256:<hex>" of the call's args, as the activity log records it.
	ArgsDigest string `json:"args_digest,omitempty"`
	// SecretArgs reports that Action's args had secret values, held in memory only.
	SecretArgs bool `json:"secret_args,omitempty"`
	// Agent is the agent key whose call needed the approval (empty for an app's own request).
	Agent string `json:"agent,omitempty"`
	// Outcome is "ok" or "error" once an approved action ran.
	Outcome string `json:"outcome,omitempty"`
	// Result is the approved action's output, delivered once to the waiting requester and
	// never stored; Error is its failure (kept).
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// InboxResult is the reply of POST /v1/inbox.
type InboxResult struct {
	ID string `json:"id"`
}

// ActivityEntry is one row of the signed activity log (GET /v1/activity).
type ActivityEntry struct {
	Time       time.Time `json:"time"`
	Person     string    `json:"person"`
	Device     string    `json:"device"`
	Agent      string    `json:"agent,omitempty"`
	App        string    `json:"app"`
	Action     string    `json:"action"`
	ArgsDigest string    `json:"args_digest"`
	Result     string    `json:"result"`
}

var (
	appIDRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	feedNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	hashRe     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ValidAppID reports whether s is a legal app id.
func ValidAppID(s string) bool { return appIDRe.MatchString(s) }

// ValidFeedName reports whether s is a legal feed name.
func ValidFeedName(s string) bool { return feedNameRe.MatchString(s) }

// ValidHash reports whether s is a legal blob hash.
func ValidHash(s string) bool { return hashRe.MatchString(s) }

// Address builds a feed address <person>/<app>/<feed>.
func Address(person, app, feed string) string { return person + "/" + app + "/" + feed }

// ParseAddress splits a feed address.
func ParseAddress(addr string) (person, app, feed string, err error) {
	p := strings.Split(addr, "/")
	if len(p) != 3 || p[0] == "" || !ValidAppID(p[1]) || !ValidFeedName(p[2]) {
		return "", "", "", Errorf("bad_request", fmt.Sprintf("bad feed address %q", addr))
	}
	return p[0], p[1], p[2], nil
}

// DisplayName is the name UIs show for a contact: the petname I gave them, else their own name,
// else the address label.
func (c Contact) DisplayName() string {
	switch {
	case c.Renamed && c.Petname != "":
		return c.Petname
	case c.Name != "":
		return c.Name
	}
	return c.Petname
}
