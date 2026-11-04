package main

import (
	"flag"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// rush is the rush-hour load generator, shipped in the same image:
//
//	harbour rush -url http://harbour/api/work -rps 30 -for 4m
//
// It opens a new connection per request (no keep-alive), so the Service spreads
// the calls over every pod and the HorizontalPodAutoscaler sees the load.
func rush(args []string) int {
	fs := flag.NewFlagSet("rush", flag.ContinueOnError)
	url := fs.String("url", "http://harbour/api/work", "endpoint to call")
	rps := fs.Int("rps", 30, "requests per second")
	length := fs.Duration("for", 4*time.Minute, "how long the rush hour lasts")
	if err := fs.Parse(args); err != nil || *rps < 1 {
		return 2
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

	var (
		sent, ok, failed atomic.Int64
		mu               sync.Mutex
		byCrane          = map[string]int{}
		wg               sync.WaitGroup
	)
	report := func(prefix string) {
		mu.Lock()
		names := make([]string, 0, len(byCrane))
		for n := range byCrane {
			names = append(names, n)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, fmt.Sprintf("%s=%d", n, byCrane[n]))
		}
		mu.Unlock()
		fmt.Printf("%s sent=%d ok=%d failed=%d cranes=%d [%s]\n", prefix, sent.Load(), ok.Load(), failed.Load(), len(names), strings.Join(parts, " "))
	}

	fmt.Printf("rush hour: %d req/s on %s for %s\n", *rps, *url, *length)
	tick := time.NewTicker(time.Second / time.Duration(*rps))
	defer tick.Stop()
	every := time.NewTicker(10 * time.Second)
	defer every.Stop()
	end := time.After(*length)
loop:
	for {
		select {
		case <-tick.C:
			sent.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := client.Get(*url)
				if err != nil {
					failed.Add(1)
					return
				}
				res.Body.Close()
				if res.StatusCode != http.StatusOK {
					failed.Add(1)
					return
				}
				ok.Add(1)
				mu.Lock()
				byCrane[res.Header.Get("X-Harbour-Crane")]++
				mu.Unlock()
			}()
		case <-every.C:
			report("…")
		case <-end:
			break loop
		}
	}
	wg.Wait()
	report("done")
	return 0
}
