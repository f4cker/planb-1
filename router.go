// Copyright 2015 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/golang-lru"
	"github.com/nu7hatch/gouuid"
	"github.com/tsuru/planb/backend"
)

type FixedReadCloser struct {
	value []byte
}

func (r *FixedReadCloser) Read(p []byte) (n int, err error) {
	return copy(p, r.value), io.EOF
}

func (r *FixedReadCloser) Close() error {
	return nil
}

var (
	emptyResponseBody   = &FixedReadCloser{}
	noRouteResponseBody = &FixedReadCloser{value: []byte("no such route")}
)

type requestData struct {
	backendLen int
	backend    string
	backendIdx int
	backendKey string
	host       string
	debug      bool
	startTime  time.Time
}

func (r *requestData) String() string {
	back := r.backend
	if back == "" {
		back = "?"
	}
	return r.host + " -> " + back
}

type bufferPool struct {
	syncPool sync.Pool
}

func (p *bufferPool) Get() []byte {
	b := p.syncPool.Get()
	if b == nil {
		return make([]byte, 32*1024)
	}
	return b.([]byte)
}

func (p *bufferPool) Put(b []byte) {
	p.syncPool.Put(b)
}

type Router struct {
	http.Transport
	ReadRedisHost   string
	ReadRedisPort   int
	WriteRedisHost  string
	WriteRedisPort  int
	LogPath         string
	DialTimeout     time.Duration
	RequestTimeout  time.Duration
	DeadBackendTTL  int
	FlushInterval   time.Duration
	RequestIDHeader string
	rp              *httputil.ReverseProxy
	dialer          *net.Dialer
	backend         backend.RoutesBackend
	logger          *Logger
	rrMutex         sync.RWMutex
	roundRobin      map[string]*int32
	cache           *lru.Cache
	markingDisabled bool
}

func (router *Router) Init() error {
	var err error
	if router.backend == nil {
		be, err := backend.NewRedisBackend(backend.RedisOptions{}, backend.RedisOptions{})
		if err != nil {
			return err
		}
		router.backend = be
	}
	if router.LogPath == "" {
		router.LogPath = "./access.log"
	}
	if router.logger == nil && router.LogPath != "none" {
		router.logger, err = NewFileLogger(router.LogPath)
		if err != nil {
			return err
		}
	}
	if router.DeadBackendTTL == 0 {
		router.DeadBackendTTL = 30
	}
	if router.cache == nil {
		router.cache, err = lru.New(100)
		if err != nil {
			return err
		}
	}
	router.dialer = &net.Dialer{
		Timeout:   router.DialTimeout,
		KeepAlive: 30 * time.Second,
	}
	router.Transport = http.Transport{
		Dial:                router.dialer.Dial,
		TLSHandshakeTimeout: router.DialTimeout,
		MaxIdleConnsPerHost: 100,
	}
	router.roundRobin = make(map[string]*int32)
	router.rp = &httputil.ReverseProxy{
		Director:      func(*http.Request) {},
		Transport:     router,
		FlushInterval: router.FlushInterval,
		BufferPool:    &bufferPool{},
	}
	return nil
}

func (router *Router) Stop() {
	if router.logger != nil {
		router.logger.Stop()
	}
}

type backendSet struct {
	id       string
	backends []string
	dead     map[int]struct{}
	expires  time.Time
}

func (s *backendSet) Expired() bool {
	return time.Now().After(s.expires)
}

func (router *Router) getBackends(host string) (*backendSet, error) {
	if data, ok := router.cache.Get(host); ok {
		set := data.(backendSet)
		if !set.Expired() {
			return &set, nil
		}
	}
	var set backendSet
	var err error
	set.id, set.backends, set.dead, err = router.backend.Backends(host)
	if err != nil {
		return nil, fmt.Errorf("error running routes backend commands: %s", err)
	}
	set.expires = time.Now().Add(2 * time.Second)
	router.cache.Add(host, set)
	return &set, nil
}

func (router *Router) getRequestData(req *http.Request, save bool) (*requestData, error) {
	host, _, _ := net.SplitHostPort(req.Host)
	if host == "" {
		host = req.Host
	}
	reqData := &requestData{
		debug:     req.Header.Get("X-Debug-Router") != "",
		startTime: time.Now(),
		host:      host,
	}
	req.Header.Del("X-Debug-Router")
	set, err := router.getBackends(host)
	if err != nil {
		return reqData, err
	}
	reqData.backendKey = set.id
	reqData.backendLen = len(set.backends)
	router.rrMutex.RLock()
	roundRobin := router.roundRobin[host]
	if roundRobin == nil {
		router.rrMutex.RUnlock()
		router.rrMutex.Lock()
		roundRobin = router.roundRobin[host]
		if roundRobin == nil {
			roundRobin = new(int32)
			router.roundRobin[host] = roundRobin
		}
		router.rrMutex.Unlock()
	} else {
		router.rrMutex.RUnlock()
	}
	// We always add, it will eventually overflow to zero which is fine.
	initialNumber := atomic.AddInt32(roundRobin, 1)
	initialNumber = (initialNumber - 1) % int32(reqData.backendLen)
	toUseNumber := -1
	for chosenNumber := initialNumber; ; {
		_, isDead := set.dead[int(chosenNumber)]
		if !isDead {
			toUseNumber = int(chosenNumber)
			break
		}
		chosenNumber = (chosenNumber + 1) % int32(reqData.backendLen)
		if chosenNumber == initialNumber {
			break
		}
	}
	if toUseNumber == -1 {
		return reqData, errors.New("all backends are dead")
	}
	reqData.backendIdx = toUseNumber
	reqData.backend = set.backends[toUseNumber]
	return reqData, nil
}

func (router *Router) chooseBackend(req *http.Request) *requestData {
	req.URL.Scheme = ""
	req.URL.Host = ""
	reqData, err := router.getRequestData(req, true)
	if err != nil {
		logError(reqData.String(), req.URL.Path, err)
		return reqData
	}
	url, err := url.Parse(reqData.backend)
	if err != nil {
		logError(reqData.String(), req.URL.Path, fmt.Errorf("invalid backend url: %s", err))
		return reqData
	}
	req.URL.Scheme = url.Scheme
	req.URL.Host = url.Host
	if req.URL.Host == "" {
		req.URL.Scheme = "http"
		req.URL.Host = reqData.backend
	}
	if router.RequestIDHeader != "" && req.Header.Get(router.RequestIDHeader) == "" {
		unparsedID, err := uuid.NewV4()
		if err != nil {
			logError(reqData.String(), req.URL.Path, fmt.Errorf("unable to generate request id: %s", err))
			return reqData
		}
		uniqueID := unparsedID.String()
		req.Header.Set(router.RequestIDHeader, uniqueID)
	}
	return reqData
}

func (router *Router) RoundTrip(req *http.Request) (*http.Response, error) {
	reqData := router.chooseBackend(req)
	rsp := router.RoundTripWithData(req, reqData)
	return rsp, nil
}

func (router *Router) RoundTripWithData(req *http.Request, reqData *requestData) *http.Response {
	var rsp *http.Response
	var err error
	var backendDuration time.Duration
	if req.URL.Scheme == "" || req.URL.Host == "" {
		rsp = &http.Response{
			Request:       req,
			StatusCode:    http.StatusBadRequest,
			ProtoMajor:    req.ProtoMajor,
			ProtoMinor:    req.ProtoMinor,
			ContentLength: int64(len(noRouteResponseBody.value)),
			Body:          noRouteResponseBody,
		}
	} else {
		var timedout int32
		if router.RequestTimeout > 0 {
			timer := time.AfterFunc(router.RequestTimeout, func() {
				atomic.AddInt32(&timedout, 1)
				router.Transport.CancelRequest(req)
			})
			defer timer.Stop()
		}
		host, _, _ := net.SplitHostPort(req.URL.Host)
		if host == "" {
			host = req.URL.Host
		}
		isIP := net.ParseIP(host) != nil
		if !isIP {
			req.Header.Set("X-Host", req.Host)
			req.Host = host
		}
		t0 := time.Now().UTC()
		rsp, err = router.Transport.RoundTrip(req)
		backendDuration = time.Since(t0)
		if err != nil {
			markAsDead := false
			if netErr, ok := err.(net.Error); ok {
				markAsDead = !netErr.Temporary()
			}
			isTimeout := atomic.LoadInt32(&timedout) == int32(1)
			if isTimeout {
				markAsDead = false
				err = fmt.Errorf("request timed out after %v: %s", router.RequestTimeout, err)
			} else {
				err = fmt.Errorf("error in backend request: %s", err)
			}
			if markAsDead {
				err = fmt.Errorf("%s *DEAD*", err)
			}
			logError(reqData.String(), req.URL.Path, err)
			if markAsDead && !router.markingDisabled {
				markErr := router.backend.MarkDead(reqData.host, reqData.backend, reqData.backendIdx, reqData.backendLen, router.DeadBackendTTL)
				if markErr != nil {
					logError(reqData.String(), req.URL.Path, fmt.Errorf("error markind dead backend in routes backend: %s", markErr))
				}
			}
			rsp = &http.Response{
				Request:    req,
				StatusCode: http.StatusServiceUnavailable,
				ProtoMajor: req.ProtoMajor,
				ProtoMinor: req.ProtoMinor,
				Header:     http.Header{},
				Body:       emptyResponseBody,
			}
		}
	}
	if reqData.debug {
		rsp.Header.Set("X-Debug-Backend-Url", reqData.backend)
		rsp.Header.Set("X-Debug-Backend-Id", strconv.FormatUint(uint64(reqData.backendIdx), 10))
		rsp.Header.Set("X-Debug-Frontend-Key", reqData.host)
	}
	if router.logger != nil {
		router.logger.MessageRaw(&logEntry{
			now:             time.Now(),
			req:             req,
			rsp:             rsp,
			backendDuration: backendDuration,
			totalDuration:   time.Since(reqData.startTime),
			backendKey:      reqData.backendKey,
		})
	}
	return rsp
}

func (router *Router) serveWebsocket(rw http.ResponseWriter, req *http.Request) (*requestData, error) {
	reqData, err := router.getRequestData(req, false)
	if err != nil {
		return reqData, err
	}
	url, err := url.Parse(reqData.backend)
	if err != nil {
		return reqData, err
	}
	req.Host = url.Host
	dstConn, err := router.dialer.Dial("tcp", url.Host)
	if err != nil {
		return reqData, err
	}
	defer dstConn.Close()
	hj, ok := rw.(http.Hijacker)
	if !ok {
		return reqData, errors.New("not a hijacker")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return reqData, err
	}
	defer conn.Close()
	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		if prior, ok := req.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		req.Header.Set("X-Forwarded-For", clientIP)
	}
	err = req.Write(dstConn)
	if err != nil {
		return reqData, err
	}
	errc := make(chan error, 2)
	cp := func(dst io.Writer, src io.Reader) {
		_, err := io.Copy(dst, src)
		errc <- err
	}
	go cp(dstConn, conn)
	go cp(conn, dstConn)
	<-errc
	return reqData, nil
}

func (router *Router) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Host == "__ping__" && req.URL.Path == "/" {
		rw.WriteHeader(http.StatusOK)
		rw.Write([]byte("OK"))
		return
	}
	upgrade := req.Header.Get("Upgrade")
	if upgrade != "" && strings.ToLower(upgrade) == "websocket" {
		reqData, err := router.serveWebsocket(rw, req)
		if err != nil {
			logError(reqData.String(), req.URL.Path, err)
			http.Error(rw, "", http.StatusBadGateway)
		}
		return
	}
	router.rp.ServeHTTP(rw, req)
}
