/*
 * Flitnix upload transport.
 *
 * Provides one streaming primitive used by both the phone page and the guest
 * drop box: a single PUT of a File with live progress, executed in a Web
 * Worker so the transfer is not sharing the page's main thread with layout,
 * painting and progress rendering.
 *
 * When workers are unavailable the same contract is served by a main-thread
 * XMLHttpRequest, so behaviour (and error reporting) is identical either way.
 *
 *   var stream = FlitnixUpload.stream({
 *     url: "/api/...",
 *     headers: {"Authorization": "Bearer ..."},
 *     file: fileObject,
 *     onProgress: function (loaded, total) { ... }
 *   });
 *   stream.promise.then(function (res) { res.status; res.responseText; });
 *   stream.promise.catch(function (err) { err.aborted; err.network; err.timeout; });
 *   stream.abort();
 *
 * The promise resolves for every completed exchange, including HTTP error
 * statuses, and rejects only for cancellation or a transport failure.
 */
(function (global) {
  "use strict";

  var WORKER_URL = "/static/upload-worker.js";

  var worker = null;
  var workerBroken = false;
  var pending = Object.create(null);
  var nextId = 1;

  function workerAvailable() {
    return !workerBroken && typeof Worker !== "undefined";
  }

  function transportError(kind, message) {
    if (kind === "abort") {
      var aborted = new Error("upload aborted");
      aborted.aborted = true;
      return aborted;
    }
    if (kind === "timeout") {
      var timedOut = new Error(message || "Upload timed out.");
      timedOut.timeout = true;
      return timedOut;
    }
    var failed = new Error(message || "Network error while uploading.");
    failed.network = true;
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
        entry.resolve({ status: msg.status, responseText: msg.responseText || "" });
      } else if (msg.type === "error") {
        entry.reject(transportError(msg.kind, msg.message));
      }
    };
    // A worker that cannot start or that dies mid-transfer must not strand
    // the caller: fail what is in flight and let later calls fall back to
    // the main thread rather than repeating the failure forever.
    worker.onerror = function () {
      workerBroken = true;
      worker = null;
      failAllPending();
    };
    return worker;
  }

  function streamInWorker(opts) {
    var w = ensureWorker();
    var id = nextId++;
    var promise = new Promise(function (resolve, reject) {
      pending[id] = { onProgress: opts.onProgress, resolve: resolve, reject: reject };
      w.postMessage({
        type: "start",
        uploadId: id,
        url: opts.url,
        headers: opts.headers || {},
        file: opts.file
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
          // Worker already gone; fail fast instead of waiting forever.
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
        try {
          xhr.abort();
        } catch (e) {
          // Already settled.
        }
      }
    };
  }

  function stream(opts) {
    if (workerAvailable()) {
      try {
        return streamInWorker(opts);
      } catch (err) {
        // Construction or postMessage failed; use the same-thread path for
        // this transfer and every later one.
        workerBroken = true;
        worker = null;
      }
    }
    return streamOnMainThread(opts);
  }

  global.FlitnixUpload = { stream: stream };
})(typeof self !== "undefined" ? self : this);
