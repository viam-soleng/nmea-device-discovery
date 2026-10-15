# Module nmea-device-discovery

Inventories the devices on an NMEA 2000 network. The module watches the device-identity PGNs — ISO Address Claim (60928), Product Information (126996), and Configuration Information (126998) — plus Heartbeat (126993) for liveness, and reports one entry per physical device with its manufacturer, model, serial, installation details, current source address, and a last-seen timestamp.

It is an analyzer/inventory tool for chart plotters, sensors, gateways, and other nodes on the bus: query it every few minutes to see what is on the network and when each device was first and last heard from. It remembers devices across restarts in the module's data directory, so the list is everything the boat has ever had on the bus, not just what has spoken since the last power-up.

The module holds no bus connection of its own. It reads through a [viamboat](https://github.com/viam-labs/viamboat) `boat-all-pgn` sensor configured on the same machine, which must be connected to the NMEA 2000 network.

## Discovering devices already on the bus

Devices announce themselves (address claim, product information, configuration information) only when they power up or when asked. A device that was already running when viamboat started is missing from the inventory, or listed without its model name, until something on the bus asks it.

To ask, configure a viamboat `sender` generic component on the same bus, name it in the model's `sender-component` attribute, and run:

```json
{"command": "discover"}
```

`discover` broadcasts an ISO Request (PGN 59904) for each identity PGN through the sender. Every device on the bus replies within a second or two, and the replies show up in the next reading. Because every device answers, `discover` can run at most once every 30 seconds; run it after a restart or when the inventory looks incomplete, not on a schedule. It cannot refresh the sender's own device, since viamboat drops the frames it transmits itself. See the [model documentation](viam-soleng_nmea-device-discovery_devicelist.md#discovering-devices-already-on-the-bus) for details.

## Development

`go test ./...` covers the merge and persistence logic against synthetic readings. To inspect the model's output for any machine without deploying:

```sh
viam machines part run --part <part-id> --component all-pgn --method GetReadings > dump.json
go run ./cmd/cli dump.json
go run ./cmd/cli -data-dir ./state dump.json   # persists state across runs, like VIAM_MODULE_DATA
```

## Models

This module provides the following model(s):

- [`viam-soleng:nmea-device-discovery:devicelist`](viam-soleng_nmea-device-discovery_devicelist.md) - Sensor reporting one entry per NMEA 2000 device seen on the network
