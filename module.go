package nmeadevicediscovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sensor "go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

var (
	Devicelist       = resource.NewModel("viam-soleng", "nmea-device-discovery", "devicelist")
	errUnimplemented = errors.New("unimplemented")
)

func init() {
	resource.RegisterComponent(sensor.API, Devicelist,
		resource.Registration[sensor.Sensor, *Config]{
			Constructor: newNmeaDeviceDiscoveryDevicelist,
		},
	)
}

type Config struct {
	// AllPGNComponent names the viamboat boat-all-pgn sensor component whose
	// accumulated bus history this model reshapes into a device inventory.
	AllPGNComponent string `json:"all-pgn-component"`
	// Persist keeps a JSON file under VIAM_MODULE_DATA so first_seen survives
	// restarts and devices that left the bus stay listed. On by default.
	Persist *bool `json:"persist,omitempty"`
}

// Validate ensures all parts of the config are valid and important fields exist.
// Returns three values:
//  1. Required dependencies: other resources that must exist for this resource to work.
//  2. Optional dependencies: other resources that may exist but are not required.
//  3. An error if any Config fields are missing or invalid.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	if cfg.AllPGNComponent == "" {
		return nil, nil, resource.NewConfigValidationFieldRequiredError(path, "all-pgn-component")
	}
	return []string{cfg.AllPGNComponent}, nil, nil
}

type nmeaDeviceDiscoveryDevicelist struct {
	resource.AlwaysRebuild
	resource.Named

	name resource.Name

	logger logging.Logger
	cfg    *Config

	// allPgn is the viamboat boat-all-pgn sensor this model reads through. It
	// holds the bus history (latest message per PGN and source, with
	// timestamps) since viamboat last started.
	allPgn sensor.Sensor

	// state is what this model remembers across restarts (see state.go);
	// statePath is "" when persistence is off or VIAM_MODULE_DATA is unset.
	mu        sync.Mutex
	state     *persistedState
	statePath string

	cancelCtx  context.Context
	cancelFunc func()
}

func newNmeaDeviceDiscoveryDevicelist(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	return NewDevicelist(ctx, deps, rawConf.ResourceName(), conf, logger)

}

func NewDevicelist(ctx context.Context, deps resource.Dependencies, name resource.Name, conf *Config, logger logging.Logger) (sensor.Sensor, error) {
	allPgn, err := sensor.FromProvider(deps, conf.AllPGNComponent)
	if err != nil {
		return nil, fmt.Errorf("all-pgn-component %q not found in dependencies: %w", conf.AllPGNComponent, err)
	}

	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	s := &nmeaDeviceDiscoveryDevicelist{
		name:       name,
		logger:     logger,
		cfg:        conf,
		allPgn:     allPgn,
		state:      newState(),
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
	}

	if s.persist() {
		if dir := os.Getenv("VIAM_MODULE_DATA"); dir != "" {
			s.statePath = filepath.Join(dir, name.ShortName()+"-devices.json")
			if st, err := loadState(s.statePath); err != nil {
				logger.Warnw("could not load persisted devices; starting empty", "path", s.statePath, "error", err)
			} else {
				s.state = st
				if n := len(st.Devices); n > 0 {
					logger.Infow("loaded persisted devices", "path", s.statePath, "devices", n)
				}
			}
		} else {
			logger.Warn("VIAM_MODULE_DATA is not set; first_seen and offline devices will not survive restarts")
		}
	}
	return s, nil
}

// persist reports the effective setting: true unless "persist": false.
func (s *nmeaDeviceDiscoveryDevicelist) persist() bool {
	return s.cfg == nil || s.cfg.Persist == nil || *s.cfg.Persist
}

func (s *nmeaDeviceDiscoveryDevicelist) Name() resource.Name {
	return s.name
}

// The NMEA 2000 device-identity PGNs this model reshapes into an inventory,
// plus Heartbeat, read only for its timestamp.
const (
	pgnAddressClaim = 60928  // ISO Address Claim: NAME fields, source address
	pgnProductInfo  = 126996 // Product Information
	pgnConfigInfo   = 126998 // Configuration Information
	// Heartbeat is sent every 60 s by devices built to NMEA 2000 v2.0 or
	// later. The identity PGNs are only sent at power-up or on request, so
	// without it last_seen would say when a device last introduced itself,
	// not whether it is still on the bus.
	pgnHeartbeat = 126993
)

// readPGNs is the filter pushed down to the all-pgn sensor: a few entries
// per device rather than the whole bus history.
var readPGNs = []interface{}{pgnAddressClaim, pgnProductInfo, pgnConfigInfo, pgnHeartbeat}

// identityFieldNames maps the gonmea field names of each identity PGN to the
// snake_case keys this sensor reports.
var identityFieldNames = map[int]map[string]string{
	pgnAddressClaim: {
		"Unique Number":         "unique_number",
		"Manufacturer Code":     "manufacturer_code",
		"Device Instance Lower": "device_instance_lower",
		"Device Instance Upper": "device_instance_upper",
		"Device Function":       "device_function",
		"Device Class":          "device_class",
		"System Instance":       "system_instance",
		"Industry Group":        "industry_group",
	},
	pgnProductInfo: {
		"NMEA 2000 Version":     "nmea2000_version",
		"Product Code":          "product_code",
		"Model ID":              "model_id",
		"Software Version Code": "software_version_code",
		"Model Version":         "model_version",
		"Model Serial Code":     "model_serial_code",
		"Certification Level":   "certification_level",
		"Load Equivalency":      "load_equivalency",
	},
	pgnConfigInfo: {
		"Installation Description #1": "installation_description_1",
		"Installation Description #2": "installation_description_2",
		"Manufacturer Information":    "manufacturer_information",
	},
}

// asInt tolerates the numeric shapes a reading can arrive in: native ints from
// an in-process call, float64 once values have crossed a gRPC/structpb hop.
func asInt(v interface{}) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

// Readings returns one entry per device seen on the NMEA 2000 network: the
// devices in the all-pgn sensor's current history, merged per device from
// the identity PGNs, plus the devices remembered in the state file that the
// bus has not heard from since viamboat restarted. Devices are identified by
// their ISO NAME from the address claim (unique number, manufacturer, device
// instance and device function; see stableIdentity) rather than by source
// address: addresses move under contention, and keying by address would
// report ghost devices at stale addresses. Entries are never pruned;
// first_seen and last_seen let the caller decide what is new or stale.
func (s *nmeaDeviceDiscoveryDevicelist) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	live, err := s.liveDevices(ctx)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.state == nil {
		s.state = newState()
	}
	merged, changed := s.state.merge(live, time.Now())
	if changed && s.statePath != "" {
		if err := s.state.save(s.statePath); err != nil {
			s.logger.Warnw("could not persist devices", "path", s.statePath, "error", err)
		}
	}
	s.mu.Unlock()

	// Render display keys: "<manufacturer> <model> [<unique>]" reads like a
	// device name in the Viam app, while the bracketed unique number (or
	// source address for claim-less devices) keeps keys unique when a network
	// carries several identical products. Distinct devices that still share
	// a label are told apart by disambiguate.
	devices := map[string]interface{}{}
	for key, m := range disambiguate(merged) {
		devices[key] = m.render()
	}
	return devices, nil
}

// liveDevices filters the all-pgn history down to the identity PGNs (and
// Heartbeat) and merges them into one mergedDevice per stable identity,
// keyed by that identity.
func (s *nmeaDeviceDiscoveryDevicelist) liveDevices(ctx context.Context) (map[string]mergedDevice, error) {
	raw, err := s.allPgn.Readings(ctx, map[string]interface{}{"pgns": readPGNs})
	if err != nil {
		return nil, fmt.Errorf("Reading from all-pgn-component: %w", err)
	}

	// The all-pgn history holds one entry per (PGN, source); group the up to
	// three identity entries of each source address together.
	perSrc := map[int]map[int]map[string]interface{}{}
	for _, v := range raw {
		entry, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		pgn, okP := asInt(entry["pgn"])
		src, okS := asInt(entry["src"])
		if !okP || !okS {
			continue
		}
		if perSrc[src] == nil {
			perSrc[src] = map[int]map[string]interface{}{}
		}
		perSrc[src][pgn] = entry
	}

	// Merge each source's entries into one device and de-duplicate by stable
	// identity: a device that moved addresses (contention, commanded address)
	// appears under several sources, and only its most recent incarnation is
	// kept.
	winners := map[string]mergedDevice{}
	for src, entries := range perSrc {
		// A heartbeat alone says something is at this address, not what. It
		// only ever contributes a timestamp to a device identified by its
		// identity PGNs, so it does not create a placeholder entry.
		if !hasIdentityEntry(entries) {
			continue
		}
		m := mergedDevice{src: src, fields: map[string]interface{}{}}
		for pgn, entry := range entries {
			for from, to := range identityFieldNames[pgn] {
				if val, has := entry[from]; has {
					m.fields[to] = val
				}
			}
			// The all-pgn timestamp is CANTimeFormat.String() (RFC822Z);
			// last_seen is the newest across the merged entries, first_seen
			// (before the state file has a say) the oldest.
			ts, tsRaw, parsed := parseTimestamp(entry)
			if parsed {
				m.noteSeen(ts, tsRaw)
			} else if tsRaw != "" && m.lastSeenRaw == "" {
				m.lastSeenRaw = tsRaw
			}
		}

		// Stable identity from the address claim; a device we have only seen
		// identity fragments from (no claim yet) is identified by its address.
		_, m.hasClaim = entries[pgnAddressClaim]
		m.identity = fmt.Sprintf("src-%d", src)
		if m.hasClaim {
			m.identity = stableIdentity(m.fields)
		}

		if prev, seen := winners[m.identity]; seen && !m.beats(prev) {
			continue
		}
		winners[m.identity] = m
	}
	return winners, nil
}

// hasIdentityEntry reports whether a source's entries include any of the
// identity PGNs.
func hasIdentityEntry(entries map[int]map[string]interface{}) bool {
	for pgn := range entries {
		if _, ok := identityFieldNames[pgn]; ok {
			return true
		}
	}
	return false
}

// parseTimestamp reads an all-pgn entry's timestamp. The raw string is
// returned even when it does not parse, so it can still be reported.
func parseTimestamp(entry map[string]interface{}) (time.Time, string, bool) {
	raw, _ := entry["timestamp"].(string)
	if raw == "" {
		return time.Time{}, "", false
	}
	ts, err := time.Parse(time.RFC822Z, raw)
	return ts, raw, err == nil
}

// mergedDevice is one device: its identity fields merged from the identity
// PGNs, the address it was last heard at, and when it was first and last
// seen.
type mergedDevice struct {
	identity string
	hasClaim bool
	// fields holds the snake_case identity fields; src, first_seen and
	// last_seen are rendered from the other members.
	fields       map[string]interface{}
	src          int
	firstSeen    time.Time
	firstSeenRaw string
	lastSeen     time.Time
	lastSeenRaw  string
}

// noteSeen widens the first/last seen window by one bus timestamp.
func (m *mergedDevice) noteSeen(ts time.Time, raw string) {
	if m.lastSeenRaw == "" || ts.After(m.lastSeen) {
		m.lastSeen, m.lastSeenRaw = ts, raw
	}
	if m.firstSeenRaw == "" || ts.Before(m.firstSeen) {
		m.firstSeen, m.firstSeenRaw = ts, raw
	}
}

// render produces the reading entry for the device.
func (m mergedDevice) render() map[string]interface{} {
	out := make(map[string]interface{}, len(m.fields)+3)
	for k, v := range m.fields {
		out[k] = v
	}
	out["src"] = m.src
	if m.firstSeenRaw != "" {
		out["first_seen"] = m.firstSeenRaw
	}
	if m.lastSeenRaw != "" {
		out["last_seen"] = m.lastSeenRaw
	}
	return out
}

// beats decides which address of a device is reported when the history
// holds it at several. Newest last_seen wins; the all-pgn timestamps have
// one-minute resolution, so ties are common and must not fall through to
// map iteration order. On a tie prefer the more completely described
// address, then the lower one.
func (m mergedDevice) beats(prev mergedDevice) bool {
	if !m.lastSeen.Equal(prev.lastSeen) {
		return m.lastSeen.After(prev.lastSeen)
	}
	if len(m.fields) != len(prev.fields) {
		return len(m.fields) > len(prev.fields)
	}
	return m.src < prev.src
}

// stableIdentity is the device's ISO NAME minus its address: unique number,
// manufacturer, device instance and device function. Unique number plus
// manufacturer alone is not unique on a real bus: Garmin MFDs expose several
// virtual nodes (Display, Button Interface) under one unique number, and an
// Actisense dual gateway claims two addresses that differ only by device
// instance. Both are live at once and must be reported separately.
//
// The decoder drops the manufacturer code (and can drop the function) when
// the value is not in its lookup table, as with an unregistered code such as
// the 2046 placeholder from the open-source NMEA2000 library. Those slots
// read "unknown" rather than "<nil>" so the key stays legible and stable.
func stableIdentity(device map[string]interface{}) string {
	return fmt.Sprintf("%v-%v-%d-%v",
		intIfNumeric(device["unique_number"]),
		fieldOrUnknown(device, "manufacturer_code"),
		deviceInstance(device),
		fieldOrUnknown(device, "device_function"))
}

func fieldOrUnknown(device map[string]interface{}, key string) interface{} {
	v, has := device[key]
	if !has || v == nil || v == "" {
		return "unknown"
	}
	return intIfNumeric(v)
}

// deviceInstance assembles the NMEA 2000 device instance from the two
// address-claim fields: lower (3 bits) and upper (5 bits).
func deviceInstance(device map[string]interface{}) int {
	lower, _ := asInt(device["device_instance_lower"])
	upper, _ := asInt(device["device_instance_upper"])
	return lower | upper<<3
}

// disambiguate renders display keys for the devices and resolves the one
// collision a device's own fields leave open: two nodes that share
// manufacturer, model, unique number and device instance but differ by
// device function, as a plotter's Display and Button Interface nodes do
// before their product info is seen. Those get " (<device function>)"
// appended. The device instance is part of the base key (see
// deviceDisplayKey) rather than a collision qualifier, so a name does not
// depend on which other devices happen to be on the bus; after a restart,
// when devices reappear one at a time, each gets the same name as before.
func disambiguate(devices map[string]mergedDevice) map[string]mergedDevice {
	byKey := map[string][]mergedDevice{}
	for identity, m := range devices {
		k := deviceDisplayKey(identity, m.fields)
		byKey[k] = append(byKey[k], m)
	}
	out := map[string]mergedDevice{}
	for base, group := range byKey {
		if len(group) == 1 {
			out[base] = group[0]
			continue
		}
		for _, m := range group {
			out[fmt.Sprintf("%s (%v)", base, fieldOrUnknown(m.fields, "device_function"))] = m
		}
	}
	return out
}

// intIfNumeric normalizes a value to int when it is numeric. Values arrive as
// float64 after a structpb hop, and %v renders large floats in scientific
// notation (1.037921e+06); lookup-rendered strings ("Garmin") pass through.
func intIfNumeric(v interface{}) interface{} {
	if n, ok := asInt(v); ok {
		return n
	}
	return v
}

// sanitizeLabel makes a decoded NMEA string safe and tidy for use in a map
// key. Fixed-length string fields follow the C convention of NUL termination
// with junk after the terminator (a real Furuno MFD reports model_id
// "TZT14\x004 TZT" — the model is "TZT14"), and model IDs often carry doubled
// spaces; truncate at the first NUL, then collapse whitespace and controls.
func sanitizeLabel(s string) string {
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r <= ' ' || r == 0x7F
	}), " ")
}

// deviceDisplayKey builds the human-facing map key for a merged device:
// "<manufacturer> <model> [<unique number>]", with graceful fallbacks when
// product info or the address claim has not been seen yet. A non-zero
// device instance is appended as " #<n>": the two halves of a dual gateway
// claim instance 0 and 1, and the suffix keeps the second half's name the
// same whether or not the first is currently on the bus.
func deviceDisplayKey(identity string, device map[string]interface{}) string {
	label := ""
	if mfg, has := device["manufacturer_code"]; has {
		label = fmt.Sprintf("%v", intIfNumeric(mfg))
	}
	if model, ok := device["model_id"].(string); ok {
		if clean := sanitizeLabel(model); clean != "" {
			if label != "" {
				label += " "
			}
			label += clean
		}
	}

	suffix := identity // "src-N" for claim-less devices
	if unique, ok := asInt(device["unique_number"]); ok {
		suffix = fmt.Sprintf("%d", unique)
	}

	key := suffix
	if label != "" {
		key = fmt.Sprintf("%s [%s]", label, suffix)
	}
	if inst := deviceInstance(device); inst != 0 {
		key += fmt.Sprintf(" #%d", inst)
	}
	return key
}

// DoCommand supports:
//
//	{"command": "devices"}                   same as Readings
//	{"command": "forget", "device": "<key>"} drop one remembered device, by its
//	                                         reading key (or "identity")
//	{"command": "reset"}                     forget every remembered device
//	{"command": "status"}                    persistence settings and counts
func (s *nmeaDeviceDiscoveryDevicelist) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	command, _ := cmd["command"].(string)
	switch command {
	case "", "devices":
		return s.Readings(ctx, nil)

	case "forget":
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.state == nil {
			s.state = newState()
		}
		identity, _ := cmd["identity"].(string)
		if key, ok := cmd["device"].(string); ok && identity == "" {
			remembered := map[string]mergedDevice{}
			for id, p := range s.state.Devices {
				remembered[id] = fromPersisted(id, p)
			}
			for k, m := range disambiguate(remembered) {
				if k == key {
					identity = m.identity
				}
			}
			if identity == "" {
				return nil, fmt.Errorf("no remembered device %q", key)
			}
		}
		if identity == "" {
			return nil, errors.New("forget needs \"device\" (reading key) or \"identity\"")
		}
		if !s.state.forget(identity) {
			return nil, fmt.Errorf("no remembered device with identity %q", identity)
		}
		return map[string]interface{}{"forgotten": identity}, s.saveLocked()

	case "reset":
		s.mu.Lock()
		defer s.mu.Unlock()
		s.state = newState()
		if s.statePath != "" {
			if err := os.Remove(s.statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		return map[string]interface{}{"reset": true}, nil

	case "status":
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 0
		if s.state != nil {
			n = len(s.state.Devices)
		}
		return map[string]interface{}{
			"persist":            s.persist(),
			"persist_path":       s.statePath,
			"remembered_devices": n,
		}, nil
	}
	return nil, fmt.Errorf("unknown command %q", command)
}

// saveLocked persists the state if a path is configured. Caller holds s.mu.
func (s *nmeaDeviceDiscoveryDevicelist) saveLocked() error {
	if s.statePath == "" {
		return nil
	}
	return s.state.save(s.statePath)
}

func (s *nmeaDeviceDiscoveryDevicelist) Status(ctx context.Context) (map[string]interface{}, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *nmeaDeviceDiscoveryDevicelist) Close(context.Context) error {
	// Put close code here
	s.cancelFunc()
	return nil
}
