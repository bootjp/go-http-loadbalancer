package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

type Nodes map[url.URL]bool

type loadBalancer struct {
	AllowScheme []string
	Backends    Nodes
	Transport   http.RoundTripper
	Director    func(req *http.Request)
}

func (l *loadBalancer) IsAllowScheme(url *url.URL) bool {
	//check Scheme
	//var state bool
	//if scheme == "" && req.TLS != nil {
	//	req.URL.Scheme = "https"
	//}
	// localhost is always empty scheme
	if url.Host == "localhost" {
		return true
	}
	for _, s := range l.AllowScheme {
		if s == url.Scheme {
			return true
		}
	}
	return false
}

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
	l.Director(outreq)
	outreq.Close = false

	// todo remove hop header  RFC 2616, section 13.5.1 https://tools.ietf.org/html/rfc2616#section-13.5.1
	res, err := transport.RoundTrip(outreq)
	if err != nil {
		log.Println(err)
	}
	defer func() {
		err = res.Body.Close()
	}()

	// todo remove hop header  RFC 2616, section 13.5.1 https://tools.ietf.org/html/rfc2616#section-13.5.1
	for k, v := range res.Header {
		for _, vv := range v {
			w.Header().Add(k, vv)
		}
	}

	if _, err = io.Copy(w, res.Body); err != nil {
		log.Println(err)
	}

}

func NewLoadBalancer() *loadBalancer {
	u := url.URL{
		Scheme: "http",
		Host:   "localhost:3000",
	}

	return &loadBalancer{
		AllowScheme: []string{"https", "http"},
		Backends:    map[url.URL]bool{u: true},
		// access golang reverse proxy Director
		// TODO fix
		Director: httputil.NewSingleHostReverseProxy(&u).Director,
	}
}

// todo
// deleteHopHEADR
// add forwarded header
// error handling
// add original Director
// run benchmark

func main() {
	l := NewLoadBalancer()
	err := http.ListenAndServe(":8080", l)
	if err != nil {
		log.Fatalln(err)
	}
}
