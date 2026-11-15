// Command wings-plugin-statecount is an example external plugin written in Go.
//
// Wings starts it as its own process and sends it events over stdin/stdout
// (JSON-RPC, see plugin/sdk/SPEC.md). It counts how many times each server
// changed state and keeps the counts in its data directory
// ($WINGS_PLUGIN_DATA/counts.json), which survives restarts and updates.
//
//	go build -o wings-plugin-statecount .
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/pelican/wings/plugin/sdk"
)

// counter implements plugin.EventListener.
type counter struct {
	mu     sync.Mutex
	path   string // counts.json in the data directory
	counts map[string]map[string]int
}

func (*counter) Name() string     { return "statecount" }
func (*counter) Events() []string { return []string{"server.state"} }

// OnEvent counts the change and writes the counts back to disk.
func (c *counter) OnEvent(_ context.Context, e sdk.Event) error {
	state, _ := e.Data["state"].(string)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[e.Server] == nil {
		c.counts[e.Server] = map[string]int{}
	}
	c.counts[e.Server][state]++
	fmt.Fprintf(os.Stderr, "%s is now %s (%d times)\n", e.Server, state, c.counts[e.Server][state])
	return c.save()
}

// save writes the counts atomically so a crash never leaves half a file.
func (c *counter) save() error {
	b, err := json.MarshalIndent(c.counts, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// load reads the counts saved by an earlier run, if any.
func load(dir string) (*counter, error) {
	c := &counter{path: filepath.Join(dir, "counts.json"), counts: map[string]map[string]int{}}
	b, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	return c, json.Unmarshal(b, &c.counts)
}

func main() {
	dir := os.Getenv("WINGS_PLUGIN_DATA")
	if dir == "" {
		dir = "." // run by hand, outside Wings
	}
	c, err := load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loading counts:", err)
		os.Exit(1)
	}
	if err := sdk.ServeEventListener(c); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
