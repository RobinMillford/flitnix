/*
 * Flitnix upload transport worker.
 *
 * Streams one file either via single-pass PUT ("start") or resumable chunked
 * HEAD + PATCH ("startResumable") from a dedicated worker thread so layout,
 * painting and progress rendering on the main thread cannot contend with the
 * socket write path.
 *
 * Protocol (main thread <-> worker):
 *   in : {type: "start", uploadId, url, headers, file}
 *   in : {type: "startResumable", uploadId, url, headers, file, chunkSize}
 *   in : {type: "abort", uploadId}
 *   out: {type: "progress", uploadId, loaded, total}
 *   out: {type: "done", uploadId, status, responseText, offset}
 *   out: {type: "error", uploadId, kind, message, offset}
 */
"use strict";

var active = Object.create(null);
var DEFAULT_CHUNK_SIZE = 4 * 1024 * 1024;

function parseRetryAfterMs(value, now) {
  // Mirrors the Go client's ParseRetryAfter: delta seconds or an HTTP date;
  // malformed, negative and missing values yield 0; huge values clamp to
  // the maximum automatic wait (5 minutes).
  if (value === null || value === undefined) return 0;
  var s = String(value).trim();
  if (!s) return 0;
  var secs = Number(s);
  if (isNaN(secs)) {
    var t = Date.parse(s);
    if (isNaN(t)) return 0;
    secs = (t - (now || Date.now())) / 1000;
  }
  if (!isFinite(secs) || secs < 0) return 0;
  if (secs > 86400) return 300000;
  return Math.round(secs * 1000);
}

self.onmessage = function (ev) {
  var msg = ev.data;
  if (!msg || !msg.type) return;
  if (msg.type === "start") startUpload(msg);
  else if (msg.type === "startResumable") startResumableUpload(msg);
  else if (msg.type === "abort") abortUpload(msg.uploadId);
};

function startUpload(msg) {
  var id = msg.uploadId;
  var xhr = new XMLHttpRequest();
  active[id] = { xhr: xhr, aborted: false };

  function settle(fn) {
    return function () {
      if (active[id] && active[id].xhr === xhr) delete active[id];
      fn();
    };
  }

  var lastPost = 0;
  xhr.upload.onprogress = function (e) {
    if (!e.lengthComputable) return;
    var now = Date.now();
    if (now - lastPost < 100 && e.loaded !== e.total) return;
    lastPost = now;
    self.postMessage({ type: "progress", uploadId: id, loaded: e.loaded, total: e.total });
  };

  xhr.onload = settle(function () {
    self.postMessage({
      type: "done",
      uploadId: id,
      status: xhr.status,
      responseText: xhr.responseText || ""
    });
  });
  xhr.onerror = settle(function () {
    self.postMessage({ type: "error", uploadId: id, kind: "network", message: "" });
  });
  xhr.ontimeout = settle(function () {
    self.postMessage({ type: "error", uploadId: id, kind: "timeout", message: "" });
  });
  xhr.onabort = settle(function () {
    self.postMessage({ type: "error", uploadId: id, kind: "abort", message: "" });
  });

  try {
    xhr.open("PUT", msg.url);
    var headers = msg.headers || {};
    for (var name in headers) {
      if (Object.prototype.hasOwnProperty.call(headers, name)) {
        xhr.setRequestHeader(name, headers[name]);
      }
    }
    xhr.send(msg.file);
  } catch (err) {
    delete active[id];
    self.postMessage({
      type: "error",
      uploadId: id,
      kind: "network",
      message: (err && err.message) || ""
    });
  }
}

function startResumableUpload(msg) {
  var id = msg.uploadId;
  var file = msg.file;
  var total = file ? file.size : 0;
  var chunkSize = msg.chunkSize || DEFAULT_CHUNK_SIZE;
  var offset = 0;
  var mismatchRetries = 0;
  var state = { xhr: null, aborted: false };
  active[id] = state;

  function applyHeaders(xhr, extra) {
    var headers = msg.headers || {};
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

  function finishDone(status, text, retryAfterMs) {
    if (active[id] === state) delete active[id];
    self.postMessage({
      type: "done",
      uploadId: id,
      status: status,
      responseText: text || "",
      offset: offset,
      retryAfterMs: typeof retryAfterMs === "number" ? retryAfterMs : 0
    });
  }

  function finishError(kind, message) {
    if (active[id] === state) delete active[id];
    self.postMessage({
      type: "error",
      uploadId: id,
      kind: kind,
      message: message || "",
      offset: offset
    });
  }

  function queryHead() {
    if (state.aborted) { finishError("abort", ""); return; }
    var xhr = new XMLHttpRequest();
    state.xhr = xhr;
    xhr.onload = function () {
      if (state.aborted) { finishError("abort", ""); return; }
      if (xhr.status === 200) {
        var srvOff = parseInt(xhr.getResponseHeader("Upload-Offset") || "0", 10);
        if (!isNaN(srvOff) && srvOff >= 0 && srvOff <= total) {
          offset = srvOff;
        }
        var srvChunk = parseInt(xhr.getResponseHeader("Upload-Chunk-Size") || "0", 10);
        if (!isNaN(srvChunk) && srvChunk > 0) {
          chunkSize = srvChunk;
        }
        var upState = xhr.getResponseHeader("X-Upload-State") || "";
        if (upState === "completed" || (total > 0 && offset === total)) {
          self.postMessage({ type: "progress", uploadId: id, loaded: total, total: total });
          finishDone(200, JSON.stringify({ written: total }));
          return;
        }
        self.postMessage({ type: "progress", uploadId: id, loaded: offset, total: total });
        sendNextChunk();
        return;
      }
      if (xhr.status === 404 || xhr.status === 405) {
        // Endpoint does not advertise HEAD/PATCH; fall back cleanly to PUT.
        delete active[id];
        startUpload(msg);
        return;
      }
      finishDone(xhr.status, xhr.responseText || "", parseRetryAfterMs(xhr.getResponseHeader("Retry-After")));
    };
    xhr.onerror = function () { finishError("network", ""); };
    xhr.ontimeout = function () { finishError("timeout", ""); };
    xhr.onabort = function () { finishError("abort", ""); };
    try {
      xhr.open("HEAD", msg.url);
      applyHeaders(xhr, null);
      xhr.send();
    } catch (err) {
      finishError("network", (err && err.message) || "");
    }
  }

  function sendNextChunk() {
    if (state.aborted) { finishError("abort", ""); return; }
    var end = total === 0 ? 0 : Math.min(total, offset + chunkSize);
    var slice = total === 0 ? new Blob([]) : file.slice(offset, end);
    var sliceLen = end - offset;

    var xhr = new XMLHttpRequest();
    state.xhr = xhr;
    var lastPost = 0;
    xhr.upload.onprogress = function (e) {
      if (!e.lengthComputable) return;
      var loadedTotal = Math.min(total, offset + e.loaded);
      var now = Date.now();
      if (now - lastPost < 100 && loadedTotal !== total) return;
      lastPost = now;
      self.postMessage({ type: "progress", uploadId: id, loaded: loadedTotal, total: total });
    };
    xhr.onload = function () {
      if (state.aborted) { finishError("abort", ""); return; }
      if (xhr.status === 204) {
        var srvOff = parseInt(xhr.getResponseHeader("Upload-Offset") || "", 10);
        offset = (!isNaN(srvOff) && srvOff >= 0) ? srvOff : end;
        self.postMessage({ type: "progress", uploadId: id, loaded: offset, total: total });
        mismatchRetries = 0;
        if (offset < total) sendNextChunk();
        else finishDone(200, xhr.responseText || "");
        return;
      }
      if (xhr.status === 200) {
        offset = total;
        self.postMessage({ type: "progress", uploadId: id, loaded: total, total: total });
        finishDone(200, xhr.responseText || "");
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
            self.postMessage({ type: "progress", uploadId: id, loaded: offset, total: total });
            if (offset === total && total > 0) {
              finishDone(200, JSON.stringify({ written: total }));
            } else {
              sendNextChunk();
            }
            return;
          }
        }
      }
      // Server may have committed an earlier chunk and returned Upload-Offset on 400/408/507.
      var hdrOff = parseInt(xhr.getResponseHeader("Upload-Offset") || "", 10);
      if (!isNaN(hdrOff) && hdrOff >= 0 && hdrOff <= total) {
        offset = hdrOff;
      }
      finishDone(xhr.status, xhr.responseText || "", parseRetryAfterMs(xhr.getResponseHeader("Retry-After")));
    };
    xhr.onerror = function () { finishError("network", ""); };
    xhr.ontimeout = function () { finishError("timeout", ""); };
    xhr.onabort = function () { finishError("abort", ""); };
    try {
      xhr.open("PATCH", msg.url);
      applyHeaders(xhr, {
        "Content-Type": "application/offset+octet-stream",
        "Upload-Offset": String(offset)
      });
      xhr.send(slice);
    } catch (err) {
      finishError("network", (err && err.message) || "");
    }
  }

  queryHead();
}

function abortUpload(id) {
  var entry = active[id];
  if (!entry) return;
  entry.aborted = true;
  var xhr = entry.xhr || entry;
  try {
    xhr.abort();
  } catch (e) {
    // Already settled; nothing to withdraw.
  }
}
