// Package proxy is the HTTP-aware reverse proxy used for public services
// whose protocol is "http". It forwards to 127.0.0.1:<port> and can strip
// response headers that identify the origin's software (spec.md 7.6 rule 5).
//
// Private services do not use this package: after the sol/1 handshake they
// are a raw TCP pipe, which handles HTTP, WebSockets, and range requests
// without any HTTP parsing in Sol.
package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// identifyingHeaders are removed from responses when stripping is on.
var identifyingHeaders = []string{
	"Server", "X-Powered-By", "X-AspNet-Version", "X-AspNetMvc-Version",
	"X-Generator", "X-Runtime", "X-Version", "Via",
}

// NewServiceProxy returns a handler that proxies to 127.0.0.1:port.
// WebSocket upgrades are passed through by httputil.ReverseProxy.
// If strip is true, identifying response headers are removed and upstream
// errors produce a generic page.
func NewServiceProxy(port int, strip bool) http.Handler {
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Do not forward client-identifying headers to the app.
			r.Out.Header.Del("X-Forwarded-For")
			r.Out.Header.Del("Forwarded")
		},
	}
	if strip {
		rp.ModifyResponse = func(resp *http.Response) error {
			StripHeaders(resp.Header)
			return nil
		}
		rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "service unavailable", http.StatusBadGateway)
		}
	}
	return rp
}

// StripHeaders removes headers that reveal server software or versions.
func StripHeaders(h http.Header) {
	for _, k := range identifyingHeaders {
		h.Del(k)
	}
}
