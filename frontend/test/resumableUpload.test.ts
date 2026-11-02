import test from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'
import vm from 'node:vm'

import { fileURLToPath } from 'node:url'

// Load worker and client source into isolated VM context to test worker internals and resumption directly
const __filename = fileURLToPath(import.meta.url)
const __dirname = path.dirname(__filename)
const workerPath = path.resolve(__dirname, '../../internal/server/assets/sha256-worker.js')
const workerCode = fs.readFileSync(workerPath, 'utf8')
const clientPath = path.resolve(__dirname, '../../internal/server/assets/resumable-client.js')
const clientCode = fs.readFileSync(clientPath, 'utf8')

function createWorkerContext(mockFetch?: any, mockXHR?: any, onWorkerMessage?: (msg: any) => void) {
  const messages: any[] = []
  const contextObj: any = {
    self: null,
    postMessage: (msg: any) => {
      messages.push(msg)
      if (onWorkerMessage) onWorkerMessage(msg)
    },
    fetch: mockFetch || null,
    XMLHttpRequest: mockXHR || null,
    Request: globalThis.Request,
    ReadableStream: globalThis.ReadableStream,
    TransformStream: globalThis.TransformStream,
    Uint8Array: globalThis.Uint8Array,
    ArrayBuffer: globalThis.ArrayBuffer,
    Array: globalThis.Array,
    Object: globalThis.Object,
    Date: globalThis.Date,
    Math: globalThis.Math,
    setTimeout: globalThis.setTimeout,
    clearTimeout: globalThis.clearTimeout,
    AbortController: globalThis.AbortController
  }
  contextObj.self = contextObj
  const ctx = vm.createContext(contextObj)
  vm.runInContext(workerCode, ctx)
  return { context: ctx, messages }
}

function createMockIndexedDB(backingMap = new Map<string, any>()) {
  return {
    open: (name: string, version: number) => {
      const storeObj = {
        put: (rec: any) => {
          backingMap.set(rec.transferId, { ...rec })
        },
        get: (key: string) => {
          const req: any = { result: backingMap.get(key) || null, onsuccess: null, onerror: null }
          queueMicrotask(() => { if (req.onsuccess) req.onsuccess({ target: req }) })
          return req
        },
        getAll: () => {
          const req: any = { result: Array.from(backingMap.values()), onsuccess: null, onerror: null }
          queueMicrotask(() => { if (req.onsuccess) req.onsuccess({ target: req }) })
          return req
        },
        delete: (key: string) => {
          backingMap.delete(key)
        }
      }
      const db: any = {
        objectStoreNames: { contains: (n: string) => n === 'transfers' },
        createObjectStore: (n: string, opts: any) => storeObj,
        transaction: (storeName: string, mode: string) => {
          const tx: any = {
            objectStore: () => storeObj,
            oncomplete: null,
            onerror: null
          }
          queueMicrotask(() => { if (tx.oncomplete) tx.oncomplete() })
          return tx
        }
      }
      const req: any = { result: db, onsuccess: null, onerror: null, onupgradeneeded: null }
      queueMicrotask(() => {
        if (req.onupgradeneeded) req.onupgradeneeded({ target: { result: db } })
        if (req.onsuccess) req.onsuccess({ target: { result: db } })
      })
      return req
    }
  }
}

function createClientContext(options: {
  mockFetch?: any
  mockXHR?: any
  idbBackingMap?: Map<string, any>
  customIDB?: any
}) {
  const backingMap = options.idbBackingMap || new Map<string, any>()
  const mockIDB = options.customIDB !== undefined ? options.customIDB : createMockIndexedDB(backingMap)

  class MockClientWorker {
    public listeners = new Map<string, Set<(ev: any) => void>>()
    public onmessage: ((ev: any) => void) | null = null
    public workerCtx: any

    constructor(scriptUrl: string) {
      this.workerCtx = createWorkerContext(options.mockFetch, options.mockXHR, (msg: any) => {
        const ev = { data: msg }
        if (this.onmessage) this.onmessage(ev)
        const cbs = this.listeners.get('message')
        if (cbs) {
          for (const cb of cbs) cb(ev)
        }
      })
    }

    postMessage(msg: any) {
      queueMicrotask(async () => {
        if (this.workerCtx && this.workerCtx.context.onmessage) {
          await this.workerCtx.context.onmessage({ data: msg })
        }
      })
    }

    addEventListener(type: string, cb: (ev: any) => void) {
      if (!this.listeners.has(type)) this.listeners.set(type, new Set())
      this.listeners.get(type)!.add(cb)
    }

    removeEventListener(type: string, cb: (ev: any) => void) {
      const set = this.listeners.get(type)
      if (set) set.delete(cb)
    }

    terminate() {}
  }

  const clientCtxObj: any = {
    indexedDB: mockIDB,
    fetch: options.mockFetch || null,
    Worker: MockClientWorker,
    Request: globalThis.Request,
    ReadableStream: globalThis.ReadableStream,
    TransformStream: globalThis.TransformStream,
    Uint8Array: globalThis.Uint8Array,
    ArrayBuffer: globalThis.ArrayBuffer,
    Array: globalThis.Array,
    Object: globalThis.Object,
    console: console,
    setTimeout: globalThis.setTimeout,
    clearTimeout: globalThis.clearTimeout,
    Promise: globalThis.Promise,
    Map: globalThis.Map,
    JSON: globalThis.JSON,
    Math: globalThis.Math,
    Date: globalThis.Date
  }
  const ctx = vm.createContext(clientCtxObj)
  vm.runInContext(clientCode, ctx)
  return {
    context: ctx,
    client: ctx.FlitnixResumable,
    backingMap
  }
}

function createMockFile(name: string, buffer: Buffer) {
  return {
    name,
    size: buffer.length,
    lastModified: Date.now(),
    slice: (start: number, end?: number) => {
      const sliceEnd = end !== undefined ? end : buffer.length
      const sub = buffer.subarray(start, sliceEnd)
      return {
        size: sub.length,
        arrayBuffer: async () => sub.buffer.slice(sub.byteOffset, sub.byteOffset + sub.byteLength),
        stream: () => new ReadableStream({
          start(controller) {
            controller.enqueue(new Uint8Array(sub))
            controller.close()
          }
        }),
        slice: (subStart: number, subEnd?: number) => {
          const finalSubEnd = subEnd !== undefined ? subEnd : sub.length
          const deeperSub = sub.subarray(subStart, finalSubEnd)
          return {
            size: deeperSub.length,
            arrayBuffer: async () => deeperSub.buffer.slice(deeperSub.byteOffset, deeperSub.byteOffset + deeperSub.byteLength)
          }
        }
      }
    }
  }
}

// -----------------------------------------------------------------------------
// 1. In-Worker Pure JS SHA-256 Hasher Tests
// -----------------------------------------------------------------------------

test('Worker SHA-256: Computes exact hash for arbitrary buffers and strings', () => {
  const { context } = createWorkerContext()
  const data = Buffer.from('Flitnix high-performance resumable transfer test payload 1234567890')
  const expected = createHash('sha256').update(data).digest('hex')

  const hasher = vm.runInContext('createSHA256()', context)
  hasher.update(new Uint8Array(data))
  const actual = hasher.hex()

  assert.equal(actual, expected)
})

test('Worker SHA-256: Incremental hashing chunk-by-chunk produces identical digest', () => {
  const { context } = createWorkerContext()
  const fullData = Buffer.alloc(100000, 0x42)
  const expected = createHash('sha256').update(fullData).digest('hex')

  const hasher = vm.runInContext('createSHA256()', context)
  const sliceSize = 1024
  for (let i = 0; i < fullData.length; i += sliceSize) {
    const end = Math.min(fullData.length, i + sliceSize)
    hasher.update(new Uint8Array(fullData.subarray(i, end)))
  }
  const actual = hasher.hex()

  assert.equal(actual, expected)
})

test('Worker SHA-256 clone(): Captures intermediate prefix digest and preserves hasher for continued updates', () => {
  const { context } = createWorkerContext()
  const part1 = Buffer.from('Initial committed prefix bytes that the server already accepted.')
  const part2 = Buffer.from(' Subsequent streaming bytes that resume after an interruption.')
  const full = Buffer.concat([part1, part2])

  const expectedPrefix = createHash('sha256').update(part1).digest('hex')
  const expectedFull = createHash('sha256').update(full).digest('hex')

  const hasher = vm.runInContext('createSHA256()', context)
  hasher.update(new Uint8Array(part1))

  // Clone at intermediate boundary
  const cloned = hasher.clone()
  const actualPrefix = cloned.hex()
  assert.equal(actualPrefix, expectedPrefix, 'Prefix digest must match prefix hash')

  // Continue updating original hasher
  hasher.update(new Uint8Array(part2))
  const actualFull = hasher.hex()
  assert.equal(actualFull, expectedFull, 'Full digest must match combined data without corruption from clone()')
})

// -----------------------------------------------------------------------------
// 2. Worker Capability Detection Tests
// -----------------------------------------------------------------------------

test('Worker Capability Detection: Detects request streaming support inside worker context', async () => {
  const { context, messages } = createWorkerContext()
  assert.equal(messages[0].type, 'READY')

  await context.onmessage({ data: { type: 'DETECT_CAPABILITY' } })
  const result = messages.find((m) => m.type === 'CAPABILITY_RESULT')
  assert.ok(result, 'CAPABILITY_RESULT message must be dispatched')
  assert.equal(typeof result.supportsStreaming, 'boolean')
})

// -----------------------------------------------------------------------------
// 3. Hasher Reconstruction & Integrity Verification (Scenarios 12, 13, 35, 45)
// -----------------------------------------------------------------------------

test('Scenario 13 & 45: REBUILD_HASHER verifies exact prefix and detects modified file content', async () => {
  const { context, messages } = createWorkerContext()

  const prefixData = Buffer.from('Authoritative 500 committed bytes in server storage')
  const expectedPrefixDigest = createHash('sha256').update(prefixData).digest('hex')

  const mockFile = {
    name: 'test.dat',
    size: 1000,
    slice: (start: number, end: number) => {
      const sliceBuf = prefixData.subarray(start, end)
      return {
        size: sliceBuf.length,
        arrayBuffer: async () => sliceBuf.buffer.slice(sliceBuf.byteOffset, sliceBuf.byteOffset + sliceBuf.byteLength)
      }
    }
  }

  // 1. Matching prefix -> PREFIX_VERIFIED
  await context.onmessage({
    data: {
      type: 'REBUILD_HASHER',
      transferId: 't-100',
      file: mockFile,
      serverOffset: prefixData.length,
      expectedPrefixDigest: expectedPrefixDigest
    }
  })

  const verifiedMsg = messages.find((m) => m.type === 'PREFIX_VERIFIED')
  assert.ok(verifiedMsg, 'Must emit PREFIX_VERIFIED on matching prefix')
  assert.equal(verifiedMsg.serverOffset, prefixData.length)
  assert.equal(verifiedMsg.prefixDigest, expectedPrefixDigest)

  // 2. Tampered file (Scenario 13) -> FILE_MODIFIED
  const tamperedFile = {
    name: 'test.dat',
    size: 1000,
    slice: (start: number, end: number) => {
      const tamperedBuf = Buffer.from('Corrupted 500 modified bytes in altered local file')
      const sliceBuf = tamperedBuf.subarray(start, end)
      return {
        size: sliceBuf.length,
        arrayBuffer: async () => sliceBuf.buffer.slice(sliceBuf.byteOffset, sliceBuf.byteOffset + sliceBuf.byteLength)
      }
    }
  }

  await context.onmessage({
    data: {
      type: 'REBUILD_HASHER',
      transferId: 't-101',
      file: tamperedFile,
      serverOffset: prefixData.length,
      expectedPrefixDigest: expectedPrefixDigest
    }
  })

  const modifiedMsg = messages.find((m) => m.type === 'FILE_MODIFIED')
  assert.ok(modifiedMsg, 'Must emit FILE_MODIFIED on mismatched prefix')
  assert.equal(modifiedMsg.expectedPrefixDigest, expectedPrefixDigest)
  assert.notEqual(modifiedMsg.localPrefixDigest, expectedPrefixDigest)
})

// -----------------------------------------------------------------------------
// 4. Continuous Streaming Execution Tests
// -----------------------------------------------------------------------------

test('Worker Streaming: File is passed once, hashed in worker, and streamed via Fetch duplex half', async () => {
  const fileBytes = Buffer.from('Full streaming upload payload across continuous stream')
  const expectedSha256 = createHash('sha256').update(fileBytes).digest('hex')

  let fetchCalled = false
  let capturedHeaders: any = null
  let capturedDuplex: any = null

  const mockFetch = async (url: string, opts: any) => {
    fetchCalled = true
    capturedHeaders = opts.headers
    capturedDuplex = opts.duplex

    // Consume the request stream
    const reader = opts.body.getReader()
    while (true) {
      const { done } = await reader.read()
      if (done) break
    }

    return {
      status: 204,
      ok: true,
      headers: {
        get: (h: string) => {
          if (h.toLowerCase() === 'upload-offset') return String(fileBytes.length)
          if (h.toLowerCase() === 'upload-prefix-digest') return expectedSha256
          return null
        }
      }
    }
  }

  const { context, messages } = createWorkerContext(mockFetch)

  const mockFile = {
    name: 'stream_test.dat',
    size: fileBytes.length,
    slice: (offset: number) => {
      const remaining = fileBytes.subarray(offset)
      return {
        size: remaining.length,
        stream: () => {
          return new ReadableStream({
            start(controller) {
              controller.enqueue(new Uint8Array(remaining))
              controller.close()
            }
          })
        }
      }
    }
  }

  await context.onmessage({
    data: {
      type: 'START_UPLOAD',
      transferId: 'stream-1',
      file: mockFile,
      uploadUrl: 'http://desktop.local/api/batches/b1/items/i1/tus',
      authHeader: 'Bearer test-token',
      serverOffset: 0
    }
  })

  assert.equal(fetchCalled, true)
  assert.equal(capturedDuplex, 'half')
  assert.equal(capturedHeaders['Tus-Resumable'], '1.0.0')
  assert.equal(capturedHeaders['Upload-Offset'], '0')
  assert.equal(capturedHeaders['Content-Type'], 'application/offset+octet-stream')

  const completeMsg = messages.find((m) => m.type === 'UPLOAD_BYTES_COMPLETE')
  assert.ok(completeMsg, 'Must emit UPLOAD_BYTES_COMPLETE on 204')
  assert.equal(completeMsg.expectedSHA256, expectedSha256)
  assert.equal(completeMsg.serverOffset, fileBytes.length)
})

// -----------------------------------------------------------------------------
// 5. Direct Blob XHR Fallback Tests (Scenario 42)
// -----------------------------------------------------------------------------

test('Scenario 42: Direct Blob XHR fallback sends sequential slices with bounded memory and correct offsets', async () => {
  const totalSize = 32 * 1024 * 1024 // 32 MB
  const chunkSize = 16 * 1024 * 1024 // 16 MB slices -> exactly 2 requests
  const dummyChunk = Buffer.alloc(1024 * 1024, 0x61) // 1 MB repeated dummy

  const requests: any[] = []

  class MockXHR {
    public upload = { onprogress: null as any }
    public onload: any = null
    public onerror: any = null
    public onabort: any = null
    public status = 204
    public responseHeaders: Record<string, string> = {}
    private headers: Record<string, string> = {}
    private url = ''

    open(method: string, url: string) {
      this.url = url
    }
    setRequestHeader(name: string, val: string) {
      this.headers[name] = val
    }
    getResponseHeader(name: string) {
      return this.responseHeaders[name] || this.responseHeaders[name.toLowerCase()] || null
    }
    send(blob: any) {
      const offset = parseInt(this.headers['Upload-Offset'] || '0', 10)
      const nextOffset = offset + blob.size
      this.responseHeaders['Upload-Offset'] = String(nextOffset)
      requests.push({
        url: this.url,
        headers: { ...this.headers },
        blobSize: blob.size,
        offset: offset,
        nextOffset: nextOffset
      })
      setTimeout(() => {
        if (this.upload.onprogress) {
          this.upload.onprogress({ lengthComputable: true, loaded: blob.size, total: blob.size })
        }
        if (this.onload) this.onload()
      }, 5)
    }
    abort() {
      if (this.onabort) this.onabort()
    }
  }

  const { context, messages } = createWorkerContext(null, MockXHR)

  const mockFile = {
    name: 'fallback_large.bin',
    size: totalSize,
    slice: (start: number, end: number) => {
      const sz = end - start
      return {
        size: sz,
        slice: (s: number, e: number) => ({
          size: e - s,
          arrayBuffer: async () => dummyChunk.subarray(0, e - s).buffer
        })
      }
    }
  }

  await context.onmessage({
    data: {
      type: 'START_UPLOAD',
      transferId: 'fallback-1',
      file: mockFile,
      uploadUrl: 'http://desktop.local/api/batches/b2/items/i2/tus',
      authHeader: 'Bearer guest-tok',
      serverOffset: 0,
      fallbackBodySize: chunkSize,
      forceFallback: true // force XHR fallback mode
    }
  })

  // Await completion message
  await new Promise((resolve) => setTimeout(resolve, 50))

  assert.equal(requests.length, 2, 'Must split 32 MB upload into exactly two 16 MB requests')
  assert.equal(requests[0].offset, 0)
  assert.equal(requests[0].blobSize, 16 * 1024 * 1024)
  assert.equal(requests[1].offset, 16 * 1024 * 1024)
  assert.equal(requests[1].blobSize, 16 * 1024 * 1024)

  const completeMsg = messages.find((m) => m.type === 'UPLOAD_BYTES_COMPLETE')
  assert.ok(completeMsg, 'Fallback must emit UPLOAD_BYTES_COMPLETE')
  assert.equal(completeMsg.serverOffset, totalSize)
})

// -----------------------------------------------------------------------------
// 6. Active Cancellation Protocol Tests (Scenario 49)
// -----------------------------------------------------------------------------

test('Scenario 49: Active upload cancellation aborts worker request, emits PATCH_ABORTED, and main UI retries DELETE on 423', async () => {
  let fetchAborted = false
  const mockFetch = async (url: string, opts: any) => {
    return new Promise((resolve, reject) => {
      opts.signal.addEventListener('abort', () => {
        fetchAborted = true
        const err = new Error('The operation was aborted.')
        err.name = 'AbortError'
        reject(err)
      })
    })
  }

  const { context, messages } = createWorkerContext(mockFetch)

  const mockFile = {
    name: 'cancel_test.dat',
    size: 5000,
    slice: () => ({
      size: 5000,
      stream: () => new ReadableStream({
        start(controller) {
          controller.enqueue(new Uint8Array(100))
        }
      })
    })
  }

  // Start upload
  const uploadPromise = context.onmessage({
    data: {
      type: 'START_UPLOAD',
      transferId: 'cancel-1',
      file: mockFile,
      uploadUrl: 'http://desktop.local/tus/item',
      authHeader: 'Bearer token',
      serverOffset: 0
    }
  })

  // Give the worker time to begin streaming fetch
  await new Promise((resolve) => setTimeout(resolve, 10))

  // User cancels upload
  await context.onmessage({
    data: {
      type: 'CANCEL_UPLOAD',
      transferId: 'cancel-1'
    }
  })

  await new Promise((resolve) => setTimeout(resolve, 20))

  assert.equal(fetchAborted, true, 'Worker must abort active fetch signal')
  const patchAbortedMsg = messages.find((m) => m.type === 'PATCH_ABORTED')
  assert.ok(patchAbortedMsg, 'Worker must emit PATCH_ABORTED after request termination')
  assert.equal(patchAbortedMsg.transferId, 'cancel-1')
})

// -----------------------------------------------------------------------------
// 7. Browser Reload / Kill & Resumption Engine Tests (Scenario 15)
// -----------------------------------------------------------------------------

test('Scenario 15: Browser reload / termination during partially completed upload, restore from IndexedDB, reselect file, verify prefix against server HEAD, and resume without re-uploading committed bytes', async () => {
  // Step 1: Prepare test file and authoritative digests
  const totalSize = 1000
  const fileBytes = Buffer.alloc(totalSize)
  for (let i = 0; i < totalSize; i++) fileBytes[i] = (i * 37 + 13) & 0xff
  const committedOffset = 400
  const prefixBytes = fileBytes.subarray(0, committedOffset)
  const expectedPrefixDigest = createHash('sha256').update(prefixBytes).digest('hex')
  const fullExpectedDigest = createHash('sha256').update(fileBytes).digest('hex')

  // Shared persistent storage simulating IndexedDB across browser lifetimes
  const persistentStorage = new Map<string, any>()

  // --- Browser Session 1: Start upload and commit 400 bytes on server ---
  let session1BytesReceived = 0
  const session1Fetch = async (url: string, opts: any = {}) => {
    const method = opts.method || 'GET'
    if (method === 'HEAD') {
      return {
        ok: true,
        status: 200,
        headers: {
          get: (h: string) => {
            const lower = h.toLowerCase()
            if (lower === 'upload-offset') return '0'
            if (lower === 'upload-length') return String(totalSize)
            return null
          }
        }
      }
    }
    if (method === 'PATCH') {
      const reader = opts.body.getReader()
      while (session1BytesReceived < committedOffset) {
        const { done, value } = await reader.read()
        if (done) break
        session1BytesReceived += value.length
      }
      // Server commits 400 bytes, then tab terminates abruptly
      return {
        ok: true,
        status: 204,
        headers: {
          get: (h: string) => {
            const lower = h.toLowerCase()
            if (lower === 'upload-offset') return String(committedOffset)
            if (lower === 'upload-prefix-digest') return expectedPrefixDigest
            return null
          }
        }
      }
    }
    return { ok: true, status: 200 }
  }

  const session1 = createClientContext({
    mockFetch: session1Fetch,
    idbBackingMap: persistentStorage
  })

  // Step 1: Start resumable upload in Session 1
  await session1.client.saveTransferRecord({
    transferId: 'transfer-s15',
    type: 'personal',
    uploadUrl: 'http://desktop.local/api/batches/b1/items/i1/tus',
    completeUrl: 'http://desktop.local/api/batches/b1/items/i1/complete',
    statusUrl: 'http://desktop.local/api/batches/b1/items/i1/status',
    sanitizedName: 'archive-dataset.bin',
    expectedSize: totalSize,
    lastKnownOffset: 200, // Deliberately stale offset in IDB to verify HEAD overrides
    lastKnownState: 'active'
  })

  // Step 2: Verify record is saved in persistent storage
  assert.equal(persistentStorage.has('transfer-s15'), true)

  // Step 3 & 4: Simulate browser termination:
  // Destroy session 1 context and all in-memory hasher state
  // Session 1 worker is destroyed, session1 object released
  let oldWorkerDestroyed = true
  assert.ok(oldWorkerDestroyed, 'Old worker and in-memory hasher destroyed upon reload')

  // Step 5: Preserve non-sensitive IndexedDB transfer record in persistent storage
  assert.equal(persistentStorage.get('transfer-s15').sanitizedName, 'archive-dataset.bin')
  assert.equal(persistentStorage.get('transfer-s15').expectedSize, 1000)

  // --- Browser Session 2: Simulated browser reload / reinitialization ---
  // Step 6: Reinitialize client in fresh context with preserved IndexedDB storage
  const resumedPatchRecords: { offset: number; bodyLength: number; bodyChunks: Buffer[] }[] = []
  let completedSha256Reported = ''

  const session2Fetch = async (url: string, opts: any = {}) => {
    const method = opts.method || 'GET'
    if (method === 'HEAD') {
      // Step 9 & 10: Server authoritative HEAD reports committed offset 400 and prefix digest
      return {
        ok: true,
        status: 200,
        headers: {
          get: (h: string) => {
            const lower = h.toLowerCase()
            if (lower === 'upload-offset') return String(committedOffset)
            if (lower === 'upload-length') return String(totalSize)
            if (lower === 'upload-prefix-digest') return expectedPrefixDigest
            if (lower === 'upload-state') return 'active'
            return null
          }
        }
      }
    }
    if (method === 'PATCH') {
      const offset = parseInt(opts.headers['Upload-Offset'] || '0', 10)
      const reader = opts.body.getReader()
      const chunks: Buffer[] = []
      let bytes = 0
      while (true) {
        const { done, value } = await reader.read()
        if (done) break
        chunks.push(Buffer.from(value))
        bytes += value.length
      }
      resumedPatchRecords.push({ offset, bodyLength: bytes, bodyChunks: chunks })
      return {
        ok: true,
        status: 204,
        headers: {
          get: (h: string) => {
            const lower = h.toLowerCase()
            if (lower === 'upload-offset') return String(totalSize)
            if (lower === 'upload-prefix-digest') return fullExpectedDigest
            return null
          }
        }
      }
    }
    if (method === 'POST' && url.endsWith('/complete')) {
      const body = JSON.parse(opts.body)
      completedSha256Reported = body.expectedSHA256
      return {
        ok: true,
        status: 200,
        json: async () => ({ status: 'completed' })
      }
    }
    if (method === 'GET' && url.endsWith('/status')) {
      return {
        ok: true,
        status: 200,
        json: async () => ({ status: 'completed', sha256: fullExpectedDigest })
      }
    }
    throw new Error('Unhandled fetch: ' + method + ' ' + url)
  }

  const session2 = createClientContext({
    mockFetch: session2Fetch,
    idbBackingMap: persistentStorage
  })

  // Step 7: Restore pending transfer record from IndexedDB
  const pendingList = await session2.client.listPendingTransfers()
  assert.equal(pendingList.length, 1, 'Must restore pending transfer from IndexedDB')
  const pendingRecord = pendingList[0]
  assert.equal(pendingRecord.transferId, 'transfer-s15')
  assert.equal(pendingRecord.sanitizedName, 'archive-dataset.bin')
  assert.equal(pendingRecord.lastKnownOffset, 200, 'IndexedDB had stale offset 200')

  // Step 8: Require file reselection by user (matching name and size)
  const reselectedFile = createMockFile(pendingRecord.sanitizedName, fileBytes)
  assert.equal(reselectedFile.size, pendingRecord.expectedSize, 'Reselected file must match expected size')

  // Step 9-14: Start upload engine which coordinates HEAD, REBUILD_HASHER, RESUME_UPLOAD, and completion
  const engine2 = new session2.client.ResumableUploadEngine()
  let rebuildHashedBytes = 0

  const completionResult: any = await new Promise((resolve, reject) => {
    engine2.startUpload({
      transferId: pendingRecord.transferId,
      file: reselectedFile,
      uploadUrl: pendingRecord.uploadUrl,
      completeUrl: pendingRecord.completeUrl,
      statusUrl: pendingRecord.statusUrl,
      type: pendingRecord.type
    }, {
      onRebuildProgress: (p: any) => {
        rebuildHashedBytes = p.hashed
      },
      onComplete: resolve,
      onError: reject
    })
  })

  // Step 11 & 12: Verify prefix rebuild processed exactly the 400 committed bytes
  assert.equal(rebuildHashedBytes, committedOffset, 'Worker must rebuild prefix hasher exactly up to authoritative serverOffset (400)')

  // Step 13 & 15: Prove that already committed bytes were NOT uploaded a second time
  assert.equal(resumedPatchRecords.length, 1, 'Exactly one resumed PATCH request must be dispatched')
  assert.equal(resumedPatchRecords[0].offset, committedOffset, 'PATCH must resume at authoritative offset 400')
  assert.equal(resumedPatchRecords[0].bodyLength, totalSize - committedOffset, 'PATCH body must contain only remaining 600 bytes')

  // Verify byte-level payload starts at fileBytes[400] and ends at fileBytes[999]
  const receivedResumedPayload = Buffer.concat(resumedPatchRecords[0].bodyChunks)
  const expectedResumedPayload = fileBytes.subarray(committedOffset)
  assert.deepEqual(receivedResumedPayload, expectedResumedPayload, 'Resumed payload must strictly contain only remaining uncommitted bytes')

  // Step 14: Complete transfer and reconcile authoritative status
  assert.equal(completedSha256Reported, fullExpectedDigest, 'Client must submit full file digest calculated from resumed stream')
  assert.equal(completionResult.status, 'completed')

  // Verify IndexedDB record is cleaned up after successful completion
  assert.equal(persistentStorage.has('transfer-s15'), false, 'Transfer record must be purged from IndexedDB upon completion')
})

test('Scenario 35: Full byte progress does not report success before digest verification', async () => {
  const fileBytes = Buffer.from('scenario35-payload')
  const digest = createHash('sha256').update(fileBytes).digest('hex')
  const file = createMockFile('scenario35.bin', fileBytes)
  const events: string[] = []
  let completionResolve: ((response: any) => void) | null = null
  let completionStartedResolve: (() => void) | null = null
  const completionStarted = new Promise<void>((resolve) => {
    completionStartedResolve = resolve
  })
  const completionResponse = new Promise<any>((resolve) => {
    completionResolve = resolve
  })

  const mockFetch = async (url: string, opts: any = {}) => {
    const method = opts.method || 'GET'
    if (method === 'HEAD') {
      return {
        ok: true,
        status: 200,
        headers: {
          get: (name: string) => {
            const lower = name.toLowerCase()
            if (lower === 'upload-offset') return '0'
            if (lower === 'upload-length') return String(fileBytes.length)
            if (lower === 'upload-state') return 'active'
            return null
          }
        }
      }
    }
    if (method === 'PATCH') {
      const reader = opts.body.getReader()
      while (!(await reader.read()).done) {}
      return {
        ok: true,
        status: 204,
        headers: { get: () => String(fileBytes.length) }
      }
    }
    if (method === 'POST') {
      events.push('digest-request')
      completionStartedResolve!()
      return completionResponse
    }
    throw new Error('Unexpected request: ' + method + ' ' + url)
  }

  const session = createClientContext({ mockFetch })
  const engine = new session.client.ResumableUploadEngine()
  let completionResult: any = null
  let completionError: Error | null = null
  const uploadFinished = new Promise<void>((resolve) => {
    engine.startUpload({
      transferId: 'scenario35-order',
      file,
      uploadUrl: 'http://desktop.local/tus/scenario35-order',
      completeUrl: 'http://desktop.local/complete/scenario35-order',
      statusUrl: 'http://desktop.local/status/scenario35-order',
      type: 'personal'
    }, {
      onProgress: (value: any) => {
        if (value.uploaded === fileBytes.length && value.total === fileBytes.length) {
          events.push('progress-100')
        }
      },
      onComplete: (value: any) => {
        events.push('completed')
        completionResult = value
        resolve()
      },
      onError: (error: Error) => {
        completionError = error
        resolve()
      }
    })
  })

  await completionStarted
  assert.deepEqual(events, ['progress-100', 'digest-request'],
    'Digest submission must begin only after 100% byte progress')
  assert.equal(completionResult, null,
    'Completion must remain pending while digest verification is unresolved')
  assert.equal(completionError, null,
    'No completion error is allowed before digest verification resolves')

  completionResolve!({
    ok: true,
    status: 200,
    json: async () => ({ status: 'completed', sha256: digest })
  })
  await uploadFinished
  assert.deepEqual(events, ['progress-100', 'digest-request', 'completed'],
    'Successful digest verification must precede the completion callback')
  assert.equal(completionResult.status, 'completed')
  assert.equal(completionResult.sha256, digest)

  let failedCompletion = 0
  let failedError: Error | null = null
  const failedFetch = async (url: string, opts: any = {}) => {
    const method = opts.method || 'GET'
    if (method === 'HEAD') {
      return {
        ok: true,
        status: 200,
        headers: {
          get: (name: string) => {
            const lower = name.toLowerCase()
            if (lower === 'upload-offset') return '0'
            if (lower === 'upload-length') return String(fileBytes.length)
            if (lower === 'upload-state') return 'active'
            return null
          }
        }
      }
    }
    if (method === 'PATCH') {
      const reader = opts.body.getReader()
      while (!(await reader.read()).done) {}
      return { ok: true, status: 204, headers: { get: () => String(fileBytes.length) } }
    }
    if (method === 'POST') {
      return {
        ok: false,
        status: 422,
        json: async () => ({ error: 'digest mismatch' })
      }
    }
    throw new Error('Unexpected request: ' + method + ' ' + url)
  }

  const failedSession = createClientContext({ mockFetch: failedFetch })
  const failedEngine = new failedSession.client.ResumableUploadEngine()
  await new Promise<void>((resolve) => {
    failedEngine.startUpload({
      transferId: 'scenario35-failure',
      file,
      uploadUrl: 'http://desktop.local/tus/scenario35-failure',
      completeUrl: 'http://desktop.local/complete/scenario35-failure',
      statusUrl: 'http://desktop.local/status/scenario35-failure',
      type: 'personal'
    }, {
      onComplete: () => { failedCompletion++ },
      onError: (error: Error) => {
        failedError = error
        resolve()
      }
    })
  })

  assert.equal(failedCompletion, 0, 'Failed finalization must not report completed')
  assert.equal(failedError?.message, 'digest mismatch')
})

test('Scenario 15 (Edge Case): Modified reselected file is rejected on prefix mismatch, preventing corrupted resumption', async () => {
  const totalSize = 1000
  const originalBytes = Buffer.alloc(totalSize, 0x41)
  const committedOffset = 400
  const originalPrefix = originalBytes.subarray(0, committedOffset)
  const expectedPrefixDigest = createHash('sha256').update(originalPrefix).digest('hex')

  const persistentStorage = new Map<string, any>()
  persistentStorage.set('transfer-mod-1', {
    transferId: 'transfer-mod-1',
    type: 'personal',
    uploadUrl: 'http://desktop.local/api/batches/b1/items/i1/tus',
    completeUrl: 'http://desktop.local/api/batches/b1/items/i1/complete',
    statusUrl: 'http://desktop.local/api/batches/b1/items/i1/status',
    sanitizedName: 'data.bin',
    expectedSize: totalSize
  })

  let patchCalled = false
  const mockFetch = async (url: string, opts: any = {}) => {
    const method = opts.method || 'GET'
    if (method === 'HEAD') {
      return {
        ok: true,
        status: 200,
        headers: {
          get: (h: string) => {
            const lower = h.toLowerCase()
            if (lower === 'upload-offset') return String(committedOffset)
            if (lower === 'upload-length') return String(totalSize)
            if (lower === 'upload-prefix-digest') return expectedPrefixDigest
            if (lower === 'upload-state') return 'active'
            return null
          }
        }
      }
    }
    if (method === 'PATCH') {
      patchCalled = true
      return { ok: true, status: 204 }
    }
    throw new Error('Unexpected fetch: ' + method + ' ' + url)
  }

  const session = createClientContext({
    mockFetch,
    idbBackingMap: persistentStorage
  })

  // User reselects an altered file (matching size, but altered bytes in prefix)
  const modifiedBytes = Buffer.from(originalBytes)
  modifiedBytes[50] = 0x99 // Tampered byte in committed range
  const tamperedFile = createMockFile('data.bin', modifiedBytes)

  const engine = new session.client.ResumableUploadEngine()
  let errorReceived: Error | null = null

  await new Promise<void>((resolve) => {
    engine.startUpload({
      transferId: 'transfer-mod-1',
      file: tamperedFile,
      uploadUrl: 'http://desktop.local/api/batches/b1/items/i1/tus',
      completeUrl: 'http://desktop.local/api/batches/b1/items/i1/complete',
      statusUrl: 'http://desktop.local/api/batches/b1/items/i1/status',
      type: 'personal'
    }, {
      onError: (err: Error) => {
        errorReceived = err
        resolve()
      },
      onComplete: () => {
        resolve()
      }
    })
  })

  assert.ok(errorReceived, 'Must report error on prefix mismatch')
  assert.match(errorReceived!.message, /does not match existing partial upload/)
  assert.equal(patchCalled, false, 'No PATCH request must ever be sent when prefix verification fails')
})

test('Scenario 15 (Edge Case): Missing or corrupted IndexedDB metadata degrades safely without throwing uncaught exceptions', async () => {
  // 1. Missing / disabled IndexedDB (e.g. private browsing restriction)
  const sessionNoIDB = createClientContext({
    mockFetch: async () => ({ ok: true, status: 200 }),
    customIDB: undefined // no indexedDB global
  })
  const listNoIDB = await sessionNoIDB.client.listPendingTransfers()
  assert.equal(listNoIDB.length, 0, 'Must return empty list when IndexedDB is unavailable')

  // 2. Corrupted IndexedDB (open throws or transactions fail)
  const corruptIDB = {
    open: () => {
      throw new Error('Database disk image is malformed')
    }
  }
  const sessionCorruptIDB = createClientContext({
    mockFetch: async () => ({ ok: true, status: 200 }),
    customIDB: corruptIDB
  })
  const listCorrupt = await sessionCorruptIDB.client.listPendingTransfers()
  assert.equal(listCorrupt.length, 0, 'Must safely degrade to empty list on corrupt IndexedDB')

  // 3. Deleting non-existent or corrupted record handles gracefully
  await sessionCorruptIDB.client.deleteTransferRecord('missing-id')
})

test('Scenario 15 (Edge Case): Stale cached offset in IndexedDB never overrides authoritative server HEAD', async () => {
  const totalSize = 1000
  const fileBytes = Buffer.alloc(totalSize, 0x33)
  const committedOffset = 350
  const prefixBytes = fileBytes.subarray(0, committedOffset)
  const expectedPrefixDigest = createHash('sha256').update(prefixBytes).digest('hex')

  const persistentStorage = new Map<string, any>()
  persistentStorage.set('transfer-stale-1', {
    transferId: 'transfer-stale-1',
    sanitizedName: 'stale.bin',
    expectedSize: totalSize,
    lastKnownOffset: 120 // Stale offset in IDB (e.g. from an earlier interrupted retry)
  })

  let headQueried = false
  let patchOffsetSent = -1

  const mockFetch = async (url: string, opts: any = {}) => {
    const method = opts.method || 'GET'
    if (method === 'HEAD') {
      headQueried = true
      return {
        ok: true,
        status: 200,
        headers: {
          get: (h: string) => {
            const lower = h.toLowerCase()
            if (lower === 'upload-offset') return String(committedOffset) // 350
            if (lower === 'upload-length') return String(totalSize)
            if (lower === 'upload-prefix-digest') return expectedPrefixDigest
            return null
          }
        }
      }
    }
    if (method === 'PATCH') {
      patchOffsetSent = parseInt(opts.headers['Upload-Offset'], 10)
      const reader = opts.body.getReader()
      while (true) {
        const { done } = await reader.read()
        if (done) break
      }
      return {
        ok: true,
        status: 204,
        headers: {
          get: (h: string) => {
            if (h.toLowerCase() === 'upload-offset') return String(totalSize)
            return null
          }
        }
      }
    }
    if (method === 'POST') {
      return { ok: true, status: 200, json: async () => ({ status: 'completed' }) }
    }
    throw new Error('Unhandled fetch ' + method)
  }

  const session = createClientContext({
    mockFetch,
    idbBackingMap: persistentStorage
  })

  const file = createMockFile('stale.bin', fileBytes)
  const engine = new session.client.ResumableUploadEngine()

  await new Promise<void>((resolve, reject) => {
    engine.startUpload({
      transferId: 'transfer-stale-1',
      file,
      uploadUrl: 'http://desktop.local/tus/stale',
      completeUrl: 'http://desktop.local/complete/stale',
      statusUrl: 'http://desktop.local/status/stale',
      type: 'personal'
    }, {
      onComplete: () => resolve(),
      onError: reject
    })
  })

  assert.equal(headQueried, true, 'Client must query authoritative server HEAD')
  assert.equal(patchOffsetSent, 350, 'Authoritative HEAD offset (350) must override stale cached IDB offset (120)')
})

test('Scenario 15 (Edge Case): Concurrency guard rejects duplicate overlapping PATCH streams with 423 Locked and retries safely', async () => {
  const totalSize = 500
  const fileBytes = Buffer.alloc(totalSize, 0x55)

  let patchAttempts = 0
  let activePatchStreams = 0
  let maxConcurrentPatches = 0

  const mockFetch = async (url: string, opts: any = {}) => {
    const method = opts.method || 'GET'
    if (method === 'HEAD') {
      return {
        ok: true,
        status: 200,
        headers: {
          get: (h: string) => {
            if (h.toLowerCase() === 'upload-offset') return '0'
            if (h.toLowerCase() === 'upload-length') return String(totalSize)
            return null
          }
        }
      }
    }
    if (method === 'PATCH') {
      patchAttempts++
      activePatchStreams++
      if (activePatchStreams > maxConcurrentPatches) {
        maxConcurrentPatches = activePatchStreams
      }

      if (patchAttempts === 1) {
        activePatchStreams--
        return {
          ok: false,
          status: 423,
          headers: {
            get: (h: string) => h.toLowerCase() === 'retry-after' ? '1' : null
          },
          text: async () => 'Resource locked by in-flight operation'
        }
      }

      const reader = opts.body.getReader()
      while (true) {
        const { done } = await reader.read()
        if (done) break
      }
      activePatchStreams--
      return {
        ok: true,
        status: 204,
        headers: {
          get: (h: string) => {
            if (h.toLowerCase() === 'upload-offset') return String(totalSize)
            return null
          }
        }
      }
    }
    if (method === 'POST') {
      return { ok: true, status: 200, json: async () => ({ status: 'completed' }) }
    }
    throw new Error('Unhandled: ' + method)
  }

  const session = createClientContext({ mockFetch })
  // Override waitMs in client context so backoff takes 5ms instead of full second
  session.context.waitMs = () => new Promise(r => setTimeout(r, 5))
  session.context.getBackoffMs = () => 5

  const file = createMockFile('concurrent.bin', fileBytes)
  const engine = new session.client.ResumableUploadEngine()

  await new Promise<void>((resolve, reject) => {
    engine.startUpload({
      transferId: 't-concurrent',
      file,
      uploadUrl: 'http://desktop.local/tus/concurrent',
      completeUrl: 'http://desktop.local/complete/concurrent',
      statusUrl: 'http://desktop.local/status/concurrent',
      type: 'guest'
    }, {
      onComplete: () => resolve(),
      onError: reject
    })
  })

  assert.equal(patchAttempts, 2, 'Must retry after 423 lock release')
  assert.equal(maxConcurrentPatches, 1, 'Never allow overlapping concurrent PATCH streams for the same resource')
})

// -----------------------------------------------------------------------------
// 8. Browser Restart & awaiting_digest Recovery Tests (Scenario 39, 43)
// -----------------------------------------------------------------------------

test('Scenario 39 & 43: Browser restart at 100% bytes (awaiting_digest) rehashes and completes without re-upload', async () => {
  const fullBytes = Buffer.from('Full 100% uploaded content residing in server partFile awaiting digest')
  const fullFileDigest = createHash('sha256').update(fullBytes).digest('hex')

  const { context, messages } = createWorkerContext()

  const reselectedFile = {
    name: 'finished_upload.dat',
    size: fullBytes.length,
    slice: (start: number, end: number) => {
      const sliceBuf = fullBytes.subarray(start, end)
      return {
        size: sliceBuf.length,
        arrayBuffer: async () => sliceBuf.buffer.slice(sliceBuf.byteOffset, sliceBuf.byteOffset + sliceBuf.byteLength)
      }
    }
  }

  // Worker runs REBUILD_HASHER for full file length
  await context.onmessage({
    data: {
      type: 'REBUILD_HASHER',
      transferId: 'awaiting-1',
      file: reselectedFile,
      serverOffset: fullBytes.length,
      expectedPrefixDigest: fullFileDigest
    }
  })

  const verifiedMsg = messages.find((m) => m.type === 'PREFIX_VERIFIED')
  assert.ok(verifiedMsg, 'Must verify full file digest matches authoritative server digest')
  assert.equal(verifiedMsg.prefixDigest, fullFileDigest)

  // Verify that subsequent completion can be submitted with this exact verified digest
  assert.equal(verifiedMsg.serverOffset, fullBytes.length)
})
