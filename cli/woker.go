package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

// todo extended url.URL. struct
type Nodes []url.URL

type Algorithm = func(req *http.Request)

type loadBalancer struct {
	AllowScheme []string
	Backends    Nodes
	Transport   http.RoundTripper
	Algorithm   Algorithm
	backends    uint32
	req         uint32
}

//func (l *loadBalancer) IsAllowScheme(url *url.URL) bool {
//	//check Scheme
//	//var state bool
//	//if scheme == "" && req.TLS != nil {
//	//	req.URL.Scheme = "https"
//	//}
//	// localhost is always empty scheme
//	if url.Host == "localhost" {
//		return true
//	}
//	for _, s := range l.AllowScheme {
//		if s == url.Scheme {
//			return true
//		}
//	}
//	return false
//}

var hopHeaders = []string{
	"connection",
	"keep-alive",
	"proxy-authenticate",
	"proxy-authorization",
	"te",
	"trailers",
	"transfer-encoding",
	"upgrade",
}

//func (l *loadBalancer) removeHopHeader(header http.Header) http.Header {
//	for _, v := range hopHeaders {
//		hv := header.Get(v)
//		if hv == "" {
//			continue
//		}
//		// support gRPC Te header.
//		// see https://github.com/golang/go/issues/21096
//		if strings.ToLower(v) == "Te" && hv == "trailers" {
//			continue
//		}
//		header.Del(v)
//	}
//	return header
//}

func (l *loadBalancer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// inspired https://github.com/golang/go/blob/master/src/net/http/httputil/reverseproxy.go
	log.Println(req.RemoteAddr, " ", req.Method, " ", req.URL)
	transport := l.Transport

	if transport == nil {
		transport = http.DefaultTransport
	}

	ctx := req.Context()

	if cn, ok := w.(http.CloseNotifier); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		notifyChan := cn.CloseNotify()
		go func() {
			select {
			case <-notifyChan:
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	outreq := req.Clone(ctx)
	l.Algorithm(outreq)
	outreq.Close = false

	// RFC 2616, section 13.5.1 https://tools.ietf.org/html/rfc2616#section-13.5.1
	for _, v := range hopHeaders {
		hv := outreq.Header.Get(v)
		if hv == "" {
			continue
		}
		// support gRPC Te header.
		// see https://github.com/golang/go/issues/21096
		if v == "Te" && hv == "trailers" {
			continue
		}
		outreq.Header.Del(v)
	}

	res, err := transport.RoundTrip(outreq)
	if err != nil {
		log.Println(err)
	}
	defer func() {
		err = res.Body.Close()
	}()

	// RFC 2616, section 13.5.1 https://tools.ietf.org/html/rfc2616#section-13.5.1
	for _, v := range hopHeaders {
		res.Header.Del(v)
	}

	copyHeader(w.Header(), res.Header)

	announcedTrailers := len(res.Trailer)
	if announcedTrailers > 0 {
		trailerKeys := make([]string, 0, len(res.Trailer))
		for k := range res.Trailer {
			trailerKeys = append(trailerKeys, k)
		}
		w.Header().Add("Trailer", strings.Join(trailerKeys, ", "))
	}
	w.WriteHeader(res.StatusCode)

	if _, err = io.Copy(w, res.Body); err != nil {
		log.Println(err)
	}

	if len(res.Trailer) > 0 {
		// Force chunking if we saw a response trailer.
		// This prevents net/http from calculating the length for short
		// bodies and adding a Content-Length.
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}

	if len(res.Trailer) == announcedTrailers {
		copyHeader(w.Header(), res.Trailer)
		return
	}
	for k, vv := range res.Trailer {
		k = http.TrailerPrefix + k
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
}

func copyHeader(dst http.Header, src http.Header) {
	for k, v := range src {
		for _, vv := range v {
			dst.Add(k, vv)
		}
	}
}

func NewLoadBalancer(node Nodes) *loadBalancer {

	backend := len(node)

	//_ = httputil.NewSingleHostReverseProxy(&u).Director

	return &loadBalancer{
		AllowScheme: []string{"https", "http"},
		Backends:    node,
		Transport:   nil,
		backends:    uint32(backend),
		req:         uint32(0),
		// access golang reverse proxy Algorithm
		// TODO fix

	}
}

// todo
// error handling
// add original Algorithm
// run benchmark

func main() {
	u := url.URL{
		Scheme: "http",
		Host:   "localhost:3000",
	}

	u2 := url.URL{
		Scheme: "http",
		Host:   "localhost:3030",
	}
	backends := []url.URL{u, u2}
	l := NewLoadBalancer(backends)
	l.Algorithm = func(req *http.Request) {
		pick := l.Backends[l.req]

		fmt.Println(l.req, uint32(len(l.Backends)-1))
		if l.req >= uint32(len(l.Backends)-1) {
			atomic.StoreUint32(&l.req, 0)
		} else {
			atomic.AddUint32(&l.req, 1)
		}
		fmt.Println(pick)
		targetQuery := pick.RawQuery
		req.URL.Scheme = pick.Scheme
		req.URL.Host = pick.Host
		req.URL.Path = pick.Path + req.URL.Path
		if targetQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = targetQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
		}
		if _, ok := req.Header["User-Agent"]; !ok {
			req.Header.Set("User-Agent", "")
		}
	}
	err := http.ListenAndServe(":8080", l)
	if err != nil {
		log.Fatalln(err)
	}
}
