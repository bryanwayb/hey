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

package requester

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	barChar = "∎"
)

type report struct {
	avgTotal float64
	fastest  float64
	slowest  float64
	average  float64
	rps      float64

	avgConn       float64
	avgDNS        float64
	avgReq        float64
	avgRes        float64
	avgDelay      float64
	connSLats     []int64
	connELats     []int64
	connLats      []float64
	dnsSLats      []int64
	dnsELats      []int64
	dnsLats       []float64
	reqSLats      []int64
	reqELats      []int64
	reqLats       []float64
	resSLats      []int64
	resELats      []int64
	resLats       []float64
	delaySLats    []int64
	delayELats    []int64
	delayLats     []float64

	results chan *result
	total   time.Duration

	errorDist      map[string]int
	statusCodeDist map[int]int
	lats           []float64
	sizeTotal      int64

	output string

	w io.Writer
}

func newReport(w io.Writer, size int, results chan *result, output string, total time.Duration) *report {
	return &report{
		output:         output,
		results:        results,
		total:          total,
		statusCodeDist: make(map[int]int),
		errorDist:      make(map[string]int),
		w:              w,
	}
}

func (r *report) finalize() {
	for res := range r.results {
		if res.err != nil {
			r.errorDist[res.err.Error()]++
		} else {
			r.lats = append(r.lats, res.duration.Seconds())
			r.avgTotal += res.duration.Seconds()
			r.avgConn += res.connDuration.Seconds()
			r.avgDelay += res.delayDuration.Seconds()
			r.avgDNS += res.dnsDuration.Seconds()
			r.avgReq += res.reqDuration.Seconds()
			r.avgRes += res.resDuration.Seconds()
			r.connSLats = append(r.connSLats, res.connEnd.UnixNano())
			r.connELats = append(r.connELats, res.connEnd.UnixNano())
			r.connLats = append(r.connLats, res.connDuration.Seconds())
			r.dnsSLats = append(r.dnsSLats, res.dnsStart.UnixNano())
			r.dnsELats = append(r.dnsELats, res.dnsEnd.UnixNano())
			r.dnsLats = append(r.dnsLats, res.dnsDuration.Seconds())
			r.reqSLats = append(r.reqSLats, res.reqStart.UnixNano())
			r.reqELats = append(r.reqELats, res.reqEnd.UnixNano())
			r.reqLats = append(r.reqLats, res.reqDuration.Seconds())
			r.delaySLats = append(r.delaySLats, res.delayStart.UnixNano())
			r.delayELats = append(r.delayELats, res.delayEnd.UnixNano())
			r.delayLats = append(r.delayLats, res.delayDuration.Seconds())
			r.resSLats = append(r.resSLats, res.resStart.UnixNano())
			r.resELats = append(r.resELats, res.resStart.UnixNano())
			r.resLats = append(r.resLats, res.resDuration.Seconds())
			r.statusCodeDist[res.statusCode]++
			if res.contentLength > 0 {
				r.sizeTotal += res.contentLength
			}
		}
	}
	r.rps = float64(len(r.lats)) / r.total.Seconds()
	r.average = r.avgTotal / float64(len(r.lats))
	r.avgConn = r.avgConn / float64(len(r.lats))
	r.avgDelay = r.avgDelay / float64(len(r.lats))
	r.avgDNS = r.avgDNS / float64(len(r.lats))
	r.avgReq = r.avgReq / float64(len(r.lats))
	r.avgRes = r.avgRes / float64(len(r.lats))
	r.print()
}

func (r *report) printCSV() {
	r.printf("response-time,DNS+dialup,DNS+dialup-start,DNS+dialup-end,DNS,DNS-start,DNS-end,Request-write,Request-write-start,Request-write-end,Response-delay,Response-delay-start,Response-delay-end,Response-read,Response-read-start,Response-read-end\n")
	for i, val := range r.lats {
		r.printf("%4.4f,%4.4f,%d,%d,%4.4f,%d,%d,%4.4f,%d,%d,%4.4f,%d,%d,%4.4f,%d,%d\n",
			val, r.connLats[i], r.connSLats[i], r.connELats[i], r.dnsLats[i], r.dnsSLats[i], r.dnsELats[i], r.reqLats[i], r.reqSLats[i], r.reqELats[i], r.delayLats[i], r.delaySLats[i], r.delayELats[i], r.resLats[i], r.resSLats[i], r.resELats[i])
	}
}

func (r *report) print() {
	if r.output == "csv" {
		r.printCSV()
		return
	}

	if len(r.lats) > 0 {
		sort.Float64s(r.lats)
		r.fastest = r.lats[0]
		r.slowest = r.lats[len(r.lats)-1]
		r.printf("Summary:\n")
		r.printf("  Total:\t%4.4f secs\n", r.total.Seconds())
		r.printf("  Slowest:\t%4.4f secs\n", r.slowest)
		r.printf("  Fastest:\t%4.4f secs\n", r.fastest)
		r.printf("  Average:\t%4.4f secs\n", r.average)
		r.printf("  Requests/sec:\t%4.4f\n", r.rps)
		if r.sizeTotal > 0 {
			r.printf("  Total data:\t%d bytes\n", r.sizeTotal)
			r.printf("  Size/request:\t%d bytes\n", r.sizeTotal/int64(len(r.lats)))
		}
		r.printHistogram()
		r.printLatencies()
		r.printf("\nDetails (average, fastest, slowest):")
		r.printSection("DNS+dialup", r.avgConn, r.connLats)
		r.printSection("DNS-lookup", r.avgDNS, r.dnsLats)
		r.printSection("req write", r.avgReq, r.reqLats)
		r.printSection("resp wait", r.avgDelay, r.delayLats)
		r.printSection("resp read", r.avgRes, r.resLats)
		r.printStatusCodes()
	}
	if len(r.errorDist) > 0 {
		r.printErrors()
	}
	r.printf("\n")
}

// printSection prints details for http-trace fields
func (r *report) printSection(tag string, avg float64, lats []float64) {
	sort.Float64s(lats)
	fastest, slowest := lats[0], lats[len(lats)-1]
	r.printf("\n  %s:\t", tag)
	r.printf(" %4.4f secs, %4.4f secs, %4.4f secs", avg, fastest, slowest)
}

// printLatencies prints percentile latencies.
func (r *report) printLatencies() {
	pctls := []int{10, 25, 50, 75, 90, 95, 99}
	data := make([]float64, len(pctls))
	j := 0
	for i := 0; i < len(r.lats) && j < len(pctls); i++ {
		current := i * 100 / len(r.lats)
		if current >= pctls[j] {
			data[j] = r.lats[i]
			j++
		}
	}
	r.printf("\nLatency distribution:\n")
	for i := 0; i < len(pctls); i++ {
		if data[i] > 0 {
			r.printf("  %v%% in %4.4f secs\n", pctls[i], data[i])
		}
	}
}

func (r *report) printHistogram() {
	bc := 10
	buckets := make([]float64, bc+1)
	counts := make([]int, bc+1)
	bs := (r.slowest - r.fastest) / float64(bc)
	for i := 0; i < bc; i++ {
		buckets[i] = r.fastest + bs*float64(i)
	}
	buckets[bc] = r.slowest
	var bi int
	var max int
	for i := 0; i < len(r.lats); {
		if r.lats[i] <= buckets[bi] {
			i++
			counts[bi]++
			if max < counts[bi] {
				max = counts[bi]
			}
		} else if bi < len(buckets)-1 {
			bi++
		}
	}
	r.printf("\nResponse time histogram:\n")
	for i := 0; i < len(buckets); i++ {
		// Normalize bar lengths.
		var barLen int
		if max > 0 {
			barLen = (counts[i]*40 + max/2) / max
		}
		r.printf("  %4.3f [%v]\t|%v\n", buckets[i], counts[i], strings.Repeat(barChar, barLen))
	}
}

// printStatusCodes prints status code distribution.
func (r *report) printStatusCodes() {
	r.printf("\n\nStatus code distribution:\n")
	for code, num := range r.statusCodeDist {
		r.printf("  [%d]\t%d responses\n", code, num)
	}
}

func (r *report) printErrors() {
	r.printf("\nError distribution:\n")
	for err, num := range r.errorDist {
		r.printf("  [%d]\t%s\n", num, err)
	}
}

func (r *report) printf(s string, v ...interface{}) {
	fmt.Fprintf(r.w, s, v...)
}
