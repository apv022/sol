package main

// sol events: the admin event stream (GET /admin/v1/events, server-sent events) printed as it
// arrives, one event per line, until interrupted. It is how a shell around the node (the Android
// app's notifications) follows inbox items without polling. A lost connection or a daemon that is
// not running yet is retried with a growing delay; a reconnect resumes after the last event seen
// (Last-Event-ID), and a daemon that restarted in between replays its current window, so a reader
// must tolerate repeats (events carry ids for that).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// streamEvent is one admin event as `sol events --json` prints it.
type streamEvent struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// eventsBackoff bounds the delay between reconnects; tests shorten it.
var eventsBackoff = struct{ min, max time.Duration }{time.Second, 30 * time.Second}

// eventsCmd is `sol events [--json]`: it streams until SIGINT/SIGTERM.
func eventsCmd(dir string, asJSON bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return streamEvents(ctx, dir, asJSON, os.Stdout)
}

// streamEvents prints events from the daemon on dir to w until ctx ends, reconnecting with
// backoff. Plain output is "<type> <data>"; --json prints a streamEvent per line.
func streamEvents(ctx context.Context, dir string, asJSON bool, w io.Writer) error {
	last, delay := "", eventsBackoff.min
	for {
		got, connected, err := streamOnce(ctx, dir, last, func(ev streamEvent) error {
			if asJSON {
				b, _ := json.Marshal(ev)
				_, err := fmt.Fprintf(w, "%s\n", b)
				return err
			}
			_, err := fmt.Fprintf(w, "%s %s\n", ev.Type, ev.Data)
			return err
		})
		if got != "" {
			last = got
		}
		if connected {
			delay = eventsBackoff.min // it was up: start the backoff over
		}
		if ctx.Err() != nil {
			return nil
		}
		var we writeError
		if errors.As(err, &we) {
			return we.error // stdout went away (the reader quit): nothing left to do
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay = min(delay*2, eventsBackoff.max)
	}
}

// writeError marks a failure to print, which ends the command instead of reconnecting.
type writeError struct{ error }

// streamOnce runs one connection: it returns the id of the last event delivered ("" if none)
// whether the daemon answered, and why the stream ended. api.url and admin.token are read afresh, since a restarted daemon
// may listen elsewhere with a new token. The token is never printed.
func streamOnce(ctx context.Context, dir, last string, emit func(streamEvent) error) (string, bool, error) {
	u, err := os.ReadFile(filepath.Join(dir, "api.url"))
	if err != nil {
		return "", false, err
	}
	tok, err := os.ReadFile(filepath.Join(dir, "admin.token"))
	if err != nil {
		return "", false, err
	}
	api := strings.TrimSpace(string(u))
	network, addr, base := "tcp", "", api
	if p, ok := strings.CutPrefix(api, "unix://"); ok {
		network, addr, base = "unix", p, "http://sol"
	} else if h, ok := strings.CutPrefix(api, "http://"); ok {
		addr = h
	} else {
		return "", false, fmt.Errorf("bad api.url %q", api)
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}}}
	defer hc.CloseIdleConnections()
	r, err := http.NewRequestWithContext(ctx, "GET", base+"/admin/v1/events", nil)
	if err != nil {
		return "", false, err
	}
	r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	r.Header.Set("Accept", "text/event-stream")
	if last != "" {
		r.Header.Set("Last-Event-ID", last)
	}
	resp, err := hc.Do(r)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("events: %s", resp.Status)
	}
	delivered := ""
	var ev streamEvent
	var data []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" { // end of one event
			if len(data) > 0 && ev.Type != "" {
				ev.Data = json.RawMessage(strings.Join(data, "\n"))
				if !json.Valid(ev.Data) {
					ev.Data, _ = json.Marshal(string(ev.Data))
				}
				if err := emit(ev); err != nil {
					return delivered, true, writeError{err}
				}
				if ev.ID != "" {
					delivered = ev.ID
				}
			}
			ev, data = streamEvent{}, nil
			continue
		}
		if strings.HasPrefix(line, ":") { // a comment: the greeting or a heartbeat
			continue
		}
		k, v, _ := strings.Cut(line, ":")
		v = strings.TrimPrefix(v, " ")
		switch k {
		case "id":
			ev.ID = v
		case "event":
			ev.Type = v
		case "data":
			data = append(data, v)
		}
	}
	if err := sc.Err(); err != nil {
		return delivered, true, err
	}
	return delivered, true, io.ErrUnexpectedEOF // the daemon closed the stream (it stopped)
}
