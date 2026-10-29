// letterboxd-user-checker asks Letterboxd's signup endpoint whether a username
// can be registered, one rotating proxy per request.
//
// A 404 on letterboxd.com/<name>/ is not proof of a free name: "admin" has no
// profile, yet signup rejects it, and the profile pages sit behind a
// Cloudflare challenge anyway. Only /s/checkusername, the endpoint the signup
// form polls while you type, separates free, taken and reserved.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var debug bool

type verdict int

const (
	vRetry     verdict = iota // proxy junk, anything untrustworthy
	vBlocked                  // Cloudflare challenged this exit IP
	vLimited                  // 429 or Cloudflare 1015: real answer, proxy is fine, try another
	vServer                   // Letterboxd or Cloudflare 5xx: not the proxy's fault
	vOdd                      // real Letterboxd answer we do not understand
	vAvailable                // AVAILABLE
	vTaken                    // TAKEN (or YOU, only when signed in)
	vReserved                 // ILLEGAL: blocked word, never registrable
	vInvalid                  // INVALID, TOO_SHORT, TOO_LONG, MISSING
)

var verdictName = map[verdict]string{
	vRetry: "retry", vBlocked: "challenged", vLimited: "limited", vServer: "server-error",
	vOdd: "odd", vAvailable: "available", vTaken: "taken", vReserved: "reserved", vInvalid: "invalid",
}

const (
	charsetDefault = "abcdefghijklmnopqrstuvwxyz0123456789_"
	minLen, maxLen = 2, 15 // TOO_SHORT below 2, TOO_LONG above 15 (verified 2026-10-29)
)

func main() {
	var (
		length     = flag.Int("len", 2, "generate every name of this length from -charset")
		charset    = flag.String("charset", charsetDefault, "characters for generated names")
		in         = flag.String("in", "", "wordlist instead of generated names, one per line")
		list       = flag.String("list", "", "also write the generated names to this file")
		out        = flag.String("out", "available.txt", "file that collects available names")
		reservedF  = flag.String("reserved", "reserved.txt", "file that collects reserved (ILLEGAL) names")
		done       = flag.String("done", "checked.txt", "names already decided, used to resume")
		proxies    = flag.String("proxies", "proxies.txt", "proxy list, scheme://host:port per line")
		perProxy   = flag.Int("per-proxy", 2, "concurrent requests through each proxy")
		timeout    = flag.Duration("timeout", 10*time.Second, "per-request timeout")
		tries      = flag.Int("tries", 10, "attempts per name before giving up")
		backoff    = flag.Duration("backoff", 10*time.Second, "pause after a proxy's first failure, doubles each failure in a row")
		maxBackoff = flag.Duration("max-backoff", 10*time.Minute, "longest pause for a failing proxy")
		limitPause = flag.Duration("limit-pause", 30*time.Second, "pause for a proxy Letterboxd rate limited")
		confirm    = flag.Bool("confirm", true, "re-check every hit through a different proxy")
		dbg        = flag.Bool("debug", false, "print why each proxy attempt was rejected")
		direct     = flag.Bool("direct", true, "also use this machine's own connection")
		directRate = flag.Int("direct-rate", 40, "direct requests per minute, 0 for no limit")
		reload     = flag.Duration("reload", 2*time.Minute, "re-read the proxy list this often, 0 to disable")
	)
	flag.Parse()
	debug = *dbg

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var all []string
	var err error
	if *in != "" {
		all, err = loadNames(*in)
		if err != nil {
			die(err)
		}
	} else {
		if *length < minLen || *length > maxLen {
			die(fmt.Errorf("-len must be %d..%d, Letterboxd rejects anything else", minLen, maxLen))
		}
		all = generate(*charset, *length)
		fmt.Printf("generated %d names of length %d from %q\n", len(all), *length, *charset)
	}
	if *list != "" {
		if err := os.WriteFile(*list, []byte(strings.Join(all, "\n")+"\n"), 0o644); err != nil {
			die(err)
		}
		fmt.Printf("name list written to %s\n", *list)
	}

	pool, err := LoadPool(*proxies, *timeout, *backoff, *maxBackoff, *limitPause)
	switch {
	case err == nil:
		live, _ := pool.Stats()
		fmt.Printf("%d proxies loaded from %s, %d workers each\n", live, *proxies, *perProxy)
	case *direct:
		fmt.Printf("no proxy list (%v), running direct from this machine\n", err)
	default:
		die(err)
	}
	if pool == nil {
		pool = &Pool{seen: map[string]bool{}, baseBackoff: *backoff, maxBackoff: *maxBackoff, limitPause: *limitPause}
	}

	var fallback *limiter
	if *direct {
		fallback = newLimiter(*directRate)
		Direct.cli = Direct.client(*timeout)
		fmt.Printf("direct connection on, limited to %d requests per minute\n", *directRate)
	}

	skip, err := loadSet(*done)
	if err != nil {
		die(err)
	}
	var names []string
	for _, n := range all {
		if !skip[n] {
			names = append(names, n)
		}
	}
	if len(skip) > 0 {
		fmt.Printf("resuming, %d already checked\n", len(all)-len(names))
	}
	fmt.Printf("checking %d names\n", len(names))
	if len(names) == 0 {
		return
	}

	hits := openAppend(*out)
	defer hits.Close()
	reserved := openAppend(*reservedF)
	defer reserved.Close()
	log := openAppend(*done)
	defer log.Close()

	r := &run{
		pool:    pool,
		timeout: *timeout,
		tries:   *tries,
		confirm: *confirm,
		queue:   make(chan *job, len(names)),
		start:   time.Now(),
		total:   len(names),
	}
	r.record = func(name string, v verdict) {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch v {
		case vAvailable:
			writeLine(hits, *out, name)
			r.found = append(r.found, name)
			fmt.Printf("\r%-110s\r", "")
			fmt.Println("AVAILABLE:", name)
		case vReserved:
			writeLine(reserved, *reservedF, name)
		}
		// Names we gave up on stay out of the resume log, so the next run tries them again.
		if v != vRetry {
			writeLine(log, *done, name+"\t"+verdictName[v])
		}
	}

	// Every name is a job. It sits in the queue or in one worker's hands until
	// decided, then the pending group shrinks by one.
	r.pending.Add(len(names))
	for _, n := range names {
		r.queue <- &job{name: n}
	}

	// Workers stop when every job is decided or the user interrupts.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	spawn := func(px *proxy, n int, lim *limiter) {
		for i := 0; i < n; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				r.worker(wctx, px, lim)
			}()
		}
	}
	pool.mu.Lock()
	proxied := append([]*proxy(nil), pool.all...)
	pool.mu.Unlock()
	for _, px := range proxied {
		spawn(px, *perProxy, nil)
	}
	if *direct {
		spawn(Direct, 1, fallback)
	}

	if *reload > 0 {
		go func() {
			t := time.NewTicker(*reload)
			defer t.Stop()
			for {
				select {
				case <-wctx.Done():
					return
				case <-t.C:
					added, err := pool.Reload(*proxies, *timeout)
					if err != nil || len(added) == 0 {
						continue
					}
					for _, px := range added {
						spawn(px, *perProxy, nil)
					}
					fmt.Printf("\r%-110s\rproxy list reloaded: %d new\n", "", len(added))
				}
			}
		}()
	}

	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
				r.progress("")
			}
		}
	}()

	finished := make(chan struct{})
	go func() {
		r.pending.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-ctx.Done():
	}
	cancel()
	workers.Wait()

	r.progress("\n")
	fmt.Printf("done in %s\n", time.Since(r.start).Round(time.Second))
	fmt.Printf("  available %d  taken %d  reserved %d  invalid %d  gave up %d\n",
		r.n[vAvailable].Load(), r.n[vTaken].Load(), r.n[vReserved].Load(), r.n[vInvalid].Load(), r.n[vRetry].Load())
	fmt.Printf("  attempts: %d proxy junk, %d challenged, %d rate limited, %d server errors, %d odd\n",
		r.e[vRetry].Load(), r.e[vBlocked].Load(), r.e[vLimited].Load(), r.e[vServer].Load(), r.e[vOdd].Load())
	if len(r.found) > 0 {
		sort.Strings(r.found)
		fmt.Printf("available this run (%s): %s\n", *out, strings.Join(r.found, " "))
	}
	if g := r.n[vRetry].Load(); g > 0 {
		fmt.Printf("%d names gave up, run again to retry them\n", g)
	}
}

// job is one name on its way to a verdict.
type job struct {
	name    string
	tries   int    // attempts that produced no trustworthy answer
	yesFrom *proxy // proxy that said available; a second proxy must agree
}

type run struct {
	pool    *Pool
	timeout time.Duration
	tries   int
	confirm bool
	queue   chan *job
	record  func(string, verdict)

	mu      sync.Mutex
	found   []string
	pending sync.WaitGroup
	start   time.Time
	total   int
	checked atomic.Int64
	n       [vInvalid + 1]atomic.Int64 // decided names per verdict, vRetry = gave up
	e       [vInvalid + 1]atomic.Int64 // rejected attempts per reason
}

func (r *run) progress(end string) {
	l, p := r.pool.Stats()
	c := r.checked.Load()
	rate := float64(c) / time.Since(r.start).Seconds()
	fmt.Printf("\rchecked %d/%d  avail %d  taken %d  reserved %d  gaveup %d  proxies %d live %d paused  %.1f/s   %s",
		c, r.total, r.n[vAvailable].Load(), r.n[vTaken].Load(), r.n[vReserved].Load(),
		r.n[vRetry].Load(), l, p, rate, end)
}

func (r *run) decide(j *job, v verdict) {
	r.n[v].Add(1)
	r.record(j.name, v)
	r.checked.Add(1)
	r.pending.Done()
}

// worker pulls jobs through one proxy until the context ends. It sleeps
// through the proxy's pauses, so a dead proxy costs nothing but its own time.
func (r *run) worker(ctx context.Context, px *proxy, lim *limiter) {
	for {
		if px.wait(ctx) != nil {
			return
		}
		if lim != nil && lim.wait(ctx) != nil {
			return
		}
		var j *job
		select {
		case <-ctx.Done():
			return
		case j = <-r.queue:
		}
		if j.yesFrom == px {
			// This proxy already said yes; hand the job to another one.
			r.queue <- j
			time.Sleep(50 * time.Millisecond)
			continue
		}

		a := ask(ctx, px, j.name, r.timeout)
		if ctx.Err() != nil {
			r.queue <- j
			return
		}
		if a.v < vAvailable {
			r.e[a.v].Add(1)
			if debug {
				fmt.Printf("\r%-110s\rdebug %s via %s: %s %s err=%v\n", "", j.name, px, verdictName[a.v], a.why, a.err)
			}
		}

		switch {
		case a.v == vBlocked:
			// Cloudflare judged the address, not the name.
			r.pool.Blocked(px)
		case a.v == vLimited:
			r.pool.Limited(px, a.retryAfter)
			j.tries++
		case a.v == vServer:
			// Letterboxd itself stumbled; give it and this route a moment.
			r.pool.Limited(px, a.retryAfter)
			j.tries++
		case a.v == vOdd:
			r.pool.Succeed(px)
			j.tries++
		case a.v == vRetry:
			// A proxy that never answered anything is junk, not evidence
			// about this name. Only a proven proxy's failure counts.
			if r.pool.Fail(px) {
				j.tries++
			}
		case a.v == vAvailable && r.confirm && j.yesFrom == nil:
			// One yes can still be one lying proxy: a free name must answer twice.
			r.pool.Succeed(px)
			j.yesFrom = px
		case a.v != vAvailable && j.yesFrom != nil:
			// The routes disagree. Trust neither; ask a third.
			r.pool.Succeed(px)
			if debug {
				fmt.Printf("\r%-110s\rdebug %s: %s said available, %s says %s\n", "", j.name, j.yesFrom, px, verdictName[a.v])
			}
			j.yesFrom = nil
			j.tries++
		default:
			r.pool.Succeed(px)
			r.decide(j, a.v)
			continue
		}
		if j.tries >= r.tries {
			r.decide(j, vRetry)
			continue
		}
		r.queue <- j
	}
}

type limiter struct{ tick <-chan time.Time }

func newLimiter(perMinute int) *limiter {
	if perMinute <= 0 {
		return &limiter{}
	}
	return &limiter{tick: time.NewTicker(time.Minute / time.Duration(perMinute)).C}
}

func (l *limiter) wait(ctx context.Context) error {
	if l.tick == nil {
		return nil
	}
	select {
	case <-l.tick:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type answer struct {
	v          verdict
	retryAfter time.Duration
	why        string
	err        error
}

// checkResponse is what /s/checkusername returns:
// {"result":true,"csrf":"…","data":{"result":"TAKEN"}}
type checkResponse struct {
	Result *bool `json:"result"`
	Data   *struct {
		Result string `json:"result"`
	} `json:"data"`
}

func ask(ctx context.Context, px *proxy, name string, timeout time.Duration) answer {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodGet,
		"https://letterboxd.com/s/checkusername?q="+url.QueryEscape(name), nil)
	if err != nil {
		return answer{v: vRetry, err: err}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Safari/605.1.15")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", "https://letterboxd.com/create-account/")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	// The User-Agent matches the Safari TLS fingerprint; a mismatch is a bot signal.

	resp, err := px.cli.Do(req)
	if err != nil {
		return answer{v: vRetry, why: "transport", err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return answer{v: vRetry, why: "body read", err: err}
	}
	a := classify(resp.StatusCode, resp.Header, body)
	if debug && a.v < vAvailable {
		snippet := strings.Join(strings.Fields(string(body)), " ")
		if len(snippet) > 140 {
			snippet = snippet[:140]
		}
		a.why += fmt.Sprintf(" [status=%d ray=%q ctype=%q body=%q]", resp.StatusCode,
			resp.Header.Get("Cf-Ray"), resp.Header.Get("Content-Type"), snippet)
	}
	return a
}

// classify turns one HTTP response into a verdict. Letterboxd sits behind
// Cloudflare, so every genuine answer carries a Cf-Ray header; anything
// without it was written by the proxy (error pages, captive portals, ads).
func classify(status int, h http.Header, body []byte) answer {
	ra := retryAfter(h.Get("Retry-After"))
	text := string(body)

	if h.Get("Cf-Ray") == "" {
		switch status {
		case http.StatusProxyAuthRequired:
			return answer{v: vRetry, why: "407 proxy wants auth"}
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return answer{v: vRetry, why: fmt.Sprintf("%d from the proxy itself", status)}
		}
		return answer{v: vRetry, why: fmt.Sprintf("%d without Cf-Ray, not Letterboxd", status)}
	}

	// Managed / JS / interactive challenge: the exit IP is distrusted.
	if h.Get("Cf-Mitigated") == "challenge" || strings.Contains(text, "challenge-platform") ||
		strings.Contains(text, "<title>Just a moment") {
		return answer{v: vBlocked, why: "cloudflare challenge"}
	}

	switch {
	case status == http.StatusTooManyRequests:
		return answer{v: vLimited, retryAfter: ra, why: "429"}
	case strings.Contains(text, "error code: 1015"):
		// Cloudflare rate limit rule, served as 429 or 403 depending on config.
		return answer{v: vLimited, retryAfter: ra, why: "cloudflare 1015 rate limit"}
	case status == http.StatusForbidden:
		// 1020 (WAF), 1006-1009 (IP / ASN / country ban), bot fight mode.
		why := "403"
		for _, c := range []string{"1020", "1006", "1007", "1008", "1009", "1010", "1012"} {
			if strings.Contains(text, "error code: "+c) || strings.Contains(text, "Error "+c) {
				why = "cloudflare " + c
				break
			}
		}
		return answer{v: vBlocked, why: why}
	case status >= 500:
		// 500-504 from Letterboxd, 520-530 from Cloudflare failing to reach it.
		return answer{v: vServer, retryAfter: ra, why: fmt.Sprintf("%d", status)}
	case status >= 300 && status < 400:
		return answer{v: vOdd, why: fmt.Sprintf("%d redirect to %q", status, h.Get("Location"))}
	case status != http.StatusOK:
		// 400, 404, 405, … : the endpoint answered but not as expected.
		return answer{v: vOdd, why: fmt.Sprintf("%d", status)}
	}

	var cr checkResponse
	if err := json.Unmarshal(body, &cr); err != nil || cr.Result == nil || cr.Data == nil {
		// A 200 that is not the JSON envelope: truncated body or an injected page.
		return answer{v: vRetry, why: "200 but not the checkusername JSON"}
	}
	if !*cr.Result {
		return answer{v: vOdd, why: "result:false"}
	}
	switch cr.Data.Result {
	case "AVAILABLE":
		return answer{v: vAvailable}
	case "TAKEN", "YOU":
		return answer{v: vTaken}
	case "ILLEGAL":
		return answer{v: vReserved}
	case "INVALID", "TOO_SHORT", "TOO_LONG", "MISSING":
		return answer{v: vInvalid}
	}
	return answer{v: vOdd, why: "unknown result " + strconv.Quote(cr.Data.Result)}
}

// retryAfter reads seconds or an HTTP date; zero when absent or unreadable.
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// generate returns every string of length n over charset, in order.
func generate(charset string, n int) []string {
	cs := []byte(strings.ToLower(charset))
	seen := map[byte]bool{}
	var uniq []byte
	for _, c := range cs {
		if !seen[c] && validChar(c) {
			seen[c] = true
			uniq = append(uniq, c)
		}
	}
	var out []string
	buf := make([]byte, n)
	var rec func(int)
	rec = func(i int) {
		if i == n {
			out = append(out, string(buf))
			return
		}
		for _, c := range uniq {
			buf[i] = c
			rec(i + 1)
		}
	}
	rec(0)
	return out
}

func validChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
}

// valid mirrors Letterboxd's rule: a-z, 0-9 and _, 2 to 15 characters.
func valid(s string) bool {
	if len(s) < minLen || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !validChar(s[i]) {
			return false
		}
	}
	return true
}

func loadNames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var names []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		n := strings.ToLower(strings.TrimSpace(sc.Text()))
		if n == "" || seen[n] || !valid(n) {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	return names, sc.Err()
}

// loadSet reads the resume log; the name is the first field of each line.
func loadSet(path string) (map[string]bool, error) {
	set := map[string]bool{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return set, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fs := strings.Fields(sc.Text()); len(fs) > 0 {
			set[fs[0]] = true
		}
	}
	return set, sc.Err()
}

func openAppend(path string) *os.File {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		die(err)
	}
	return f
}

func writeLine(f *os.File, path, line string) {
	if _, err := fmt.Fprintln(f, line); err != nil {
		die(fmt.Errorf("writing %s: %w", path, err))
	}
	_ = f.Sync()
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
