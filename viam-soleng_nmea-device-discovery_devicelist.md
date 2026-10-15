# Model viam-soleng:nmea-device-discovery:devicelist

A sensor that reports an inventory of the devices on an NMEA 2000 network. Each reading returns one entry per physical device, merging that device's ISO Address Claim (PGN 60928), Product Information (PGN 126996), and Configuration Information (PGN 126998) into a single object. Heartbeat (PGN 126993) is read as well, only for its timestamp.

Entries are keyed by a human-readable device label, `<manufacturer> <model> [<unique number>]` (for example `Simrad NSS4-12 iGPS [1589637]`). Devices are identified by their ISO NAME from the address claim (unique number, manufacturer, device instance and device function), not by bus address — NMEA 2000 addresses can move when devices contend, and a device that changed address is reported once, at its newest address. A device with a non-zero device instance carries it in its label as `... #<device instance>`, so the two halves of a dual gateway are `... [340545]` and `... [340545] #1` whether or not both are currently on the bus. Devices that still share a label and differ only by device function (a plotter's virtual nodes, before their product info is seen) get `... (<device function>)`. Entries are never pruned: use the `last_seen` timestamp to decide what counts as stale for your application. A device heard from only partially (for example, product info without an address claim) is keyed by its source address as `... [src-<n>]` until its claim is seen.

The model remembers what it has seen. Under `VIAM_MODULE_DATA` (a directory viam-server provides to every module, which survives restarts and module updates) it keeps a small JSON file keyed by device identity, so each entry carries a `first_seen` that survives restarts, and a device that has left the bus stays listed with its last `last_seen` instead of vanishing when viamboat restarts with an empty history. Product info the bus has not repeated since a restart is filled in from the file. The file is written only when something changed, atomically. Set `persist` to `false` to turn this off; use the `forget` and `reset` commands (below) to drop devices that were genuinely removed from the boat.

This model holds no bus connection of its own; it reads through a [viamboat](https://github.com/viam-labs/viamboat) `boat-all-pgn` sensor on the same machine, which accumulates the bus history. Inventory completeness is therefore passive: devices appear once they claim an address or answer requests on the bus. Devices send their identity only at power-up or when asked, so a device that was already on the bus when viamboat started is missing until something asks it; configure a `sender-component` and run the `discover` command (below) to ask. Query cadence of every few minutes is appropriate.

## Configuration

The following attribute template can be used to configure this model:

```json
{
  "all-pgn-component": <string>,
  "persist": <boolean>,
  "sender-component": <string>
}
```

### Attributes

The following attributes are available for this model:

| Name                | Type    | Inclusion | Description                                                                 |
|---------------------|---------|-----------|-----------------------------------------------------------------------------|
| `all-pgn-component` | string  | Required  | Name of the viamboat `boat-all-pgn` sensor component to read the bus through |
| `persist`           | boolean | Optional  | Default `true`: remember devices in `$VIAM_MODULE_DATA/<component name>-devices.json` so `first_seen` and offline devices survive restarts. Set `false` to report only what the current all-pgn history holds. |
| `sender-component`  | string  | Optional  | Name of a viamboat `sender` generic component on the same bus. Required for the `discover` command; without it, `discover` returns an error and everything else works as before. |

### Example Configuration

A complete machine configuration needs the viamboat reader-backed `boat-all-pgn` sensor plus this model depending on it:

```json
{
  "components": [
    {
      "name": "all-pgn",
      "api": "rdk:component:sensor",
      "model": "viam-labs:viamboat:boat-all-pgn",
      "attributes": { "reader": "can0" }
    },
    {
      "name": "devicelist",
      "api": "rdk:component:sensor",
      "model": "viam-soleng:nmea-device-discovery:devicelist",
      "attributes": { "all-pgn-component": "all-pgn" },
      "depends_on": ["all-pgn"]
    }
  ]
}
```

### Example Reading

```json
{
  "Garmin GPSMAP 8612 [1037921]": {
    "first_seen": "02 Aug 26 16:40 +0000",
    "last_seen": "07 Aug 26 09:12 +0000",
    "src": 3,
    "unique_number": 1037921,
    "manufacturer_code": "Garmin",
    "device_function": 130,
    "device_class": "Navigation",
    "model_id": "GPSMAP 8612",
    "product_code": 27716,
    "software_version_code": "9.10",
    "installation_description_1": "helm"
  },
  "999 CR Box [12345]": {
    "first_seen": "07 Aug 26 09:10 +0000",
    "last_seen": "07 Aug 26 09:10 +0000",
    "src": 42,
    "unique_number": 12345,
    "manufacturer_code": 999,
    "model_id": "CR Box"
  }
}
```

Fields are best-effort per device: only what that device has transmitted on the bus is present. Manufacturer codes known to the NMEA database render as names ("Garmin"); unknown codes stay numeric. Key labels are sanitized (fixed-length model strings are NUL-terminated with junk beyond the terminator on some devices), but the `model_id` field keeps the raw decoded value. `last_seen` has one-minute resolution.

`last_seen` is the newest timestamp among the device's identity messages and its Heartbeat. Devices only send their identity messages at power-up or on request, so on their own they would date a live device to the last viamboat restart. Devices built to NMEA 2000 v2.0 or later send Heartbeat every 60 seconds, which keeps `last_seen` current while they are on the bus; older devices that send no Heartbeat only move `last_seen` when they re-claim their address.

### first_seen

`first_seen` is the earliest bus timestamp the model has for the device: on first sight, the oldest timestamp across its entries in the all-pgn history (which can predate this component's install if viamboat was already running), and from then on the value remembered in the state file. It is tied to the device's identity, not its label, so it does not restart when product info arrives and the label grows. A source without an address claim (`src-<n>`) reports a `first_seen` but is not remembered, since a bus address is not an identity.

### DoCommand

| Command | Arguments | Effect |
|---------|-----------|--------|
| `{"command": "devices"}` | | Same as Readings. |
| `{"command": "forget", "device": "<reading key>"}` | or `"identity"` | Drop one remembered device (for example after physically removing it). It reappears, with a new `first_seen`, if it is heard again. |
| `{"command": "reset"}` | | Forget every remembered device and delete the state file. |
| `{"command": "status"}` | | Effective settings: `all_pgn_component`, `sender_component`, `persist`, `persist_path`, and `remembered_devices`. `GetStatus` returns the same map. |
| `{"command": "discover"}` | optional `"pgns"`: a subset of `[60928, 126996, 126998]` | Ask every device on the bus to announce itself (see below). Needs `sender-component`. Returns `{"requested": [...]}`. |

### Discovering devices already on the bus

NMEA 2000 devices send their address claim (60928), product information (126996) and configuration information (126998) when they power up, and after that only when asked. A device that was already running when viamboat started is therefore missing from the inventory, or listed without its model name, until something on the bus asks it to identify itself.

`discover` does the asking. Through the viamboat `sender` named by `sender-component`, it broadcasts an ISO Request (PGN 59904, destination 255) for each identity PGN: the address claim first, then, half a second later, product and configuration information. Every device on the bus replies within a second or two. The replies land in the all-pgn history and appear in the next reading; `discover` returns as soon as the requests are sent and does not wait for them.

```json
{"command": "discover"}
{"command": "discover", "pgns": [60928]}
```

Each request it sends is equivalent to this `sender` DoCommand:

```json
{"pgn": 59904, "dst": 255, "priority": 6, "fields": {"PGN": 60928}}
```

Things to know:

- Every device on the bus answers every request, and product information is a multi-frame reply, so `discover` is refused if it ran less than 30 seconds ago. Run it after a restart or when the inventory looks incomplete, not on a schedule.
- It cannot refresh the sender's own device. viamboat drops the frames it transmits itself, so the sender's replies never reach the all-pgn history.
- The sender must have an address. A sender configured with a `unique_number` claims one first; until the claim settles, `discover` fails with the sender's "address not yet claimed" error and can be retried right away.
