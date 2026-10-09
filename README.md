# PDU Monitor

A macOS app for the PDUs of a data centre: power per rack at a glance, per-PDU and per-outlet detail, and
switching servers **On / Off / Restart** with a confirmation pop-up. It replaces the Python `pdustats` tool.

* **Overview** – one tile (or one table row) per rack: amps against the rack limit, kW, and a red alert when a rack, or one of its PDUs, goes over its limit.
* **Rack** – servers with the ports of both PDUs joined into one line (the outlet name, for example `200U31`, is the key), the PDUs of the rack with their own limits, total rack power.
* **PDU** – every outlet (state, A, W), totals per phase and bank, device information.
* **Control** – tick servers or ports, choose Restart / On / Off, confirm. A restart switches **all** ports of a server off, waits until the PDUs confirm it, pauses (default 8 s, 3–60 s in the settings) and switches them on again. If one PDU does not switch off, the ports already switched off are switched back on.
* **Devices** – CyberPower (ePDU2: PDU81xxx and similar) and APC (rPDU2 firmware, and the older rPDU/sPDU MIBs; tested model family AP7932) over **SNMP v1**. Read and write communities are set per device and kept in the macOS Keychain.
* **Updates** – built in (Sparkle); see [docs/UPDATES.md](docs/UPDATES.md).
* **Demo mode** (Settings → General) shows sample racks without touching any PDU.

## Install

Every build is on the **Actions** tab of the repository (artifact `PDUMonitor`, the zip with the Universal app).
Unzip, move `PDUMonitor.app` to Applications. Until the builds are signed with a Developer ID certificate and notarized,
macOS blocks the first start: right-click the app → **Open** (or `xattr -dr com.apple.quarantine /Applications/PDUMonitor.app`).
macOS 14 or newer, Apple Silicon and Intel.

The first time it talks to a PDU, macOS asks for permission to use the local network: allow it.

## First start

1. Settings (⌘,) → **Racks** → add a rack, set its limit (24 A).
2. **PDUs** → *Add PDU…*: name, rack, vendor, address, community. *Test connection* reads the device.
3. Name the outlets on the PDUs after the servers (`200U31`). The same name on the two PDUs of a rack becomes one server.
   A name that starts with `N` followed by the unit (`N200U43`) is shown as a network device. Outlets still named `Outlet_N` are not servers.

## For developers

* `Sources/PDUCore` – SNMP v1 (BER codec and UDP client, no dependencies), vendor drivers, rack model, restart sequence. Builds and is tested on Linux and macOS: `swift test`.
* `Sources/PDUMonitor` – the SwiftUI application (macOS only).
* `Tests/PDUCoreTests` – the drivers are tested end to end over real UDP against a fake SNMP agent. OIDs come from the vendor MIB files (CPS-MIB, PowerNet-MIB).
* `scripts/build-app.sh` builds the Universal `.app` and zip. CI: `.github/workflows/macos.yml`.
* History for graphs: `HistoryStore` is called after every poll today with a no-op implementation; a database can be plugged in without changing the callers.
