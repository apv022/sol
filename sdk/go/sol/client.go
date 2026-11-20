package sol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Client is the real API client: HTTP/1.1 + JSON to the daemon.
type Client struct {
	token   string
	network string // "unix" or "tcp"
	addr    string
	base    string
	hc      *http.Client
}

// FromEnv builds a Client from SOL_API and SOL_APP_TOKEN.
func FromEnv() (*Client, error) {
	api := os.Getenv("SOL_API")
	if api == "" {
		return nil, errors.New("sol: SOL_API is not set")
	}
	return New(api, os.Getenv("SOL_APP_TOKEN"))
}

// New builds a Client for api ("unix:///path" or "http://host:port") and an app token.
func New(api, token string) (*Client, error) {
	c := &Client{token: token}
	u, err := url.Parse(api)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "unix":
		c.network, c.addr, c.base = "unix", u.Path, "http://sol"
	case "http":
		c.network, c.addr, c.base = "tcp", u.Host, "http://"+u.Host
	default:
		return nil, fmt.Errorf("sol: unsupported SOL_API %q", api)
	}
	c.hc = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, c.network, c.addr)
		},
	}}
	return c, nil
}

func (c *Client) req(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	return r, nil
}

func decodeErr(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var w struct {
		Error *Error `json:"error"`
	}
	if json.Unmarshal(b, &w) == nil && w.Error != nil && w.Error.Code != "" {
		return w.Error
	}
	return &Error{Code: codeForStatus(resp.StatusCode), Message: http.StatusText(resp.StatusCode)}
}

// do sends a JSON request and decodes a JSON reply into out (may be nil).
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	r, err := c.req(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return decodeErr(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// WhoAmI implements API.
func (c *Client) WhoAmI(ctx context.Context) (w WhoAmI, err error) {
	err = c.do(ctx, "GET", "/v1/whoami", nil, &w)
	return
}

// People implements API.
func (c *Client) People(ctx context.Context) (p []Contact, err error) {
	err = c.do(ctx, "GET", "/v1/people", nil, &p)
	return
}

// Devices implements API.
func (c *Client) Devices(ctx context.Context) (d []Device, err error) {
	err = c.do(ctx, "GET", "/v1/devices", nil, &d)
	return
}

// Append implements API.
func (c *Client) Append(ctx context.Context, feed string, body any) (e Entry, err error) {
	err = c.do(ctx, "POST", "/v1/feeds/"+feed+"/append", map[string]any{"body": body}, &e)
	return
}

// Read implements API.
func (c *Client) Read(ctx context.Context, address string, from uint64, limit int) (es []Entry, err error) {
	q := url.Values{}
	if from > 0 {
		q.Set("from", strconv.FormatUint(from, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	err = c.do(ctx, "GET", "/v1/feeds/"+address+"?"+q.Encode(), nil, &es)
	return
}

// Feeds implements API.
func (c *Client) Feeds(ctx context.Context, owner string) (f []FeedInfo, err error) {
	err = c.do(ctx, "GET", "/v1/feeds?"+url.Values{"owner": {owner}}.Encode(), nil, &f)
	return
}

// PutBlob implements API.
func (c *Client) PutBlob(ctx context.Context, r io.Reader, name string) (b BlobInfo, err error) {
	req, err := c.req(ctx, "PUT", "/v1/blobs", r)
	if err != nil {
		return b, err
	}
	if name != "" {
		req.Header.Set("Sol-Name", name)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return b, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return b, decodeErr(resp)
	}
	err = json.NewDecoder(resp.Body).Decode(&b)
	return
}

func blobPath(hash string, o BlobOptions) string {
	p := "/v1/blobs/" + hash
	if o.From != "" {
		p += "?" + url.Values{"from": {o.From}}.Encode()
	}
	return p
}

func rangeHeader(o BlobOptions) string {
	switch {
	case o.Length > 0:
		return fmt.Sprintf("bytes=%d-%d", o.Offset, o.Offset+o.Length-1)
	case o.Offset > 0:
		return fmt.Sprintf("bytes=%d-", o.Offset)
	}
	return ""
}

// GetBlob implements API.
func (c *Client) GetBlob(ctx context.Context, hash string, o BlobOptions) (io.ReadCloser, error) {
	req, err := c.req(ctx, "GET", blobPath(hash, o), nil)
	if err != nil {
		return nil, err
	}
	if h := rangeHeader(o); h != "" {
		req.Header.Set("Range", h)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, decodeErr(resp)
	}
	return resp.Body, nil
}

// StatBlob implements API. Size comes from Content-Length, Local from the Sol-Local header.
func (c *Client) StatBlob(ctx context.Context, hash string, o BlobOptions) (s BlobStat, err error) {
	req, err := c.req(ctx, "HEAD", blobPath(hash, o), nil)
	if err != nil {
		return s, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return s, decodeErr(resp)
	}
	return BlobStat{Size: resp.ContentLength, Local: resp.Header.Get("Sol-Local") == "1"}, nil
}

// Pin implements API.
func (c *Client) Pin(ctx context.Context, hashes []string) error {
	return c.do(ctx, "POST", "/v1/blobs/pin", map[string]any{"hashes": hashes}, nil)
}

// Unpin implements API.
func (c *Client) Unpin(ctx context.Context, hashes []string) error {
	return c.do(ctx, "POST", "/v1/blobs/unpin", map[string]any{"hashes": hashes}, nil)
}

// Share implements API.
func (c *Client) Share(ctx context.Context, req ShareRequest) error {
	return c.do(ctx, "POST", "/v1/shares", req, nil)
}

// Unshare implements API.
func (c *Client) Unshare(ctx context.Context, req ShareRequest) error {
	return c.do(ctx, "DELETE", "/v1/shares", req, nil)
}

// Shares implements API.
func (c *Client) Shares(ctx context.Context, feed string) (s []Share, err error) {
	err = c.do(ctx, "GET", "/v1/shares?"+url.Values{"feed": {feed}}.Encode(), nil, &s)
	return
}

// CallAction implements API. A reply that is exactly {"approval": "<id>"} is reported as an
// Approval; anything else is the Output.
func (c *Client) CallAction(ctx context.Context, req ActionCall) (ActionResult, error) {
	var raw json.RawMessage
	if err := c.do(ctx, "POST", "/v1/actions/call", req, &raw); err != nil {
		return ActionResult{}, err
	}
	var ap struct {
		Approval *string `json:"approval"`
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil && len(m) == 1 && json.Unmarshal(raw, &ap) == nil && ap.Approval != nil {
		return ActionResult{Approval: *ap.Approval}, nil
	}
	return ActionResult{Output: raw}, nil
}

// Actions implements API.
func (c *Client) Actions(ctx context.Context, app string) (a []ActionSpec, err error) {
	p := "/v1/actions"
	if app != "" {
		p += "?" + url.Values{"app": {app}}.Encode()
	}
	err = c.do(ctx, "GET", p, nil, &a)
	return
}

// PostInbox implements API.
func (c *Client) PostInbox(ctx context.Context, post InboxPost) (r InboxResult, err error) {
	err = c.do(ctx, "POST", "/v1/inbox", post, &r)
	return
}

// Inbox implements API.
func (c *Client) Inbox(ctx context.Context) (items []InboxItem, err error) {
	err = c.do(ctx, "GET", "/v1/inbox", nil, &items)
	return
}

// Activity implements API.
func (c *Client) Activity(ctx context.Context, limit int) (a []ActivityEntry, err error) {
	p := "/v1/activity"
	if limit > 0 {
		p += "?limit=" + strconv.Itoa(limit)
	}
	err = c.do(ctx, "GET", p, nil, &a)
	return
}

// bufConn returns buffered bytes from the handshake before reading the raw conn.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// Dial implements API: it upgrades a connection to a raw stream ("Upgrade: sol-stream").
func (c *Client) Dial(ctx context.Context, req DialRequest) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, c.network, c.addr)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(req)
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	r, err := c.req(ctx, "POST", "/v1/dial", bytes.NewReader(body))
	if err != nil {
		conn.Close()
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "sol-stream")
	if err := r.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, r)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer conn.Close()
		defer resp.Body.Close()
		return nil, decodeErr(resp)
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufConn{Conn: conn, r: br}, nil
}

// Events implements API. The first connection is made before it returns, so the subscription is
// live when it does. After that the client reconnects with Last-Event-ID and backoff on its own.
func (c *Client) Events(ctx context.Context, o EventOptions) (<-chan Event, error) {
	q := url.Values{}
	if len(o.Feeds) > 0 {
		q.Set("feeds", strings.Join(o.Feeds, ","))
	}
	if o.People {
		q.Set("people", "1")
	}
	if o.Inbox {
		q.Set("inbox", "1")
	}
	last := o.LastEventID
	connect := func() (*http.Response, error) {
		r, err := c.req(ctx, "GET", "/v1/events?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		r.Header.Set("Accept", "text/event-stream")
		if last != "" {
			r.Header.Set("Last-Event-ID", last)
		}
		resp, err := c.hc.Do(r)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode/100 != 2 {
			defer resp.Body.Close()
			return nil, decodeErr(resp)
		}
		return resp, nil
	}
	resp, err := connect()
	if err != nil {
		return nil, err
	}
	out := make(chan Event)
	go func() {
		defer close(out)
		backoff := 100 * time.Millisecond
		for {
			readSSE(ctx, resp.Body, func(ev Event) bool {
				backoff = 100 * time.Millisecond
				last = ev.ID
				select {
				case out <- ev:
					return true
				case <-ctx.Done():
					return false
				}
			})
			resp.Body.Close()
			for {
				if ctx.Err() != nil {
					return
				}
				resp, err = connect()
				if err == nil {
					break
				}
				var ae *Error
				if errors.As(err, &ae) && ae.Code != "unavailable" && ae.Code != "internal" {
					select {
					case out <- Event{Type: EventError, Err: err.Error()}:
					case <-ctx.Done():
					}
					return
				}
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				}
				if backoff < 2*time.Second {
					backoff *= 2
				}
			}
		}
	}()
	return out, nil
}

// readSSE parses a text/event-stream, calling emit per decoded event until emit returns false
// or the stream ends.
func readSSE(ctx context.Context, r io.Reader, emit func(Event) bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var id, typ string
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if typ != "" {
				if ev, ok := decodeEvent(id, typ, strings.Join(data, "\n")); ok && !emit(ev) {
					return
				}
			}
			typ, data = "", nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimSpace(line[3:])
		case strings.HasPrefix(line, "event:"):
			typ = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[5:], " "))
		}
	}
}

func decodeEvent(id, typ, data string) (Event, bool) {
	ev := Event{ID: id, Type: typ}
	var err error
	switch typ {
	case EventEntry:
		ev.Entry = new(Entry)
		err = json.Unmarshal([]byte(data), ev.Entry)
	case EventFeed:
		ev.Feed = new(FeedChange)
		err = json.Unmarshal([]byte(data), ev.Feed)
	case EventPerson:
		ev.Person = new(Contact)
		err = json.Unmarshal([]byte(data), ev.Person)
	case EventInbox:
		ev.Inbox = new(InboxEvent)
		err = json.Unmarshal([]byte(data), ev.Inbox)
	case EventTheme:
		ev.Theme = new(ThemeEvent)
		err = json.Unmarshal([]byte(data), ev.Theme)
	default:
		return ev, false // unknown types are ignored for forward compatibility
	}
	return ev, err == nil
}

// EncodeEvent writes ev in text/event-stream form (used by servers, including memsol).
func EncodeEvent(w io.Writer, ev Event) error {
	var v any
	switch ev.Type {
	case EventEntry:
		v = ev.Entry
	case EventFeed:
		v = ev.Feed
	case EventPerson:
		v = ev.Person
	case EventInbox:
		v = ev.Inbox
	case EventTheme:
		v = ev.Theme
	default:
		return fmt.Errorf("sol: cannot encode event type %q", ev.Type)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, b)
	return err
}
