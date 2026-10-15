package nmeadevicediscovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// The devicelist keeps a small JSON file under VIAM_MODULE_DATA so that
//
//   - first_seen survives restarts of the module, viam-server and the box;
//   - devices that have dropped off the bus stay listed with their old
//     last_seen, instead of vanishing when viamboat restarts and its all-pgn
//     history starts empty.
//
// The file is keyed by stable identity (see stableIdentity), never by the
// display label, so a label that changes as product info arrives does not
// reset first_seen. Sources without an address claim ("src-N") are not
// persisted: a bus address is not an identity.

const stateVersion = 1

type persistedDevice struct {
	// Fields are the snake_case identity fields plus "src", as reported.
	Fields    map[string]interface{} `json:"fields"`
	FirstSeen string                 `json:"first_seen,omitempty"`
	LastSeen  string                 `json:"last_seen,omitempty"`
}

type persistedState struct {
	Version int                        `json:"version"`
	Devices map[string]persistedDevice `json:"devices"` // keyed by stable identity
}

func newState() *persistedState {
	return &persistedState{Version: stateVersion, Devices: map[string]persistedDevice{}}
}

// loadState reads the state file; a missing file is an empty state.
func loadState(path string) (*persistedState, error) {
	st := newState()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	if st.Devices == nil {
		st.Devices = map[string]persistedDevice{}
	}
	return st, nil
}

// save writes atomically (temp file + rename) so a power cut mid-write
// leaves the previous file intact.
func (st *persistedState) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// merge folds the devices seen in the current all-pgn history into the
// persisted state and returns the union: live devices with their persisted
// first_seen, plus devices the file knows but the bus has not heard from
// since viamboat restarted. The boolean reports whether the state changed
// and should be saved.
func (st *persistedState) merge(live map[string]mergedDevice, now time.Time) (map[string]mergedDevice, bool) {
	nowRaw := now.Format(time.RFC822Z)
	out := make(map[string]mergedDevice, len(live)+len(st.Devices))
	changed := false

	for identity, lv := range live {
		m := lv

		if !lv.hasClaim {
			// Keyed by address until its claim is seen; report, don't persist.
			if m.firstSeenRaw == "" {
				m.firstSeen, m.firstSeenRaw = now, nowRaw
			}
			out[identity] = m
			continue
		}

		prev, had := st.Devices[identity]
		if had {
			// Fields the bus has not repeated since restart (product info is
			// only broadcast at power-up or on request) come from the file;
			// anything heard now overrides.
			fields := make(map[string]interface{}, len(prev.Fields)+len(lv.fields))
			for k, v := range prev.Fields {
				if k != "src" {
					fields[k] = v
				}
			}
			for k, v := range lv.fields {
				fields[k] = v
			}
			m.fields = fields
			m.firstSeen, m.firstSeenRaw = parseRaw(prev.FirstSeen)
			if pl, plRaw := parseRaw(prev.LastSeen); pl.After(m.lastSeen) {
				m.lastSeen, m.lastSeenRaw = pl, plRaw
			}
		}
		if m.firstSeenRaw == "" {
			// First sight: the earliest bus timestamp we have for it, which
			// predates this sensor's install if viamboat was already running.
			if lv.firstSeenRaw != "" {
				m.firstSeen, m.firstSeenRaw = lv.firstSeen, lv.firstSeenRaw
			} else {
				m.firstSeen, m.firstSeenRaw = now, nowRaw
			}
		}
		out[identity] = m

		next := toPersisted(m)
		if !had || !samePersisted(prev, next) {
			st.Devices[identity] = next
			changed = true
		}
	}

	// Devices the file remembers that are not in the current history.
	for identity, p := range st.Devices {
		if _, seen := out[identity]; seen {
			continue
		}
		out[identity] = fromPersisted(identity, p)
	}
	return out, changed
}

func (st *persistedState) forget(identity string) bool {
	if _, ok := st.Devices[identity]; !ok {
		return false
	}
	delete(st.Devices, identity)
	return true
}

// Conversions between the in-memory and on-disk shapes.

func toPersisted(m mergedDevice) persistedDevice {
	p := persistedDevice{
		Fields:    make(map[string]interface{}, len(m.fields)+1),
		FirstSeen: m.firstSeenRaw,
		LastSeen:  m.lastSeenRaw,
	}
	for k, v := range m.fields {
		p.Fields[k] = v
	}
	p.Fields["src"] = m.src
	return p
}

func fromPersisted(identity string, p persistedDevice) mergedDevice {
	m := mergedDevice{
		identity: identity,
		hasClaim: true,
		fields:   make(map[string]interface{}, len(p.Fields)),
	}
	for k, v := range p.Fields {
		if k != "src" {
			m.fields[k] = v
		}
	}
	m.src, _ = asInt(p.Fields["src"])
	m.firstSeen, m.firstSeenRaw = parseRaw(p.FirstSeen)
	m.lastSeen, m.lastSeenRaw = parseRaw(p.LastSeen)
	return m
}

// samePersisted compares through JSON so that an int from an in-process
// reading and the float64 it becomes after a round trip through the file
// (or gRPC) count as equal; otherwise every call would rewrite the file.
func samePersisted(a, b persistedDevice) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ab, bb)
}

func parseRaw(raw string) (time.Time, string) {
	if raw == "" {
		return time.Time{}, ""
	}
	ts, err := time.Parse(time.RFC822Z, raw)
	if err != nil {
		return time.Time{}, raw
	}
	return ts, raw
}
