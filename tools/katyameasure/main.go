// Command katyameasure is a throwaway measurement harness for ga-22tvtm.
// It is NOT committed.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

func main() {
	city := os.Args[1]
	eventPath := filepath.Join(city, ".gc", "events.jsonl")

	start := time.Now()
	f, err := os.Open(eventPath)
	if err != nil {
		panic(err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	latest := map[string]time.Time{}
	var latestStart time.Time
	lines := 0
	for sc.Scan() {
		lines++
		var e events.Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case events.OrderFired:
			if e.Ts.After(latest[e.Subject]) {
				latest[e.Subject] = e.Ts
			}
		case events.ControllerStarted:
			if e.Ts.After(latestStart) {
				latestStart = e.Ts
			}
		}
	}
	f.Close()
	fmt.Printf("ACTIVE FILE single pass (both types): %v (%d lines)\n", time.Since(start), lines)
	fmt.Printf("distinct order.fired subjects in active file: %d\n", len(latest))
	fmt.Printf("latest controller.started in active file: %v\n", latestStart)

	names := make([]string, 0, len(latest))
	for k := range latest {
		names = append(names, k)
	}
	sort.Strings(names)
	now := time.Now()
	for _, n := range names {
		fmt.Printf("  %-45s %v ago\n", n, now.Sub(latest[n]).Round(time.Second))
	}
}
