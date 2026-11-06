/**
 * Flitnix Resumable Upload Client Engine (resumable-client.js)
 *
 * Implements:
 * - IndexedDB transfer registry (flitnix_uploads_v1, store: transfers)
 * - Web Worker lifecycle coordinator
 * - Streaming upload with fallback routing
 * - Resumption from server offset with prefix digest verification
 * - Reselection and awaiting_digest recovery (Scenario 39, 43)
 * - Cancellation protocol (Worker abort -> PATCH_ABORTED -> DELETE with 423 Retry-After)
 * - Completion submission and authoritative status polling
 * - Bounded exponential backoff retry with jitter
 */

(function (globalScope) {
  "use strict";

  var DB_NAME = "flitnix_uploads_v1";
  var DB_VERSION = 1;
  var STORE_TRANSFERS = "transfers";

  // ---------------------------------------------------------------------------
  // IndexedDB Non-Sensitive Registry
  // ---------------------------------------------------------------------------
  var dbPromise = null;
  var memoryRegistry = new Map(); // Graceful degradation fallback

  function openDatabase() {
    if (dbPromise) return dbPromise;
    if (typeof indexedDB === "undefined") {
      dbPromise = Promise.resolve(null);
      return dbPromise;
    }

    dbPromise = new Promise(function (resolve) {
      try {
        var req = indexedDB.open(DB_NAME, DB_VERSION);
        req.onupgradeneeded = function (ev) {
          var db = ev.target.result;
          if (!db.objectStoreNames.contains(STORE_TRANSFERS)) {
            db.createObjectStore(STORE_TRANSFERS, { keyPath: "transferId" });
          }
        };
        req.onsuccess = function (ev) {
          resolve(ev.target.result);
        };
        req.onerror = function () {
          // If IndexedDB is blocked (e.g. private mode in some browsers), degrade gracefully
          resolve(null);
        };
      } catch (err) {
        resolve(null);
      }
    });
    return dbPromise;
  }

  async function saveTransferRecord(record) {
    if (!record || !record.transferId) return;
    record.updatedAt = Date.now();
    var db = await openDatabase();
    if (!db) {
      memoryRegistry.set(record.transferId, Object.assign({}, record));
      return;
    }
    return new Promise(function (resolve) {
      try {
        var tx = db.transaction(STORE_TRANSFERS, "readwrite");
        var store = tx.objectStore(STORE_TRANSFERS);
        store.put(record);
        tx.oncomplete = function () { resolve(); };
        tx.onerror = function () { resolve(); };
      } catch (e) {
        resolve();
      }
    });
  }

  async function getTransferRecord(transferId) {
    var db = await openDatabase();
    if (!db) {
      return memoryRegistry.get(transferId) || null;
    }
    return new Promise(function (resolve) {
      try {
        var tx = db.transaction(STORE_TRANSFERS, "readonly");
        var store = tx.objectStore(STORE_TRANSFERS);
        var req = store.get(transferId);
        req.onsuccess = function () { resolve(req.result || null); };
        req.onerror = function () { resolve(null); };
      } catch (e) {
        resolve(null);
      }
    });
  }

  async function deleteTransferRecord(transferId) {
    var db = await openDatabase();
    memoryRegistry.delete(transferId);
    if (!db) return;
    return new Promise(function (resolve) {
      try {
        var tx = db.transaction(STORE_TRANSFERS, "readwrite");
        var store = tx.objectStore(STORE_TRANSFERS);
        store.delete(transferId);
        tx.oncomplete = function () { resolve(); };
        tx.onerror = function () { resolve(); };
      } catch (e) {
        resolve();
      }
    });
  }

  async function listPendingTransfers(type) {
    var db = await openDatabase();
    if (!db) {
      var arr = [];
      memoryRegistry.forEach(function (v) {
        if (!type || v.type === type) arr.push(v);
      });
      return arr;
    }
    return new Promise(function (resolve) {
      try {
        var tx = db.transaction(STORE_TRANSFERS, "readonly");
        var store = tx.objectStore(STORE_TRANSFERS);
        var req = store.getAll();
        req.onsuccess = function () {
          var results = req.result || [];
          if (type) {
            results = results.filter(function (r) { return r.type === type; });
          }
          resolve(results);
        };
        req.onerror = function () { resolve([]); };
      } catch (e) {
        resolve([]);
      }
    });
  }

  // ---------------------------------------------------------------------------
  // Backoff with Jitter (1s base, exponential, cap 30s)
  // ---------------------------------------------------------------------------
  function getBackoffMs(attempt) {
    var capped = Math.min(30000, 1000 * Math.pow(2, attempt));
    var jitter = Math.random() * 500;
    return Math.floor(capped + jitter);
  }

  function waitMs(ms) {
    return new Promise(function (resolve) { setTimeout(resolve, ms); });
  }

  // ---------------------------------------------------------------------------
  // Worker Manager
  // ---------------------------------------------------------------------------
  var sharedWorker = null;
  var workerListeners = new Map(); // transferId -> callback

  function getUploadWorker() {
    if (!sharedWorker && typeof Worker !== "undefined") {
      try {
        sharedWorker = new Worker("/static/sha256-worker.js");
        sharedWorker.onmessage = function (ev) {
          var msg = ev.data;
          if (!msg) return;
          var tid = msg.transferId;
          if (tid && workerListeners.has(tid)) {
            var cb = workerListeners.get(tid);
            cb(msg);
          }
        };
      } catch (err) {
        if (typeof console !== "undefined") console.warn("Failed to spawn sha256-worker.js:", err);
      }
    }
    return sharedWorker;
  }

  function registerWorkerListener(transferId, cb) {
    workerListeners.set(transferId, cb);
  }

  function unregisterWorkerListener(transferId) {
    workerListeners.delete(transferId);
  }

  // ---------------------------------------------------------------------------
  // Resumable Upload Orchestrator
  // ---------------------------------------------------------------------------
  function ResumableUploadEngine(options) {
    this.options = Object.assign({
      fallbackBodySize: 16 * 1024 * 1024, // 16 MB default
      forceFallback: false,
      maxRetries: 5
    }, options);
  }

  /**
   * Queries authoritative HEAD on tus resource with bounded settlement retry for 423 Locked.
   */
  ResumableUploadEngine.prototype.queryHead = async function (uploadUrl, authHeader, maxRetries) {
    var retries = (typeof maxRetries === "number") ? maxRetries : 5;
    var attempt = 0;
    while (true) {
      var headers = {
        "Tus-Resumable": "1.0.0"
      };
      if (authHeader) headers["Authorization"] = authHeader;

      var resp = await fetch(uploadUrl, {
        method: "HEAD",
        headers: headers,
        cache: "no-store"
      });

      if (resp.status === 423 && attempt < retries) {
        attempt++;
        var waitSec = parseInt(resp.headers.get("Retry-After") || "1", 10);
        await waitMs(waitSec * 1000);
        continue;
      }

      if (!resp.ok) {
        throw new Error("HEAD query failed with status " + resp.status);
      }

      var offsetStr = resp.headers.get("Upload-Offset");
      var lengthStr = resp.headers.get("Upload-Length");
      var state = resp.headers.get("Upload-State") || "active";
      var prefixDigest = resp.headers.get("Upload-Prefix-Digest") || "";

      var offset = parseInt(offsetStr, 10);
      var length = parseInt(lengthStr, 10);

      return {
        offset: !isNaN(offset) ? offset : 0,
        length: !isNaN(length) ? length : 0,
        state: state,
        prefixDigest: prefixDigest
      };
    }
  };

  /**
   * Queries authoritative application status endpoint.
   */
  ResumableUploadEngine.prototype.queryStatus = async function (statusUrl, authHeader) {
    var headers = {};
    if (authHeader) headers["Authorization"] = authHeader;

    var resp = await fetch(statusUrl, {
      method: "GET",
      headers: headers,
      cache: "no-store"
    });

    if (!resp.ok) {
      throw new Error("Status query failed with status " + resp.status);
    }
    return await resp.json();
  };

  /**
   * Starts or resumes a transfer via the dedicated Web Worker.
   */
  ResumableUploadEngine.prototype.startUpload = function (params, callbacks) {
    var self = this;
    var transferId = params.transferId;
    var file = params.file;
    var uploadUrl = params.uploadUrl;
    var completeUrl = params.completeUrl;
    var statusUrl = params.statusUrl;
    var authHeader = params.authHeader;
    var type = params.type; // "personal" or "guest"
    var isCancelled = false;

    var worker = getUploadWorker();
    if (!worker) {
      if (callbacks.onError) callbacks.onError(new Error("Web Worker unavailable"));
      return { cancel: function () {} };
    }

    // Persist non-sensitive record immediately
    var transferRec = {
      transferId: transferId,
      type: type,
      uploadUrl: uploadUrl,
      completeUrl: completeUrl,
      statusUrl: statusUrl,
      sanitizedName: file.name,
      expectedSize: file.size,
      lastModified: file.lastModified,
      lastKnownOffset: 0,
      lastKnownState: "active",
      retryCount: 0
    };
    saveTransferRecord(transferRec);

    var listener = async function (msg) {
      if (isCancelled && msg.type !== "PATCH_ABORTED") return;

      switch (msg.type) {
        case "PROGRESS":
          if (callbacks.onProgress) {
            callbacks.onProgress({
              uploaded: msg.uploaded,
              total: msg.total,
              speed: msg.speed,
              eta: msg.eta
            });
          }
          break;

        case "REBUILD_PROGRESS":
          if (callbacks.onRebuildProgress) {
            callbacks.onRebuildProgress({
              hashed: msg.hashed,
              total: msg.total
            });
          }
          break;

        case "PREFIX_VERIFIED":
          // Resume upload from the verified offset using the retained worker hasher
          worker.postMessage({
            type: "RESUME_UPLOAD",
            transferId: transferId,
            file: file,
            uploadUrl: uploadUrl,
            authHeader: authHeader,
            serverOffset: msg.serverOffset,
            fallbackBodySize: self.options.fallbackBodySize,
            forceFallback: self.options.forceFallback
          }, []);
          break;

        case "FILE_MODIFIED":
          unregisterWorkerListener(transferId);
          if (callbacks.onError) {
            callbacks.onError(new Error("Selected file does not match existing partial upload on desktop. Transfer halted."));
          }
          break;

        case "UPLOAD_BYTES_COMPLETE":
          // Safety guard: verify that server committed offset actually reached total file size!
          if (msg.serverOffset < file.size) {
            handleInterruption("Committed offset (" + msg.serverOffset + ") is less than file size (" + file.size + ")");
            break;
          }
          // All bytes committed on server; submit completion with calculated digest
          try {
            var completeResp = await self.submitCompletion(completeUrl, msg.expectedSHA256, authHeader);
            await deleteTransferRecord(transferId);
            unregisterWorkerListener(transferId);
            if (callbacks.onComplete) {
              callbacks.onComplete({
                status: "completed",
                sha256: msg.expectedSHA256,
                response: completeResp
              });
            }
          } catch (compErr) {
            // Reconcile via authoritative status query
            try {
              var st = await self.queryStatus(statusUrl, authHeader);
              if (st && st.status === "completed") {
                await deleteTransferRecord(transferId);
                unregisterWorkerListener(transferId);
                if (callbacks.onComplete) callbacks.onComplete(st);
                return;
              }
            } catch (stErr) {}

            unregisterWorkerListener(transferId);
            if (callbacks.onError) callbacks.onError(compErr);
          }
          break;

        case "UPLOAD_INTERRUPTED":
          // Invalidate worker hasher and coordinate bounded retry
          handleInterruption(msg.error, msg.retryAfter);
          break;

        case "PATCH_ABORTED":
          // Worker aborted active PATCH; caller can now safely send DELETE
          if (callbacks.onAborted) callbacks.onAborted();
          break;

        case "ERROR":
          unregisterWorkerListener(transferId);
          if (callbacks.onError) callbacks.onError(new Error(msg.error));
          break;
      }
    };

    registerWorkerListener(transferId, listener);

    // Initial launch sequence
    (async function init() {
      try {
        var head = await self.queryHead(uploadUrl, authHeader);
        if (head.state === "completed") {
          await deleteTransferRecord(transferId);
          unregisterWorkerListener(transferId);
          if (callbacks.onComplete) {
            callbacks.onComplete({
              status: "completed",
              sha256: head.prefixDigest || "",
              response: { status: "completed" }
            });
          }
          return;
        }

        if (head.state === "awaiting_digest" || head.offset >= file.size) {
          // Server already has 100% bytes; calculate full-file digest and complete
          var fullHasher = new workerScopeSha256();
          // We can ask worker to REBUILD_HASHER for the full file
          worker.postMessage({
            type: "REBUILD_HASHER",
            transferId: transferId,
            file: file,
            serverOffset: file.size,
            expectedPrefixDigest: head.prefixDigest
          }, []);
          return;
        }

        if (head.offset > 0) {
          // Partial upload exists on server: notify progress and rebuild client hasher up to committed offset
          if (callbacks.onProgress) {
            callbacks.onProgress({
              uploaded: head.offset,
              total: file.size,
              speed: 0,
              eta: 0
            });
          }
          worker.postMessage({
            type: "REBUILD_HASHER",
            transferId: transferId,
            file: file,
            serverOffset: head.offset,
            expectedPrefixDigest: head.prefixDigest
          }, []);
        } else {
          // Fresh upload from offset 0
          worker.postMessage({
            type: "START_UPLOAD",
            transferId: transferId,
            file: file,
            uploadUrl: uploadUrl,
            authHeader: authHeader,
            serverOffset: 0,
            fallbackBodySize: self.options.fallbackBodySize,
            forceFallback: self.options.forceFallback
          }, []);
        }
      } catch (err) {
        handleInterruption(err.message);
      }
    })();

    var retryAttempt = 0;
    async function handleInterruption(errMsg, retryAfterSec) {
      if (isCancelled) return;
      if (retryAttempt >= self.options.maxRetries) {
        unregisterWorkerListener(transferId);
        if (callbacks.onError) {
          callbacks.onError(new Error("Transfer interrupted (" + (errMsg || "network drop") + "). Retries exhausted; click Retry to resume."));
        }
        return;
      }

      retryAttempt++;
      var delay = (retryAfterSec && retryAfterSec > 0)
        ? retryAfterSec * 1000
        : getBackoffMs(retryAttempt);

      if (callbacks.onReconnecting) {
        callbacks.onReconnecting({ attempt: retryAttempt, maxRetries: self.options.maxRetries, delay: delay });
      }

      await waitMs(delay);
      if (isCancelled) return;

      try {
        var head = await self.queryHead(uploadUrl, authHeader);
        transferRec.lastKnownOffset = head.offset;
        transferRec.lastKnownState = head.state;
        transferRec.retryCount = retryAttempt;
        saveTransferRecord(transferRec);

        if (head.state === "completed") {
          await deleteTransferRecord(transferId);
          unregisterWorkerListener(transferId);
          if (callbacks.onComplete) {
            callbacks.onComplete({
              status: "completed",
              sha256: head.prefixDigest || "",
              response: { status: "completed" }
            });
          }
          return;
        }

        if (head.state === "awaiting_digest" || head.offset >= file.size) {
          worker.postMessage({
            type: "REBUILD_HASHER",
            transferId: transferId,
            file: file,
            serverOffset: file.size,
            expectedPrefixDigest: head.prefixDigest
          }, []);
        } else {
          if (callbacks.onProgress) {
            callbacks.onProgress({
              uploaded: head.offset,
              total: file.size,
              speed: 0,
              eta: 0
            });
          }
          worker.postMessage({
            type: "REBUILD_HASHER",
            transferId: transferId,
            file: file,
            serverOffset: head.offset,
            expectedPrefixDigest: head.prefixDigest
          }, []);
        }
      } catch (hErr) {
        handleInterruption(hErr.message);
      }
    }

    return {
      cancel: function (onComplete) {
        isCancelled = true;
        self.cancelUpload(transferId, uploadUrl, authHeader, onComplete);
      }
    };
  };

  /**
   * Submits completion digest to application endpoint.
   */
  ResumableUploadEngine.prototype.submitCompletion = async function (completeUrl, expectedSHA256, authHeader) {
    var headers = { "Content-Type": "application/json" };
    if (authHeader) headers["Authorization"] = authHeader;

    var resp = await fetch(completeUrl, {
      method: "POST",
      headers: headers,
      body: JSON.stringify({ expectedSHA256: expectedSHA256 })
    });

    if (resp.status === 423) {
      var waitSec = parseInt(resp.headers.get("Retry-After") || "1", 10);
      await waitMs(waitSec * 1000);
      return this.submitCompletion(completeUrl, expectedSHA256, authHeader);
    }

    if (!resp.ok) {
      var errData = {};
      try { errData = await resp.json(); } catch (e) {}
      throw new Error(errData.error || ("Completion rejected (" + resp.status + ")"));
    }

    return await resp.json();
  };

  /**
   * Cancellation coordination with WP2 lifecycle lock:
   * 1. Tell worker CANCEL_UPLOAD
   * 2. Await worker PATCH_ABORTED
   * 3. Send DELETE <uploadUrl>
   * 4. If 423 Locked: respect Retry-After and retry DELETE
   * 5. Only after 204 (or 404/410) mark cancelled and delete record
   */
  ResumableUploadEngine.prototype.cancelUpload = async function (transferId, uploadUrl, authHeader, onComplete) {
    var worker = getUploadWorker();

    await new Promise(function (resolve) {
      if (!worker) { resolve(); return; }
      var handler = function (ev) {
        if (ev.data && ev.data.transferId === transferId && ev.data.type === "PATCH_ABORTED") {
          worker.removeEventListener("message", handler);
          resolve();
        }
      };
      worker.addEventListener("message", handler);
      worker.postMessage({ type: "CANCEL_UPLOAD", transferId: transferId });
    });

    // Worker PATCH has now terminated; issue DELETE
    var maxDeleteAttempts = 4;
    for (var attempt = 1; attempt <= maxDeleteAttempts; attempt++) {
      try {
        var headers = { "Tus-Resumable": "1.0.0" };
        if (authHeader) headers["Authorization"] = authHeader;

        var resp = await fetch(uploadUrl, {
          method: "DELETE",
          headers: headers
        });

        if (resp.status === 204 || resp.status === 404 || resp.status === 410) {
          // Successfully deleted on server
          await deleteTransferRecord(transferId);
          unregisterWorkerListener(transferId);
          if (onComplete) onComplete({ cancelled: true, status: resp.status });
          return;
        }

        if (resp.status === 423) {
          // Server locked: respect Retry-After
          var retryAfter = parseInt(resp.headers.get("Retry-After") || "1", 10);
          await waitMs(retryAfter * 1000);
          continue;
        }

        throw new Error("DELETE returned unexpected status: " + resp.status);
      } catch (err) {
        if (attempt === maxDeleteAttempts) {
          unregisterWorkerListener(transferId);
          if (onComplete) onComplete({ cancelled: false, error: err.message });
          return;
        }
        await waitMs(1000 * attempt);
      }
    }
  };

  // Helper Sha256 for main thread if needed
  function workerScopeSha256() {
    if (typeof globalScope.sha256 !== "undefined" && typeof globalScope.sha256.create === "function") {
      return globalScope.sha256.create();
    }
    return null;
  }

  // Export engine
  globalScope.FlitnixResumable = {
    ResumableUploadEngine: ResumableUploadEngine,
    saveTransferRecord: saveTransferRecord,
    getTransferRecord: getTransferRecord,
    deleteTransferRecord: deleteTransferRecord,
    listPendingTransfers: listPendingTransfers,
    getBackoffMs: getBackoffMs
  };

})(typeof window !== "undefined" ? window : globalThis);
