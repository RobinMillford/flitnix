# Flitnix Transfer Performance Benchmark Report

**Date:** 2026-10-29 07:10:16 UTC<br>
**Environment:** linux/amd64 (16 CPUs) | go1.27.3<br>
**Benchmark Mode:** Local Loopback TCP (`127.0.0.1`) on NVMe storage

## 1. Measured Performance Results (Loopback Microbenchmark)

> [!NOTE]
> These measurements reflect local loopback TCP streaming against fast NVMe storage to isolate and verify Flitnix's chunking pipeline, lock-free progress callbacks, and inline cryptographic SHA-256 verification. Over a physical Wi-Fi network, throughput is governed by 802.11 RF channel capacity, router contention, and phone antenna capabilities (typically 25–80 MB/s).

| Topology | Files | Data Volume | Duration | Throughput | Peak Heap | Integrity Pass |
| :--- | :---: | :---: | :---: | :---: | :---: | :---: |
| **Topology 1: Large File**<br><small>Single 500 MB file streaming transfer</small> | 1 | 500.00 MB | 440 ms | **1136.36 MB/s** | 0.74 MB | true |
| **Topology 2: Many Small Files**<br><small>1,000 files x 20 KB each (metadata & connection turnaround)</small> | 1000 | 19.53 MB | 435 ms | **44.90 MB/s** | 2.69 MB | true |
| **Topology 3: Mixed Hierarchy**<br><small>111 mixed files (50 MB video, 10x5MB clips, 100x100KB documents)</small> | 111 | 109.77 MB | 122 ms | **899.72 MB/s** | 2.38 MB | true |
| **Topology 4: Outbound Single File Download**<br><small>500 MB file download with streaming RFC 9530 Content-Digest</small> | 1 | 500.00 MB | 666 ms | **750.75 MB/s** | 1.01 MB | true |
| **Topology 5: Outbound Folder Archive Download**<br><small>111 files (110 MB) uncompressed ZIP download with deterministic two-pass Content-Digest</small> | 111 | 109.78 MB | 157 ms | **699.25 MB/s** | 1.34 MB | true |

## 2. Architecture & Protocol Analysis vs LocalSend

### Protocol Comparison

| Dimension | Flitnix | LocalSend |
| :--- | :--- | :--- |
| **Transport Layer** | HTTP/1.1 Streaming with Content-Length | HTTPS / REST / Multipart |
| **Two-Phase Commit** | Explicit: Metadata declare -> Desktop approve -> Direct sink stream | Protocol-level offer/accept dialog via REST |
| **Buffer Management** | Bounded streaming ring buffer (64KB chunks), zero in-memory staging | Chunks staged via Dart HTTP client / buffers |
| **Integrity Verification** | Inline SHA-256 computation over sink byte stream without extra passes | SHA-256 computed per chunk / post-transfer |
| **Client Prerequisites** | Zero client install (standard mobile web browser via QR) | App required on both sending and receiving devices |
| **TLS Overhead** | None on local network; optional TLS evaluation documented | Self-signed TLS with fingerprint exchange in app |

### Performance Topology Analysis

1. **Large Single File (500 MB):**
   Flitnix achieves line-rate NVMe disk throughput in loopback testing because bytes stream directly from the HTTP connection socket into the file sink with an inline cryptographic SHA-256 hasher. Heap allocation remains strictly flat (~0.7–2.5 MB) regardless of transfer size.

2. **Many Small Files (1,000 files x 20 KB):**
   Small file performance is dominated by HTTP connection turnaround and metadata verification roundtrips. Flitnix processes 1,000 files in sequential HTTP requests with deterministic sequencing and bounded memory.

3. **Mixed Directory Hierarchy (111 files, 110 MB):**
   Mixed topologies validate directory sanitization, path traversal defense, and interleaved small-file/large-file streams. In all cases, 100% SHA-256 byte integrity is verified.

## 3. Real-World Network Benchmark Procedure

To benchmark Flitnix over a physical local area network between two machines or a phone:
1. **Desktop-to-Desktop Test:**
   - On Machine A: Run `flitnix serve -addr 0.0.0.0:43821`
   - On Machine B: Run `go run ./benchmarks/main.go -server http://<Machine-A-IP>:43821`
2. **Physical Mobile Phone Test:**
   - Start Flitnix on desktop.
   - Scan QR code from mobile browser.
   - Upload a test video (e.g. 200 MB), record elapsed seconds from transfer card timer, and verify matching SHA-256 hash.

### Reproducibility

Run the benchmark suite at any time via:
```bash
./benchmarks/run_benchmarks.sh
```
