// Command cli runs the devicelist model over a saved all-pgn readings dump,
// without a viam-server, and prints the resulting inventory as JSON.
//
//	viam machines part run --part <id> --component all-pgn --method GetReadings > dump.json
//	go run ./cmd/cli dump.json
//	go run ./cmd/cli -data-dir ./state day1.json; go run ./cmd/cli -data-dir ./state day2.json
//
// With -data-dir the run persists state there (as VIAM_MODULE_DATA does on a
// machine), so a second run over a later dump shows first_seen carried over
// and devices missing from the later dump still listed.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"nmeadevicediscovery"

	sensor "go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// fileAllPgn stands in for the viamboat boat-all-pgn sensor, serving a dump
// captured from a real machine. Like the real sensor, it honors the "pgns"
// filter in extra.
type fileAllPgn struct {
	sensor.Sensor
	readings map[string]interface{}
}

func (f *fileAllPgn) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	pgns, _ := extra["pgns"].([]interface{})
	if len(pgns) == 0 {
		return f.readings, nil
	}
	want := map[float64]bool{}
	for _, p := range pgns {
		switch n := p.(type) {
		case int:
			want[float64(n)] = true
		case float64:
			want[n] = true
		}
	}
	out := map[string]interface{}{}
	for k, v := range f.readings {
		if entry, ok := v.(map[string]interface{}); ok {
			if pgn, ok := entry["pgn"].(float64); ok && want[pgn] {
				out[k] = v
			}
		}
	}
	return out, nil
}

func (f *fileAllPgn) Name() resource.Name         { return sensor.Named("all-pgn") }
func (f *fileAllPgn) Close(context.Context) error { return nil }

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func realMain() error {
	dataDir := flag.String("data-dir", "", "persist state in this directory across runs (stands in for VIAM_MODULE_DATA)")
	flag.Parse()
	if *dataDir != "" {
		if err := os.Setenv("VIAM_MODULE_DATA", *dataDir); err != nil {
			return err
		}
	}
	if flag.NArg() != 1 {
		return fmt.Errorf("usage: cli [-data-dir DIR] <all-pgn-readings.json>")
	}

	readings, err := loadDump(flag.Arg(0))
	if err != nil {
		return err
	}

	ctx := context.Background()
	// Logs to stderr so stdout stays parseable JSON.
	logger := logging.NewBlankLogger("cli")
	logger.AddAppender(logging.NewWriterAppender(os.Stderr))
	deps := resource.Dependencies{sensor.Named("all-pgn"): &fileAllPgn{readings: readings}}
	persist := *dataDir != ""
	cfg := nmeadevicediscovery.Config{AllPGNComponent: "all-pgn", Persist: &persist}

	thing, err := nmeadevicediscovery.NewDevicelist(ctx, deps, sensor.Named("devicelist"), &cfg, logger)
	if err != nil {
		return err
	}
	defer thing.Close(ctx)

	out, err := thing.Readings(ctx, nil)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// loadDump accepts either the raw readings map or the {"readings": {...}}
// envelope that `viam machines part run` prints.
func loadDump(path string) (map[string]interface{}, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if inner, ok := m["readings"].(map[string]interface{}); ok {
		return inner, nil
	}
	return m, nil
}
