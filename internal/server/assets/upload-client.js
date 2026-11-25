/*
 * Flitnix upload transport.
 *
 * Provides streaming upload primitives for the phone and guest pages, executed
 * in a dedicated Web Worker with an identical main-thread XHR fallback.
 *
 *   var stream = FlitnixUpload.streamResumable({
 *     url: "/api/batches/.../items/.../content",
 *     headers: {"Authorization": "Bearer ..."},
 *     file: fileObject,
 *     onProgress: function (loaded, total) { ... }
 *   });
 */
(function (global) {
  "use strict";

  var WORKER_URL = "/static/upload-worker.js";
  var DEFAULT_CHUNK_SIZE = 4 * 1024 * 1024;

  var worker = null;
  var workerBroken = false;
  var pending = Object.create(null);
  var nextId = 1;

  function workerAvailable() {
    return !workerBroken && typeof Worker !== "undefined";
  }

  function transportError(kind, message, offset) {
    if (kind === "abort") {
      var aborted = new Error("upload aborted");
      aborted.aborted = true;
      aborted.offset = offset;
      return aborted;
    }
    if (kind === "timeout") {
      var timedOut = new Error(message || "Upload timed out.");
      timedOut.timeout = true;
      timedOut.offset = offset;
      return timedOut;
    }
    var failed = new Error(message || "Network error while uploading.");
    failed.network = true;
    failed.offset = offset;
    return failed;
  }

  function failAllPending() {
    var ids = Object.keys(pending);
    for (var i = 0; i < ids.length; i++) {
      var entry = pending[ids[i]];
      delete pending[ids[i]];
      entry.reject(transportError("network", ""));
    }
  }

  function ensureWorker() {
    if (worker) return worker;
    worker = new Worker(WORKER_URL);
    worker.onmessage = function (ev) {
      var msg = ev.data;
      if (!msg || !msg.uploadId) return;
      var entry = pending[msg.uploadId];
      if (!entry) return;
      if (msg.type === "progress") {
        if (entry.onProgress) entry.onProgress(msg.loaded, msg.total);
        return;
      }
      delete pending[msg.uploadId];
      if (msg.type === "done") {
        entry.resolve({ status: msg.status, responseText: msg.responseText || "", offset: msg.offset });
      } else if (msg.type === "error") {
        entry.reject(transportError(msg.kind, msg.message, msg.offset));
      }
    };
    worker.onerror = function () {
      workerBroken = true;
      worker = null;
      failAllPending();
    };
    return worker;
  }

  function runInWorker(type, opts) {
    var w = ensureWorker();
    var id = nextId++;
    var promise = new Promise(function (resolve, reject) {
      pending[id] = { onProgress: opts.onProgress, resolve: resolve, reject: reject };
      w.postMessage({
        type: type,
        uploadId: id,
        url: opts.url,
        headers: opts.headers || {},
        file: opts.file,
        chunkSize: opts.chunkSize || DEFAULT_CHUNK_SIZE
      });
    });
    return {
      promise: promise,
      abort: function () {
        var entry = pending[id];
        if (!entry) return;
        try {
          w.postMessage({ type: "abort", uploadId: id });
        } catch (e) {
          delete pending[id];
          entry.reject(transportError("abort", ""));
        }
      }
    };
  }

  function streamOnMainThread(opts) {
    var xhr = new XMLHttpRequest();
    var promise = new Promise(function (resolve, reject) {
      xhr.upload.onprogress = function (ev) {
        if (!ev.lengthComputable) return;
        if (opts.onProgress) opts.onProgress(ev.loaded, ev.total);
      };
      xhr.onload = function () {
        resolve({ status: xhr.status, responseText: xhr.responseText || "" });
      };
      xhr.onerror = function () { reject(transportError("network", "")); };
      xhr.ontimeout = function () { reject(transportError("timeout", "")); };
      xhr.onabort = function () { reject(transportError("abort", "")); };

      try {
        xhr.open("PUT", opts.url);
        var headers = opts.headers || {};
        for (var name in headers) {
          if (Object.prototype.hasOwnProperty.call(headers, name)) {
            xhr.setRequestHeader(name, headers[name]);
          }
        }
        xhr.send(opts.file);
      } catch (err) {
        reject(transportError("network", (err && err.message) || ""));
      }
    });
    return {
      promise: promise,
      abort: function () {
        try { xhr.abort(); } catch (e) {}
      }
    };
  }

  function streamResumableOnMainThread(opts) {
    var file = opts.file;
    var total = file ? file.size : 0;
    var chunkSize = opts.chunkSize || DEFAULT_CHUNK_SIZE;
    var offset = 0;
    var mismatchRetries = 0;
    var currentXhr = null;
    var aborted = false;

    function applyHeaders(xhr, extra) {
      var headers = opts.headers || {};
      for (var name in headers) {
        if (Object.prototype.hasOwnProperty.call(headers, name)) {
          xhr.setRequestHeader(name, headers[name]);
        }
      }
      if (extra) {
        for (var k in extra) {
          if (Object.prototype.hasOwnProperty.call(extra, k)) {
            xhr.setRequestHeader(k, extra[k]);
          }
        }
      }
    }

    var promise = new Promise(function (resolve, reject) {
      function queryHead() {
        if (aborted) { reject(transportError("abort", "", offset)); return; }
        var xhr = new XMLHttpRequest();
        currentXhr = xhr;
        xhr.onload = function () {
          if (aborted) { reject(transportError("abort", "", offset)); return; }
          if (xhr.status === 200) {
            var srvOff = parseInt(xhr.getResponseHeader("Upload-Offset") || "0", 10);
            if (!isNaN(srvOff) && srvOff >= 0 && srvOff <= total) offset = srvOff;
            var srvChunk = parseInt(xhr.getResponseHeader("Upload-Chunk-Size") || "0", 10);
            if (!isNaN(srvChunk) && srvChunk > 0) chunkSize = srvChunk;
            var upState = xhr.getResponseHeader("X-Upload-State") || "";
            if (upState === "completed" || (total > 0 && offset === total)) {
              if (opts.onProgress) opts.onProgress(total, total);
              resolve({ status: 200, responseText: JSON.stringify({ written: total }), offset: total });
              return;
            }
            if (opts.onProgress) opts.onProgress(offset, total);
            sendNextChunk();
            return;
          }
          if (xhr.status === 404 || xhr.status === 405) {
            var fallback = streamOnMainThread(opts);
            currentXhr = { abort: fallback.abort };
            fallback.promise.then(resolve, reject);
            return;
          }
          resolve({ status: xhr.status, responseText: xhr.responseText || "", offset: offset });
        };
        xhr.onerror = function () { reject(transportError("network", "", offset)); };
        xhr.ontimeout = function () { reject(transportError("timeout", "", offset)); };
        xhr.onabort = function () { reject(transportError("abort", "", offset)); };
        try {
          xhr.open("HEAD", opts.url);
          applyHeaders(xhr, null);
          xhr.send();
        } catch (err) {
          reject(transportError("network", (err && err.message) || "", offset));
        }
      }

      function sendNextChunk() {
        if (aborted) { reject(transportError("abort", "", offset)); return; }
        var end = total === 0 ? 0 : Math.min(total, offset + chunkSize);
        var slice = total === 0 ? new Blob([]) : file.slice(offset, end);
        var xhr = new XMLHttpRequest();
        currentXhr = xhr;
        xhr.upload.onprogress = function (e) {
          if (!e.lengthComputable) return;
          if (opts.onProgress) opts.onProgress(Math.min(total, offset + e.loaded), total);
        };
        xhr.onload = function () {
          if (aborted) { reject(transportError("abort", "", offset)); return; }
          if (xhr.status === 204) {
            var srvOff = parseInt(xhr.getResponseHeader("Upload-Offset") || "", 10);
            offset = (!isNaN(srvOff) && srvOff >= 0) ? srvOff : end;
            if (opts.onProgress) opts.onProgress(offset, total);
            mismatchRetries = 0;
            if (offset < total) sendNextChunk();
            else resolve({ status: 200, responseText: xhr.responseText || "", offset: total });
            return;
          }
          if (xhr.status === 200) {
            offset = total;
            if (opts.onProgress) opts.onProgress(total, total);
            resolve({ status: 200, responseText: xhr.responseText || "", offset: total });
            return;
          }
          if (xhr.status === 409) {
            var parsed = null;
            try { parsed = JSON.parse(xhr.responseText || "{}"); } catch (e) {}
            if (parsed && parsed.code === "offset_mismatch" && mismatchRetries < 3) {
              mismatchRetries += 1;
              var expected = parseInt(xhr.getResponseHeader("Upload-Offset") || String(parsed.expectedOffset || ""), 10);
              if (!isNaN(expected) && expected >= 0 && expected <= total) {
                offset = expected;
                if (opts.onProgress) opts.onProgress(offset, total);
                if (offset === total && total > 0) {
                  resolve({ status: 200, responseText: JSON.stringify({ written: total }), offset: total });
                } else {
                  sendNextChunk();
                }
                return;
              }
            }
          }
          var hdrOff = parseInt(xhr.getResponseHeader("Upload-Offset") || "", 10);
          if (!isNaN(hdrOff) && hdrOff >= 0 && hdrOff <= total) offset = hdrOff;
          resolve({ status: xhr.status, responseText: xhr.responseText || "", offset: offset });
        };
        xhr.onerror = function () { reject(transportError("network", "", offset)); };
        xhr.ontimeout = function () { reject(transportError("timeout", "", offset)); };
        xhr.onabort = function () { reject(transportError("abort", "", offset)); };
        try {
          xhr.open("PATCH", opts.url);
          applyHeaders(xhr, {
            "Content-Type": "application/offset+octet-stream",
            "Upload-Offset": String(offset)
          });
          xhr.send(slice);
        } catch (err) {
          reject(transportError("network", (err && err.message) || "", offset));
        }
      }

      queryHead();
    });

    return {
      promise: promise,
      abort: function () {
        aborted = true;
        if (currentXhr) {
          try { currentXhr.abort(); } catch (e) {}
        }
      }
    };
  }

  function stream(opts) {
    if (workerAvailable()) {
      try {
        return runInWorker("start", opts);
      } catch (err) {
        workerBroken = true;
        worker = null;
      }
    }
    return streamOnMainThread(opts);
  }

  function streamResumable(opts) {
    if (workerAvailable()) {
      try {
        return runInWorker("startResumable", opts);
      } catch (err) {
        workerBroken = true;
        worker = null;
      }
    }
    return streamResumableOnMainThread(opts);
  }

  global.FlitnixUpload = {
    stream: stream,
    streamResumable: streamResumable
  };
})(typeof self !== "undefined" ? self : this);
