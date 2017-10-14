// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package requester provides commands to run load tests and display results.
package requester

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/sync/semaphore"
)

const heyUA = "hey/0.0.1"

type result struct {
	err           error
	statusCode    int
	duration      time.Duration
	connStart     time.Time
	connEnd       time.Time
	connDuration  time.Duration // connection setup(DNS lookup + Dial up) duration
	dnsStart      time.Time
	dnsEnd        time.Time
	dnsDuration   time.Duration // dns lookup duration
	reqStart      time.Time
	reqEnd        time.Time
	reqDuration   time.Duration // request "write" duration
	resStart      time.Time
	resEnd        time.Time
	resDuration   time.Duration // response "read" duration
	delayStart    time.Time
	delayEnd      time.Time
	delayDuration time.Duration // delay between response and request
	contentLength int64
}

type Work struct {
	// Request is the request to be made.
	Request *http.Request

	RequestBody []byte

	// N is the total number of requests to make.
	N int

	// C is the concurrency level, the number of concurrent workers to run.
	C int

	// H2 is an option to make HTTP/2 requests
	H2 bool

	// Timeout in seconds.
	Timeout int

	// Qps is the rate limit.
	QPS int

	// DisableCompression is an option to disable compression in response
	DisableCompression bool

	// DisableKeepAlives is an option to prevents re-use of TCP connections between different HTTP requests
	DisableKeepAlives bool

	// DisableRedirects is an option to prevent the following of HTTP redirects
	DisableRedirects bool

	// Output represents the output type. If "csv" is provided, the
	// output will be dumped as a csv stream.
	Output string

	// ProxyAddr is the address of HTTP proxy server in the format on "host:port".
	// Optional.
	ProxyAddr *url.URL

	// Writer is where results will be written. If nil, results are written to stdout.
	Writer io.Writer

	results chan *result
	stopCh  chan struct{}
	start   time.Time
}

func (b *Work) writer() io.Writer {
	if b.Writer == nil {
		return os.Stdout
	}
	return b.Writer
}

// Run makes all the requests, prints the summary. It blocks until
// all work is done.
func (b *Work) Run() {
	// append hey's user agent
	ua := b.Request.UserAgent()
	if ua == "" {
		ua = heyUA
	} else {
		ua += " " + heyUA
	}

	b.results = make(chan *result, b.N)
	b.stopCh = make(chan struct{}, 1000)
	b.start = time.Now()

	b.runWorkers()
	b.Finish()
}

func (b *Work) Finish() {
	b.stopCh <- struct{}{}
	close(b.results)
	total := time.Now().Sub(b.start)
	newReport(b.writer(), b.N, b.results, b.Output, total).finalize()
}

func (b *Work) makeRequest(c *http.Client, sem *semaphore.Weighted) {
	var size int64
	var code int
	var dnsStart, dnsEnd, connStart, connEnd, resStart, resEnd, reqStart, reqEnd, delayStart, delayEnd time.Time
	var dnsDuration, connDuration, resDuration, reqDuration, delayDuration time.Duration
	req := cloneRequest(b.Request, b.RequestBody)
	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dnsStart = time.Now()
		},
		DNSDone: func(dnsInfo httptrace.DNSDoneInfo) {
			dnsEnd = time.Now()
			dnsDuration = dnsEnd.Sub(dnsStart)
		},
		GetConn: func(h string) {
			connStart = time.Now()
		},
		GotConn: func(connInfo httptrace.GotConnInfo) {
			connEnd = time.Now()
			connDuration = connEnd.Sub(connStart)
			reqStart = connEnd
		},
		WroteRequest: func(w httptrace.WroteRequestInfo) {
			reqEnd = time.Now()
			reqDuration = reqEnd.Sub(reqStart)
			delayStart = reqEnd
		},
		GotFirstResponseByte: func() {
			delayEnd = time.Now()
			delayDuration = delayEnd.Sub(delayStart)
			resStart = delayEnd
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	sem.Acquire(context.Background(), 1)

	var throttle <-chan time.Time
	if b.QPS > 0 {
		throttle = time.Tick(time.Duration(1e6/(b.QPS)) * time.Microsecond)
	}

	s := time.Now()

	resp, err := c.Do(req)

	if err == nil {
		size = resp.ContentLength
		code = resp.StatusCode
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}
	resEnd = time.Now()
	resDuration = resEnd.Sub(resStart)
	finish := resEnd.Sub(s)

	if b.QPS > 0 {
		<-throttle
	}

	sem.Release(1)

	b.results <- &result{
		statusCode:    code,
		duration:      finish,
		err:           err,
		contentLength: size,
		connStart:     connStart,
		connEnd:       connEnd,
		connDuration:  connDuration,
		dnsStart:      dnsStart,
		dnsEnd:        dnsEnd,
		dnsDuration:   dnsDuration,
		reqStart:      reqStart,
		reqEnd:        reqEnd,
		reqDuration:   reqDuration,
		resStart:      resStart,
		resEnd:        resEnd,
		resDuration:   resDuration,
		delayStart:    delayStart,
		delayEnd:      delayEnd,
		delayDuration: delayDuration,
	}
}

func (b *Work) runWorker(sem *semaphore.Weighted) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
		DisableCompression: b.DisableCompression,
		DisableKeepAlives:  b.DisableKeepAlives,
		Proxy:              http.ProxyURL(b.ProxyAddr),
	}
	if b.H2 {
		http2.ConfigureTransport(tr)
	} else {
		tr.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	}

	client := &http.Client{Transport: tr, Timeout: time.Duration(b.Timeout) * time.Second}
	if b.DisableRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	b.makeRequest(client, sem)
}

func (b *Work) runWorkers() {
	var wg sync.WaitGroup
	wg.Add(b.N)

	sem := semaphore.NewWeighted(int64(b.C))

	for i := 0; i < b.N; i++ {
		go func() {
			b.runWorker(sem)
			wg.Done()
		}()
	}

	wg.Wait()
}

// cloneRequest returns a clone of the provided *http.Request.
// The clone is a shallow copy of the struct and its Header map.
func cloneRequest(r *http.Request, body []byte) *http.Request {
	// shallow copy of the struct
	r2 := new(http.Request)
	*r2 = *r
	// deep copy of the Header
	r2.Header = make(http.Header, len(r.Header))
	for k, s := range r.Header {
		r2.Header[k] = append([]string(nil), s...)
	}
	if len(body) > 0 {
		r2.Body = ioutil.NopCloser(bytes.NewReader(body))
	}
	return r2
}
