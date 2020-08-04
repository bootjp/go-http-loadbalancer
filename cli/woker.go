package main

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpguts"
)

type HealthCheck struct {
	Endpoint           url.URL
	ExpectedStatusCode int
	ExpectedResponse   string
	Timeout time.Duration
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


func(n*Node) IsAlive() bool{
	return n.Alive
}


type BalanceAlg = func(req *http.Request)
type NodeCheckFunc = func(ctx context.Context, node *Node)

type loadBalancer struct {
	Backends       []*Node
	Transport      http.RoundTripper
	BalanceAlg     BalanceAlg
	CheckNodeAlive NodeCheckFunc
	Timeout time.Duration

	roundRobinCnt uint32 // this lb is max backend nodes 4294967295.
}

var hopHeaders = []string{
	"connection",
	"keep-alive",
	"proxy-authenticate",
	"proxy-authorization",
	"te",
	"trailers",
	"transfer-encoding",
	"upgrade",
	"Proxy-Connection",
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
	if req.ContentLength == 0 {
		outreq.Body = nil // Issue 16036: nil Body for http.Transport retries
	}
	if outreq.Header == nil {
		outreq.Header = make(http.Header) // Issue 33142: historical behavior was to always allocate
	}
	l.BalanceAlg(outreq)
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

	res, err := transport.RoundTrip(outreq)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		//w.Write(http.ba)
		log.Println(err)
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
		log.Println(err)
	}
	res.Body.Close()

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

func (l *loadBalancer) RunCheckNode() {
	for {
		ctx := context.TODO()

		for _, n := range l.Backends {
			context.WithTimeout(ctx, time.Second*10)
			go l.CheckNodeAlive(ctx, n)
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

func NewLoadBalancer(node []*Node) *loadBalancer {
	return &loadBalancer{
		Backends:      node,
		Transport:     nil,
		roundRobinCnt: uint32(0),
		Timeout: 10 * time.Second,
	}
}

// todo
// error handling
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

	h:= HealthCheck{
		u,
		200,
		"<!DOCTYPE html>\n<html>\n<head>\n<title>Welcome to nginx!</title>\n<style>\n    body {\n        width: 35em;\n        margin: 0 auto;\n        font-family: Tahoma, Verdana, Arial, sans-serif;\n    }\n</style>\n</head>\n<body>\n<h1>Welcome to nginx!</h1>\n<p>If you see this page, the nginx web server is successfully installed and\nworking. Further configuration is required.</p>\n\n<p>For online documentation and support please refer to\n<a href=\"http://nginx.org/\">nginx.org</a>.<br/>\nCommercial support is available at\n<a href=\"http://nginx.com/\">nginx.com</a>.</p>\n\n<p><em>Thank you for using nginx.</em></p>\n</body>\n</html>\n",
		1  * time.Second,
	}
	var nodes = []*Node{
		NewNode(u,h),
		NewNode(u2,h),

	}

	l := NewLoadBalancer(nodes)
	//l.aa = func() {
	//
	//}

	l.BalanceAlg = func(req *http.Request) {
		ctx :=req.Context()
		ctx, cancel := context.WithTimeout(ctx, l.Timeout)
		defer cancel()

		pick := l.Backends[l.roundRobinCnt]
		if l.roundRobinCnt >= uint32(len(l.Backends)-1) {
			atomic.StoreUint32(&l.roundRobinCnt, 0)
		} else {
			atomic.AddUint32(&l.roundRobinCnt, 1)
		}
		targetQuery := pick.RawQuery
		req.URL.Scheme = pick.Scheme
		req.Header.Add("Host", req.URL.Host)
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

	l.CheckNodeAlive = func(ctx context.Context, node *Node) {

		fmt.Printf("check alive node  %v\n", node)
		select {
		case <-ctx.Done():
			node.lock.Lock()
			node.Alive = false
			node.lock.Unlock()
		default:
			req, err := http.NewRequestWithContext(
				ctx,
				"GET",
				node.HealthCheck.Endpoint.String(),
				nil,
			)
			if err != nil {
				log.Println(err)
				fmt.Printf("not alive node  %v\n", node)
				node.lock.Lock()
				node.Alive = false
				node.lock.Unlock()
				return
			}
			client := &http.Client{}
			res, err := client.Do(req)
			if err != nil {
				node.lock.Lock()
				node.Alive = false
				node.lock.Unlock()
				fmt.Printf("not alive node  %v\n", node)
				log.Println(err)
				return
			}
			defer func() {
				err = res.Body.Close()
			}()
			if res.StatusCode != node.HealthCheck.ExpectedStatusCode {
				fmt.Printf("not alive node  %v reason %s \n", node, "UnExpectedStatusCode")
				node.lock.Lock()
				node.Alive = false
				node.lock.Unlock()
				return
			}
			b, _ := ioutil.ReadAll(res.Body)
			if string(b) != node.HealthCheck.ExpectedResponse {
				fmt.Printf("not alive node  %v reason %s \n", node, "UnExpectedResponse")
				node.lock.Lock()
				node.Alive = false
				node.lock.Unlock()
				return
			}

			node.lock.Lock()
			node.Alive = true
			node.lock.Unlock()
			fmt.Printf("alive node %v\n", node)
			return
		}
	}

	go l.RunCheckNode()

	err := http.ListenAndServe(":8080", l)
	if err != nil {
		log.Fatalln(err)
	}
}
