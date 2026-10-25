# Stage 4 — Manual end-to-end test plan

Stage 4 wires the full phone↔desktop flow: LAN binding, QR onboarding, the phone
page, and the stage-3 transfer APIs. The automated suite (`go test ./...`,
including `-race`) covers the logic on loopback and the machine's real LAN
address, but nothing automated can replace holding a phone and doing it for
real. This document is the device-run procedure.

**Status: awaiting the first real device run.** The results table at the bottom
is intentionally blank. Nothing here has been verified against a physical phone
yet.

## Test setup

- **Desktop:** Fedora laptop running the stage-4 build
  (`wails build -tags webkit2_41` → `build/bin/flitnix`).
- **Phone:** Android (tested model: Moto Edge 60 Pro) with any modern browser.
- **Network:** the phone's Wi-Fi hotspot, with the laptop connected to it. No
  internet is needed — the phone browser and the desktop only talk to each
  other over the hotspot.

Optional (throughput measurement only): `iperf3` on both ends.

```bash
# Desktop (Fedora):
sudo dnf install iperf3

# Phone (Termux from F-Droid):
pkg install iperf3
```

`iperf3` measures raw network capacity. It does not touch Flitnix and is not
required for the functional pass/fail steps.

## Preconditions

1. `sha256sum build/bin/flitnix` — note the hash of the binary being tested.
2. Know the desktop's current hotspot address; the app shows it in the QR row
   (`http://<laptop-ip>:43821/`). It is whatever the interface select displays
   at run time — never assume a value from a previous run.

## Procedure

### A. Connection

| # | Step | Expected |
|---|------|----------|
| A1 | Start the phone's hotspot; connect the laptop to it | Laptop shows the hotspot as connected |
| A2 | Launch `flitnix` | Connection card appears; the hotspot interface is preselected when exactly one is usable |
| A3 | With 2+ networks connected (e.g. home Wi-Fi **and** hotspot): open the interface select | Choice is required: Start stays disabled until one is explicitly picked; the app never guesses |
| A4 | Connect to home Wi-Fi **only** (no hotspot), then Rescan | A usable home interface gets preselected |
| A5 | Disconnect everything and Rescan | Clear "no usable network found" hint |

### B. Session start and QR

| # | Step | Expected |
|---|------|----------|
| B1 | Select the hotspot interface → **Start session** | QR appears within a moment; the base URL shows `host:port` only (no `#token`), plus an interface/address chip |
| B2 | Verify the QR payload matches the displayed URL + a 43-char fragment token (decode with any QR reader that shows raw text) | `http://<laptop-ip>:43821/#<43-char-base64url>` |
| B3 | **Regenerate QR** while the phone page is open | New QR renders; the token does not change (regeneration redraws the same pairing for the same session) |
| B4 | Start the desktop app again and check the QR row after a restart | QR reappears; base URL again shows the exact `host:port` the server is listening on |

### C. Phone onboarding

| # | Step | Expected |
|---|------|----------|
| C1 | Scan the QR with the phone camera | Browser opens the transfer page; address bar shows no `#fragment` after load |
| C2 | Send the page URL to someone / check the history entry | Link contains neither the token nor its fragment |
| C3 | Reload the page | Still works (token survives in memory for this page load) — but its address-bar copy stays token-free |
| C4 | Open the page **without** a token (new tab, or an old bookmark) | "Scan the QR code" card explains that each link is single-use and a rescan is needed |

### D. Phone → desktop upload

| # | Step | Expected |
|---|------|----------|
| D1 | On the phone: **Pick files**, choose a small file (e.g. a 2 MB photo) | Item appears queued, then "Waiting for approval on the desktop…" |
| D2 | On the desktop: allow it in **Incoming files** | Transfer row goes `uploading` → `completed`; phone shows "Sent" |
| D3 | Verify by SHA-256 (phone: Termux `sha256sum <file>`; desktop: `sha256sum "$HOME/Downloads/Flitnix/<name>"` — check the actual receive directory shown in the app footer) | Hashes are identical; sizes are identical |
| D4 | Repeat with a large file (≥ 1 GiB, both directions) | Progress bar moves; hash matches |
| D5 | On the phone, pick a file, then press **Cancel** during "Waiting…" | Item leaves the queue; desktop shows nothing waiting |
| D6 | On the phone, **Cancel** mid-transfer (large file in flight) | Desktop transfer row becomes `canceled` |
| D7 | On the desktop, deny the incoming file | Phone item shows "Failed" with the desktop's refusal reason |

### E. Desktop → phone download

| # | Step | Expected |
|---|------|----------|
| E1 | On the desktop: **Choose files…**, pick a file | It appears in **Shared files** |
| E2 | On the phone: the file appears in "Files on the desktop"; tap **Download** | Browser saves the file with the original name |
| E3 | Non-ASCII filename (e.g. Unicode characters) rounds through the desktop | Phone saves the name correctly (RFC 5987 handling) |
| E4 | Phone: **Refresh** | List reflects the current share set |
| E5 | Desktop: **Remove** a shared file, phone Refresh | File disappears from the phone list |
| E6 | Verify the downloaded copy's SHA-256 | Matches the original |
| E7 | Repeat E1–E2 with a file that already exists in the phone's download folder with the same name | The browser's own duplicate-name handling applies (it never overwrites a received file) — do not retry after 401 in E2 |

### F. Session end / revocation

| # | Step | Expected |
|---|------|----------|
| F1 | With a second session still available, revoke the first (End on the desktop while the phone page is open) | Phone shows the "Session ended — rescan" state on its next action (401) |
| F2 | Scan again for a fresh session, then press **Stop session** on the desktop (straight URL back to the old page) | With no other session, the server has stopped: the browser gets a connection error, not a 401. Document whichever happens honestly |
| F3 | Old link opened while nothing runs | Reachable at all? No — server down. Either way the page never talk to a dead session |

### G. Throughput (optional, informational)

| # | Step | Expected |
|---|------|----------|
| G1 | On the desktop, start a server-side `iperf3 -s` and from Termux run `iperf3 -c <laptop-ip> -n 1073741824` | Reports ≥ 100 Mbit/s-class hotspot throughput; **record** exact values |
| G2 | Reverse direction: `iperf3 -c <laptop-ip> -n 1073741824 -R` | Record both directions |

## Firewall (read-only; nothing was changed)

On the test machine, firewalld runs with the `FedoraWorkstation` zone on
`wlo1`, which already opens TCP+UDP ports `1025-65535` to that interface —
port 43821 is inside that range, so **no firewall change was made and none is
needed** for this test. If a different zone lacks the range, ask before
changing anything; never disable firewalld.

## Known limitations (recorded, not fixed in stage 4)

- Plain HTTP: the token rides the network unencrypted, which is accepted on
  the phone's own hotspot (documented in the app's warning cards). A full
  security write-up (`docs/SECURITY.md`) is deferred to a later stage.
- One session at a time faces the port: only one pairing QR covers the active
  session; ending the last session closes the server.
- Throughput depends entirely on the hotspot; re-measure rather than trusting
  stage-3 numbers.

## Results (to be filled during the device run)

| Date | Binary SHA-256 | Phone | Hotspot interface/address | D upload hash ok | E download hash ok | F revoke behaviour | G up/down Mbps | Notes |
|------|----------------|-------|---------------------------|------------------|--------------------|--------------------|----------------|-------|
|  |  |  |  |  |  |  |  |  |
