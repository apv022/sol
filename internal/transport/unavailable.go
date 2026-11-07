package transport

import (
	"context"
	"net"
)

// Unavailable stands in for a transport that is enabled in config but cannot
// run (for example, no tor binary was found). It keeps the transport visible
// in the panel with the reason, instead of failing node startup.
type Unavailable struct {
	TransportName string
	Err           error
}

// NewUnavailable returns a placeholder transport that always reports err.
func NewUnavailable(name string, err error) *Unavailable {
	return &Unavailable{TransportName: name, Err: err}
}

func (u *Unavailable) Name() string                                   { return u.TransportName }
func (u *Unavailable) Start(context.Context) error                    { return u.Err }
func (u *Unavailable) Unpublish(string) error                         { return nil }
func (u *Unavailable) Stop() error                                    { return nil }
func (u *Unavailable) Handles(addr string) bool                       { return false }
func (u *Unavailable) Dial(context.Context, string) (net.Conn, error) { return nil, u.Err }
func (u *Unavailable) Publish(context.Context, Publication) (string, error) {
	return "", u.Err
}
func (u *Unavailable) Status() Status {
	return Status{Name: u.TransportName, State: StateFailed, Detail: u.Err.Error()}
}
