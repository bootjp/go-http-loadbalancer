package main

import (
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"
)

func init() {
	//inOurTests = true
	hopHeaders = append(hopHeaders, "fakeHopHeader")
}

func TestLBHeader(t *testing.T) {
	const backendResponse = "I am the backend"
	const backendStatus = 200
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		if len(r.TransferEncoding) > 0 {
			t.Errorf("backend got unexpected TransferEncoding: %v", r.TransferEncoding)
		}
		// todo fix add X-Forwarded-For
		//if r.Header.Get("X-Forwarded-For") == "" {
		//	t.Errorf("didn't get X-Forwarded-For header")
		//}
		if c := r.Header.Get("Connection"); c != "" {
			t.Errorf("handler got Connection header value %q", c)
		}
		// todo fix Te header value is trailers
		//if c := r.Header.Get("Te"); c != "trailers" {
		//	t.Errorf("handler got Te header value %q; want 'trailers'", c)
		//}
		if c := r.Header.Get("Upgrade"); c != "" {
			t.Errorf("handler got Upgrade header value %q", c)
		}
		if c := r.Header.Get("Proxy-Connection"); c != "" {
			t.Errorf("handler got Proxy-Connection header value %q", c)
		}
		// todo fix host header test
		//if g, e := r.Host, "some-name"; g != e {
		//	t.Errorf("backend got Host header %q, want %q", g, e)
		//}
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

	var nodes = []Node{
		{
			*u,
			true,
		},
	}

	balancer := NewLoadBalancer(nodes)
	balancer.BalanceAlg = func(req *http.Request) {
		pick := balancer.Backends[balancer.req]

		if balancer.req >= uint32(len(balancer.Backends)-1) {
			atomic.StoreUint32(&balancer.req, 0)
		} else {
			atomic.AddUint32(&balancer.req, 1)
		}
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

	frontend := httptest.NewServer(balancer)
	defer frontend.Close()
	frontendClient := frontend.Client()

	getReq, _ := http.NewRequest("GET", frontend.URL, nil)
	getReq.Host = "some-name"
	getReq.Header.Set("Connection", "close")
	getReq.Header.Set("Te", "trailers")
	getReq.Header.Set("Proxy-Connection", "should be deleted")
	getReq.Header.Set("Upgrade", "foo")
	getReq.Close = true
	res, err := frontendClient.Do(getReq)
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
	// todo fix Te header value is trailers
	//if g, e := res.Header.Get("Trailers"), "not a special header field name"; g != e {
	//	t.Errorf("header Trailers = %q; want %q", g, e)
	//}
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

	getReq, _ = http.NewRequest("GET", frontend.URL, nil)
	getReq.Close = true
	res, err = frontendClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("response given %v; want 200 StatusOK", res.Status)
	}
}

func TestLBNodeCrash(t *testing.T) {
	//const backendResponse = "I am the backend"
	//const backendStatus = 502
	//backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	//	//if r.Method == "GET" && r.FormValue("mode") == "hangup" {
	//	//	c, _, _ := w.(http.Hijacker).Hijack()
	//	//	c.Close()
	//	//	return
	//	//}
	//	//if len(r.TransferEncoding) > 0 {
	//	//	t.Errorf("backend got unexpected TransferEncoding: %v", r.TransferEncoding)
	//	//}
	//	////if r.Header.Get("X-Forwarded-For") == "" {
	//	////	t.Errorf("didn't get X-Forwarded-For header")
	//	////}
	//	//if c := r.Header.Get("Connection"); c != "" {
	//	//	t.Errorf("handler got Connection header value %q", c)
	//	//}
	//	////if c := r.Header.Get("Te"); c != "trailers" {
	//	////	t.Errorf("handler got Te header value %q; want 'trailers'", c)
	//	////}
	//	//if c := r.Header.Get("Upgrade"); c != "" {
	//	//	t.Errorf("handler got Upgrade header value %q", c)
	//	//}
	//	////if c := r.Header.Get("Proxy-Connection"); c != "" {
	//	////	t.Errorf("handler got Proxy-Connection header value %q", c)
	//	////}
	//	//if g, e := r.Host, "some-name"; g != e {
	//	//	t.Errorf("backend got Host header %q, want %q", g, e)
	//	//}
	//	w.Header().Set("Trailers", "not a special header field name")
	//	w.Header().Set("Trailer", "X-Trailer")
	//	w.Header().Set("X-Foo", "bar")
	//	w.Header().Set("Upgrade", "foo")
	//	w.Header().Set("fakeHopHeader", "foo")
	//	w.Header().Add("X-Multi-Value", "foo")
	//	w.Header().Add("X-Multi-Value", "bar")
	//	http.SetCookie(w, &http.Cookie{Name: "flavor", Value: "chocolateChip"})
	//	w.WriteHeader(backendStatus)
	//	w.Write([]byte(backendResponse))
	//	w.Header().Set("X-Trailer", "trailer_value")
	//	w.Header().Set(http.TrailerPrefix+"X-Unannounced-Trailer", "unannounced_trailer_value")
	//}))
	//defer backend.Close()
	//fmt.Println(backend.URL)
	//u, _ := url.Parse(backend.URL)
	//fmt.Println(u)
	//
	//var nodes = []Node{
	//	{
	//		*u,
	//		true,
	//	},
	//}
	//
	//balancer := NewLoadBalancer(nodes)
	//balancer.BalanceAlg = func(req *http.Request) {
	//	pick := balancer.Backends[balancer.req]
	//
	//	if balancer.req >= uint32(len(balancer.Backends)-1) {
	//		atomic.StoreUint32(&balancer.req, 0)
	//	} else {
	//		atomic.AddUint32(&balancer.req, 1)
	//	}
	//	targetQuery := pick.RawQuery
	//	req.URL.Scheme = pick.Scheme
	//	req.URL.Host = pick.Host
	//	req.URL.Path = pick.Path + req.URL.Path
	//	if targetQuery == "" || req.URL.RawQuery == "" {
	//		req.URL.RawQuery = targetQuery + req.URL.RawQuery
	//	} else {
	//		req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
	//	}
	//	fmt.Println(req.URL)
	//	if _, ok := req.Header["User-Agent"]; !ok {
	//		req.Header.Set("User-Agent", "")
	//	}
	//}
	//
	//frontend := httptest.NewServer(balancer)
	//defer frontend.Close()
	//frontendClient := frontend.Client()
	//
	//getReq, _ := http.NewRequest("GET", frontend.URL, nil)
	//getReq.Host = "some-name"
	//getReq.Header.Set("Connection", "close")
	//getReq.Header.Set("Te", "trailers")
	//getReq.Header.Set("Proxy-Connection", "should be deleted")
	//getReq.Header.Set("Upgrade", "foo")
	//getReq.Close = true
	//res, err := frontendClient.Do(getReq)
	//if err != nil {
	//	t.Fatalf("Get: %v", err)
	//}
	//if g, e := res.StatusCode, backendStatus; g != e {
	//	t.Errorf("got res.StatusCode %d; expected %d", g, e)
	//}
	//if g, e := res.Header.Get("X-Foo"), "bar"; g != e {
	//	t.Errorf("got X-Foo %q; expected %q", g, e)
	//}
	//if c := res.Header.Get("fakeHopHeader"); c != "" {
	//	t.Errorf("got %s header value %q", "fakeHopHeader", c)
	//}
	////if g, e := res.Header.Get("Trailers"), "not a special header field name"; g != e {
	////	t.Errorf("header Trailers = %q; want %q", g, e)
	////}
	//if g, e := len(res.Header["X-Multi-Value"]), 2; g != e {
	//	t.Errorf("got %d X-Multi-Value header values; expected %d", g, e)
	//}
	//if g, e := len(res.Header["Set-Cookie"]), 1; g != e {
	//	t.Fatalf("got %d SetCookies, want %d", g, e)
	//}
	//if g, e := res.Trailer, (http.Header{"X-Trailer": nil}); !reflect.DeepEqual(g, e) {
	//	t.Errorf("before reading body, Trailer = %#v; want %#v", g, e)
	//}
	//if cookie := res.Cookies()[0]; cookie.Name != "flavor" {
	//	t.Errorf("unexpected cookie %q", cookie.Name)
	//}
	//bodyBytes, _ := ioutil.ReadAll(res.Body)
	//if g, e := string(bodyBytes), backendResponse; g != e {
	//	t.Errorf("got body %q; expected %q", g, e)
	//}
	//if g, e := res.Trailer.Get("X-Trailer"), "trailer_value"; g != e {
	//	t.Errorf("Trailer(X-Trailer) = %q ; want %q", g, e)
	//}
	//if g, e := res.Trailer.Get("X-Unannounced-Trailer"), "unannounced_trailer_value"; g != e {
	//	t.Errorf("Trailer(X-Unannounced-Trailer) = %q ; want %q", g, e)
	//}
	//
	//// Test that a backend failing to be reached or one which doesn't return
	//// a response results in a StatusBadGateway.
	//getReq, _ = http.NewRequest("GET", frontend.URL+"/?mode=hangup", nil)
	//getReq.Close = true
	//res, err = frontendClient.Do(getReq)
	//if err != nil {
	//	t.Fatal(err)
	//}
	//res.Body.Close()
	//if res.StatusCode != http.StatusBadGateway {
	//	t.Errorf("request to bad proxy = %v; want 502 StatusBadGateway", res.Status)
	//}
	//time.Sleep(1 * time.Second)
}

// Issue 16875: remove any proxied headers mentioned in the "Connection"
// header value.
//func TestReverseProxyStripHeadersPresentInConnection(t *testing.T) {
//	const fakeConnectionToken = "X-Fake-Connection-Token"
//	const backendResponse = "I am the backend"
//
//	// someConnHeader is some arbitrary header to be declared as a hop-by-hop header
//	// in the Request's Connection header.
//	const someConnHeader = "X-Some-Conn-Header"
//
//	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//		if c := r.Header.Get("Connection"); c != "" {
//			t.Errorf("handler got header %q = %q; want empty", "Connection", c)
//		}
//		if c := r.Header.Get(fakeConnectionToken); c != "" {
//			t.Errorf("handler got header %q = %q; want empty", fakeConnectionToken, c)
//		}
//		if c := r.Header.Get(someConnHeader); c != "" {
//			t.Errorf("handler got header %q = %q; want empty", someConnHeader, c)
//		}
//		w.Header().Add("Connection", "Upgrade, "+fakeConnectionToken)
//		w.Header().Add("Connection", someConnHeader)
//		w.Header().Set(someConnHeader, "should be deleted")
//		w.Header().Set(fakeConnectionToken, "should be deleted")
//		io.WriteString(w, backendResponse)
//	}))
//	defer backend.Close()
//	backendURL, err := url.Parse(backend.URL)
//	if err != nil {
//		t.Fatal(err)
//	}
//	proxyHandler := NewSingleHostReverseProxy(backendURL)
//	frontend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//		proxyHandler.ServeHTTP(w, r)
//		if c := r.Header.Get(someConnHeader); c != "should be deleted" {
//			t.Errorf("handler modified header %q = %q; want %q", someConnHeader, c, "should be deleted")
//		}
//		if c := r.Header.Get(fakeConnectionToken); c != "should be deleted" {
//			t.Errorf("handler modified header %q = %q; want %q", fakeConnectionToken, c, "should be deleted")
//		}
//		c := r.Header["Connection"]
//		var cf []string
//		for _, f := range c {
//			for _, sf := range strings.Split(f, ",") {
//				if sf = strings.TrimSpace(sf); sf != "" {
//					cf = append(cf, sf)
//				}
//			}
//		}
//		sort.Strings(cf)
//		expectedValues := []string{"Upgrade", someConnHeader, fakeConnectionToken}
//		sort.Strings(expectedValues)
//		if !reflect.DeepEqual(cf, expectedValues) {
//			t.Errorf("handler modified header %q = %q; want %q", "Connection", cf, expectedValues)
//		}
//	}))
//	defer frontend.Close()
//
//	getReq, _ := http.NewRequest("GET", frontend.URL, nil)
//	getReq.Header.Add("Connection", "Upgrade, "+fakeConnectionToken)
//	getReq.Header.Add("Connection", someConnHeader)
//	getReq.Header.Set(someConnHeader, "should be deleted")
//	getReq.Header.Set(fakeConnectionToken, "should be deleted")
//	res, err := frontend.Client().Do(getReq)
//	if err != nil {
//		t.Fatalf("Get: %v", err)
//	}
//	defer res.Body.Close()
//	bodyBytes, err := ioutil.ReadAll(res.Body)
//	if err != nil {
//		t.Fatalf("reading body: %v", err)
//	}
//	if got, want := string(bodyBytes), backendResponse; got != want {
//		t.Errorf("got body %q; want %q", got, want)
//	}
//	if c := res.Header.Get("Connection"); c != "" {
//		t.Errorf("handler got header %q = %q; want empty", "Connection", c)
//	}
//	if c := res.Header.Get(someConnHeader); c != "" {
//		t.Errorf("handler got header %q = %q; want empty", someConnHeader, c)
//	}
//	if c := res.Header.Get(fakeConnectionToken); c != "" {
//		t.Errorf("handler got header %q = %q; want empty", fakeConnectionToken, c)
//	}
//}
//
//func TestXForwardedFor(t *testing.T) {
//	const prevForwardedFor = "client ip"
//	const backendResponse = "I am the backend"
//	const backendStatus = 404
//	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//		if r.Header.Get("X-Forwarded-For") == "" {
//			t.Errorf("didn't get X-Forwarded-For header")
//		}
//		if !strings.Contains(r.Header.Get("X-Forwarded-For"), prevForwardedFor) {
//			t.Errorf("X-Forwarded-For didn't contain prior data")
//		}
//		w.WriteHeader(backendStatus)
//		w.Write([]byte(backendResponse))
//	}))
//	defer backend.Close()
//	backendURL, err := url.Parse(backend.URL)
//	if err != nil {
//		t.Fatal(err)
//	}
//	proxyHandler := NewSingleHostReverseProxy(backendURL)
//	frontend := httptest.NewServer(proxyHandler)
//	defer frontend.Close()
//
//	getReq, _ := http.NewRequest("GET", frontend.URL, nil)
//	getReq.Host = "some-name"
//	getReq.Header.Set("Connection", "close")
//	getReq.Header.Set("X-Forwarded-For", prevForwardedFor)
//	getReq.Close = true
//	res, err := frontend.Client().Do(getReq)
//	if err != nil {
//		t.Fatalf("Get: %v", err)
//	}
//	if g, e := res.StatusCode, backendStatus; g != e {
//		t.Errorf("got res.StatusCode %d; expected %d", g, e)
//	}
//	bodyBytes, _ := ioutil.ReadAll(res.Body)
//	if g, e := string(bodyBytes), backendResponse; g != e {
//		t.Errorf("got body %q; expected %q", g, e)
//	}
//}
//
//// Issue 38079: don't append to X-Forwarded-For if it's present but nil
//func TestXForwardedFor_Omit(t *testing.T) {
//	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//		if v := r.Header.Get("X-Forwarded-For"); v != "" {
//			t.Errorf("got X-Forwarded-For header: %q", v)
//		}
//		w.Write([]byte("hi"))
//	}))
//	defer backend.Close()
//	backendURL, err := url.Parse(backend.URL)
//	if err != nil {
//		t.Fatal(err)
//	}
//	proxyHandler := NewSingleHostReverseProxy(backendURL)
//	frontend := httptest.NewServer(proxyHandler)
//	defer frontend.Close()
//
//	oldDirector := proxyHandler.Director
//	proxyHandler.Director = func(r *http.Request) {
//		r.Header["X-Forwarded-For"] = nil
//		oldDirector(r)
//	}
//
//	getReq, _ := http.NewRequest("GET", frontend.URL, nil)
//	getReq.Host = "some-name"
//	getReq.Close = true
//	res, err := frontend.Client().Do(getReq)
//	if err != nil {
//		t.Fatalf("Get: %v", err)
//	}
//	res.Body.Close()
//}
