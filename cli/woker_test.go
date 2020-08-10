package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"
)

func init() {
	hopHeaders = append(hopHeaders, "fakeHopHeader")
}

func TestLBHeader(t *testing.T) {
	const backendResponse = "I am the backend"
	const backendStatus = 200
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if len(r.TransferEncoding) > 0 {
			t.Errorf("backend got unexpected TransferEncoding: %v", r.TransferEncoding)
		}
		if r.Header.Get("X-Forwarded-For") != "127.0.0.1, 127.0.0.1" {
			t.Errorf("X-Forwarded-For header expect 127.0.0.1, 127.0.0.1 given %v", r.Header.Get("X-Forwarded-For"))
		}

		if c := r.Header.Get("Connection"); c != "" {
			t.Errorf("handler got Connection header value %q", c)
		}

		if c := r.Header.Get("Te"); c != "trailers" {
			t.Errorf("handler got Te header value %q; want 'trailers'", c)
		}
		if c := r.Header.Get("Upgrade"); c != "" {
			t.Errorf("handler got Upgrade header value %q", c)
		}
		if c := r.Header.Get("Proxy-Connection"); c != "" {
			t.Errorf("handler got Proxy-Connection header value %q", c)
		}

		if g, e := r.Host, "some-name"; g != "some-name" {
			t.Errorf("backend got Host header %q, want %q", g, e)
		}
		w.Header().Set("Trailers", "not a special header field name")
		w.Header().Set("Trailer", "X-Trailer")
		w.Header().Set("X-Foo", "bar")
		w.Header().Set("Upgrade", "foo")
		w.Header().Set("fakeHopHeader", "foo")
		w.Header().Add("X-Multi-Value", "foo")
		w.Header().Add("X-Multi-Value", "bar")
		http.SetCookie(w, &http.Cookie{Name: "flavor", Value: "chocolateChip"})
		w.WriteHeader(backendStatus)
		w.Write([]byte(backendResponse))
		w.Header().Set("X-Trailer", "trailer_value")
		w.Header().Set(http.TrailerPrefix+"X-Unannounced-Trailer", "unannounced_trailer_value")
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)

	h := HealthCheck{
		*u,
		200,
		backendResponse,
		1 * time.Second,
	}
	var nodes = []*Node{
		NewNode(*u, h),
	}
	nodes[0].Alive = true

	balancer := NewLoadBalancer(nodes)
	frontend := httptest.NewServer(balancer)
	defer frontend.Close()
	frontendClient := frontend.Client()

	getReq, err := http.NewRequest("GET", frontend.URL, nil)
	if err != nil {
		t.Error(err)
	}
	getReq.Host = "some-name"
	getReq.Header.Set("Connection", "close")
	getReq.Header.Set("Te", "trailers")
	getReq.Header.Set("X-Forwarded-For", "127.0.0.1")
	getReq.Header.Set("Proxy-Connection", "should be deleted")
	getReq.Header.Set("Upgrade", "foo")
	getReq.Close = true
	res, err := frontendClient.Do(getReq)

	defer res.Body.Close()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if g, e := res.StatusCode, backendStatus; g != e {
		t.Errorf("got res.StatusCode %d; expected %d", g, e)
	}

	if g, e := res.Header.Get("X-Foo"), "bar"; g != e {
		t.Errorf("got X-Foo %q; expected %q", g, e)
	}

	if c := res.Header.Get("fakeHopHeader"); c != "" {
		t.Errorf("got %s header value %q", "fakeHopHeader", c)
	}

	if g, e := res.Header.Get("Trailers"), "not a special header field name"; g != e {
		t.Errorf("header Trailers = %q; want %q", g, e)
	}

	if g, e := len(res.Header["X-Multi-Value"]), 2; g != e {
		t.Errorf("got %d X-Multi-Value header values; expected %d", g, e)
	}

	if g, e := len(res.Header["Set-Cookie"]), 1; g != e {
		t.Fatalf("got %d SetCookies, want %d", g, e)
	}

	if g, e := res.Trailer, (http.Header{"X-Trailer": nil}); !reflect.DeepEqual(g, e) {
		t.Errorf("before reading body, Trailer = %#v; want %#v", g, e)
	}

	if cookie := res.Cookies()[0]; cookie.Name != "flavor" {
		t.Errorf("unexpected cookie %q", cookie.Name)
	}
	bodyBytes, _ := ioutil.ReadAll(res.Body)

	if g, e := string(bodyBytes), backendResponse; g != e {
		t.Errorf("got body %q; expected %q", g, e)
	}

	if g, e := res.Trailer.Get("X-Trailer"), "trailer_value"; g != e {
		t.Errorf("Trailer(X-Trailer) = %q ; want %q", g, e)
	}

	if g, e := res.Trailer.Get("X-Unannounced-Trailer"), "unannounced_trailer_value"; g != e {
		t.Errorf("Trailer(X-Unannounced-Trailer) = %q ; want %q", g, e)
	}

	if res.StatusCode != http.StatusOK {
		t.Errorf("response given %v; want 200 StatusOK", res.Status)
	}

}

func TestLBNodeCrash(t *testing.T) {
	const backendResponse = "I am the backend"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.FormValue("mode") == "hangup" {
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
			return
		}
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)

	h := HealthCheck{
		*u,
		200,
		backendResponse,
		1 * time.Second,
	}
	var nodes = []*Node{
		NewNode(*u, h),
	}
	nodes[0].Alive = true
	balancer := NewLoadBalancer(nodes)
	frontend := httptest.NewServer(balancer)
	defer frontend.Close()
	frontendClient := frontend.Client()

	getReq, _ := http.NewRequest("GET", frontend.URL+"/?mode=hangup", nil)
	getReq.Close = true
	res, err := frontendClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("request to bad proxy = %v; want 502 StatusBadGateway", res.Status)
	}
}

func TestLBNodeTimeout(t *testing.T) {
	const backendResponse = "I am the backend"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.FormValue("mode") == "hangup" {
			c, _, _ := w.(http.Hijacker).Hijack()
			time.Sleep(15 * time.Second)
			c.Close()
			return
		}
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)

	h := HealthCheck{
		*u,
		200,
		backendResponse,
		1 * time.Second,
	}
	var nodes = []*Node{
		NewNode(*u, h),
	}
	nodes[0].Alive = true

	balancer := NewLoadBalancer(nodes)
	balancer.Timeout = 2 * time.Second
	frontend := httptest.NewServer(balancer)
	defer frontend.Close()
	frontendClient := frontend.Client()

	getReq, _ := http.NewRequest("GET", frontend.URL+"/?mode=hangup", nil)
	getReq.Close = true
	res, err := frontendClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	// todo change to gateway timeout
	fmt.Println(res.StatusCode)
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("request to bad proxy = %v; want 504 StatusGatewayTimeout", res.Status)
	}
}

func TestLBNodePick(t *testing.T) {
	const backendResponse = "I am the backend"

	u, _ := url.Parse("http://localhost/aa")
	u2, _ := url.Parse("http://localhost/bb")

	h := HealthCheck{
		*u,
		200,
		backendResponse,
		1 * time.Second,
	}

	var nodes = []*Node{
		NewNode(*u, h),
		NewNode(*u2, h),
	}
	nodes[0].Alive = true
	nodes[1].Alive = true

	balancer := NewLoadBalancer(nodes)
	nodes = balancer.pickAliveNodes()

	cnt := map[string]int{}
	for i := 0; i < 10000; i++ {
		res := balancer.selectNodeByRR(nodes)
		cnt[res.String()] += 1
	}

	if cnt[u.String()] != cnt[u2.String()] {
		t.Errorf("round robin result miss match map 0 %v 1 %v", cnt[u.String()], cnt[u2.String()])
	}

}

func TestNodePickAsync(t *testing.T) {
	u, _ := url.Parse("http://localhost/aa")
	u2, _ := url.Parse("http://localhost/bb")

	h := HealthCheck{
		*u,
		200,
		"backendResponse",
		1 * time.Second,
	}

	var nodes = []*Node{
		NewNode(*u, h),
		NewNode(*u2, h),
	}
	nodes[0].Alive = true
	nodes[1].Alive = true

	balancer := NewLoadBalancer(nodes)
	nodes = balancer.pickAliveNodes()

	wg := &sync.WaitGroup{}
	mu := &sync.Mutex{}
	res := map[string]int{}
	for i := 0; i < 100000; i++ {
		wg.Add(1)
		go func() {
			rr := balancer.selectNodeByRR(nodes)
			mu.Lock()
			res[rr.String()] += 1
			mu.Unlock()
			wg.Done()
		}()

	}

	wg.Wait()
	for k, v := range res {
		fmt.Println(k, v)
	}

}

func TestLBNodePickDown(t *testing.T) {
	const backendResponse = "I am the backend"

	u, _ := url.Parse("http://localhost/aa")
	u2, _ := url.Parse("http://localhost/bb")

	h := HealthCheck{
		*u,
		200,
		backendResponse,
		1 * time.Second,
	}

	var nodes = []*Node{
		NewNode(*u, h),
		NewNode(*u2, h),
	}
	nodes[0].Alive = false
	nodes[1].Alive = true

	balancer := NewLoadBalancer(nodes)
	nodes = balancer.pickAliveNodes()

	cnt := map[string]int{}
	for i := 0; i < 10000; i++ {
		res := balancer.selectNodeByRR(nodes)
		cnt[res.String()] += 1
	}

	if cnt[u2.String()] != 10000 {
		t.Errorf("round robin result miss match expect u2 10000 given %d", cnt[u2.String()])
	}

}

func TestLBNodePickAfterDown(t *testing.T) {
	const backendResponse = "I am the backend"

	u, _ := url.Parse("http://localhost/aa")
	u2, _ := url.Parse("http://localhost/bb")

	h := HealthCheck{
		*u,
		200,
		backendResponse,
		1 * time.Second,
	}

	var nodes = []*Node{
		NewNode(*u, h),
		NewNode(*u2, h),
	}

	nodes[0].Alive = true
	nodes[1].Alive = true

	balancer := NewLoadBalancer(nodes)

	cnt := map[string]int{}

	for i := 0; i < 10000; i++ {

		if i == 1000 {
			nodes[1].Alive = false
		}
		if i == 9000 {
			nodes[0].Alive = false
			nodes[1].Alive = true
		}
		aliveNodes := balancer.pickAliveNodes()
		res := balancer.selectNodeByRR(aliveNodes)
		cnt[res.String()] += 1
	}

	if cnt[u2.String()] != 1500 {
		t.Errorf("round robin result miss match expect u2 500 given %d", cnt[u2.String()])
	}
	if cnt[u.String()] != 8500 {
		t.Errorf("round robin result miss match expect u1 9500 given %d", cnt[u.String()])
	}

}

//func TestLBNodePickAfterDownAsync(t *testing.T) {
//	const backendResponse = "I am the backend"
//
//	u, _ := url.Parse("http://localhost/aa")
//	u2, _ := url.Parse("http://localhost/bb")
//
//	h := HealthCheck{
//		*u,
//		200,
//		backendResponse,
//		1 * time.Second,
//	}
//
//	var nodes = []*Node{
//		NewNode(*u, h),
//		NewNode(*u2, h),
//	}
//
//	nodes[0].Alive = true
//	nodes[1].Alive = true
//
//	balancer := NewLoadBalancer(nodes)
//
//	wg := &sync.WaitGroup{}
//	mu := &sync.Mutex{}
//	res := map[string]int{}
//	for i := uint32(0); atomic.LoadUint32(&i) < 10000; atomic.AddUint32(&i, 1) {
//
//		wg.Add(1)
//		go func(i *uint32) {
//
//			//if atomic.LoadUint32(i) == 1000 {
//			//	nodes[1].StatusAlive(false)
//			//}
//			//if atomic.LoadUint32(i) == 9000 {
//			//	nodes[0].StatusAlive(false)
//			//	nodes[1].StatusAlive(true)
//			//}
//
//			aliveNodes := balancer.pickAliveNodes()
//
//			rr := balancer.selectNodeByRR(aliveNodes)
//			mu.Lock()
//			res[rr.String()] += 1
//			mu.Unlock()
//			wg.Done()
//		}(&i)
//
//	}
//
//	wg.Wait()
//	//if res[u2.String()] != 1500 {
//	//	t.Errorf("round robin result miss match expect u2 1500 given %d", res[u2.String()])
//	//}
//	//if res[u.String()] != 8500 {
//	//	t.Errorf("round robin result miss match expect u1 8500 given %d", res[u.String()])
//	//}
//
//}

//func TestF(t *testing.T) {
//	target := []int{1}
//	println(len(target))
//
//}
