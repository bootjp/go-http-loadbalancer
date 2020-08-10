package main

import (
	"context"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpguts"
)

var Logger *log.Logger

func logf(format string, v ...interface{}) {
	if Logger == nil {
		log.Printf(format, v...)
		return
	}
	Logger.Printf(format, v...)
}

type HealthCheck struct {
	Endpoint           url.URL
	ExpectedStatusCode int
	ExpectedResponse   string
	Timeout            time.Duration
}

type Node struct {
	url.URL
	Alive       bool
	HealthCheck HealthCheck

	lock *sync.RWMutex
}

func NewNode(URL url.URL, healthCheck HealthCheck) *Node {
	return &Node{URL: URL, Alive: false, HealthCheck: healthCheck, lock: &sync.RWMutex{}}
}

func (n *Node) IsAlive() bool {
	n.lock.RLock()
	defer n.lock.RUnlock()
	return n.Alive
}

func (n *Node) StatusAlive(status bool) {
	n.lock.Lock()
	defer n.lock.Unlock()
	n.Alive = status
}

type loadBalance struct {
	Backends     []*Node
	Transport    http.RoundTripper
	Timeout      time.Duration
	ErrorHandler func(http.ResponseWriter, *http.Request, error)

	mu            *sync.Mutex
	roundRobinCnt uint64 // this lb is max backend nodes 4294967295.
}

var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Upgrade",
}

type RoundRobin struct {
}

type BalanceAlg interface {
	BalanceAlg(req *http.Request, aliveNodes []*Node)
}

type LoadBalancer interface {
	BalanceAlg(req *http.Request, aliveNodes []*Node)
	ServeHTTP(w http.ResponseWriter, req *http.Request)
	NodeCheck(ctx context.Context, node *Node)
	Run() error
}

func (l *loadBalance) Run() error {
	for _, backend := range l.Backends {
		ctx := context.TODO()
		l.NodeCheck(ctx, backend)
	}
	go l.RunCheckNode()
	return nil
}

func (l *loadBalance) defaultErrorHandler(w http.ResponseWriter, req *http.Request, err error) {
	logf("load balancer error %s", err)
	w.WriteHeader(http.StatusBadGateway)
}

func (l *loadBalance) pickAliveNodes() []*Node {
	var alive []*Node
	for _, n := range l.Backends {
		if !n.IsAlive() {
			continue
		}
		alive = append(alive, n)
	}

	return alive
}

func (l *loadBalance) selectNodeByRR(n []*Node) *Node {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roundRobinCnt = l.roundRobinCnt + 1
	return n[(l.roundRobinCnt-1)%uint64(len(n))]
}

func (l *loadBalance) getErrorHandler() func(http.ResponseWriter, *http.Request, error) {
	if l.ErrorHandler != nil {
		return l.ErrorHandler
	}
	return l.defaultErrorHandler
}

func (l *loadBalance) NodeCheck(ctx context.Context, node *Node) {

	select {
	case <-ctx.Done():
		node.StatusAlive(false)
	default:
		req, err := http.NewRequestWithContext(
			ctx,
			"GET",
			node.HealthCheck.Endpoint.String(),
			nil,
		)
		if err != nil {
			logf("%s", err)
			node.StatusAlive(false)
			return
		}
		client := &http.Client{}
		res, err := client.Do(req)
		if err != nil {
			logf("%s", err)
			node.StatusAlive(false)
			return
		}
		defer func() {
			err = res.Body.Close()
		}()
		if res.StatusCode != node.HealthCheck.ExpectedStatusCode {
			logf("%s", err)
			node.StatusAlive(false)
			return
		}
		b, _ := ioutil.ReadAll(res.Body)
		if string(b) != node.HealthCheck.ExpectedResponse {
			node.StatusAlive(false)
			return
		}

		node.StatusAlive(true)
		return
	}
}

func (l *loadBalance) BalanceAlg(req *http.Request, aliveNodes []*Node) {
	node := l.selectNodeByRR(aliveNodes)

	req.URL.Scheme = node.Scheme
	req.URL.Host = node.Host
	req.URL.Path = node.Path + req.URL.Path

	targetQuery := node.RawQuery
	if targetQuery == "" || req.URL.RawQuery == "" {
		req.URL.RawQuery = targetQuery + req.URL.RawQuery
	} else {
		req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
	}

}
func (l *loadBalance) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// inspired https://github.com/golang/go/blob/master/src/net/http/httputil/reverseproxy.go
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
	if req.ContentLength == 0 {
		outreq.Body = nil // Issue 16036: nil Body for http.Transport retries
	}
	if outreq.Header == nil {
		outreq.Header = make(http.Header) // Issue 33142: historical behavior was to always allocate
	}

	// node is not available check
	aliveNode := l.pickAliveNodes()
	if len(aliveNode) == 0 {
		l.getErrorHandler()(w, req, errors.New("no node available"))
		return
	}

	l.BalanceAlg(outreq, aliveNode)
	outreq.Close = false
	reqUpType := upgradeType(outreq.Header)

	f := outreq.Header.Get("Connection")
	for _, sf := range strings.Split(f, ",") {
		if sf = textproto.TrimString(sf); sf != "" {
			outreq.Header.Del(sf)
		}
	}

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
	if reqUpType != "" {
		outreq.Header.Set("Connection", "Upgrade")
		outreq.Header.Set("Upgrade", reqUpType)
	}

	if _, ok := outreq.Header["User-Agent"]; !ok {
		outreq.Header.Set("User-Agent", "")
	}

	xff := outreq.Header.Get("X-Forwarded-For")
	addr, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		l.getErrorHandler()(w, req, err)
		return
	}

	switch xff {
	case "":
		outreq.Header.Add("X-Forwarded-For", addr)
	default:
		xff += ", " + addr
		outreq.Header.Set("X-Forwarded-For", xff)
	}

	res, err := transport.RoundTrip(outreq)
	if err != nil {
		l.getErrorHandler()(w, req, err)
		return
	}

	for _, f := range res.Header["Connection"] {
		for _, sf := range strings.Split(f, ",") {
			if sf = textproto.TrimString(sf); sf != "" {
				res.Header.Del(sf)
			}
		}
	}

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
		l.getErrorHandler()(w, req, err)
		return
	}
	defer func() {
		if err = res.Body.Close(); err != nil {
			l.getErrorHandler()(w, req, err)
			return
		}
	}()

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

func (l *loadBalance) RunCheckNode() {
	for {
		ctx := context.TODO()

		for _, n := range l.Backends {
			context.WithTimeout(ctx, n.HealthCheck.Timeout)
			go l.NodeCheck(ctx, n)
		}
		time.Sleep(5 * time.Minute)
	}
}

func copyHeader(dst http.Header, src http.Header) {
	for k, v := range src {
		for _, vv := range v {
			dst.Add(k, vv)
		}
	}
}

func upgradeType(h http.Header) string {
	if !httpguts.HeaderValuesContainsToken(h["Connection"], "Upgrade") {
		return ""
	}
	return strings.ToLower(h.Get("Upgrade"))
}

func NewLoadBalancer(node []*Node) *loadBalance {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   1 * time.Second,
			KeepAlive: 10 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10000,
		IdleConnTimeout:       2 * time.Second,
		TLSHandshakeTimeout:   1 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConnsPerHost:   10000,
		MaxConnsPerHost:       -1,
	}

	return &loadBalance{
		Backends:  node,
		Transport: transport,
		Timeout:   1 * time.Second,

		roundRobinCnt: uint64(0),
		mu:            &sync.Mutex{},
	}
}

// todo
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

	h := HealthCheck{
		u,
		200,
		"<!DOCTYPE html>\n<html>\n<head>\n<title>Welcome to nginx!</title>\n<style>\n    body {\n        width: 35em;\n        margin: 0 auto;\n        font-family: Tahoma, Verdana, Arial, sans-serif;\n    }\n</style>\n</head>\n<body>\n<h1>Welcome to nginx!</h1>\n<p>If you see this page, the nginx web server is successfully installed and\nworking. Further configuration is required.</p>\n\n<p>For online documentation and support please refer to\n<a href=\"http://nginx.org/\">nginx.org</a>.<br/>\nCommercial support is available at\n<a href=\"http://nginx.com/\">nginx.com</a>.</p>\n\n<p><em>Thank you for using nginx.</em></p>\n</body>\n</html>\n",
		1 * time.Second,
	}
	var nodes = []*Node{
		NewNode(u, h),
		NewNode(u2, h),
	}

	srv := NewLoadBalancer(nodes)
	err := srv.Run()
	if err != nil {
		logf("%s", err)
	}

	err = http.ListenAndServe(":8080", srv)
	if err != nil {
		logf("%s", err)
		os.Exit(1)
	}
}
