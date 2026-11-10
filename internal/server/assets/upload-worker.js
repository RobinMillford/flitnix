/*
 * Flitnix upload transport worker.
 *
 * Streams one file to a PUT endpoint from a dedicated worker thread. Keeping
 * the transfer off the page's main thread means layout, painting and progress
 * rendering cannot contend with the upload for the single thread the page
 * would otherwise share with it.
 *
 * Protocol (main thread <-> worker):
 *   in : {type: "start", uploadId, url, headers, file}
 *   in : {type: "abort", uploadId}
 *   out: {type: "progress", uploadId, loaded, total}
 *   out: {type: "done", uploadId, status, responseText}
 *   out: {type: "error", uploadId, kind, message}
 *
 * HTTP-level outcomes (including 4xx/5xx) are reported as "done": the status
 * belongs to the caller. Only transport-level failures and cancellation are
 * reported as "error".
 */
"use strict";

var active = Object.create(null);

self.onmessage = function (ev) {
  var msg = ev.data;
  if (!msg || !msg.type) return;
  if (msg.type === "start") startUpload(msg);
  else if (msg.type === "abort") abortUpload(msg.uploadId);
};

function startUpload(msg) {
  var id = msg.uploadId;
  var xhr = new XMLHttpRequest();
  active[id] = xhr;

  function settle(fn) {
    return function () {
      if (active[id] === xhr) delete active[id];
      fn();
    };
  }

  var lastPost = 0;
  xhr.upload.onprogress = function (e) {
    if (!e.lengthComputable) return;
    var now = Date.now();
    // Coalesce progress to roughly 10 updates per second. The page applies
    // its own paint throttle on top; this bound simply keeps the message
    // queue from growing without limit on a fast local network.
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

function abortUpload(id) {
  var xhr = active[id];
  if (!xhr) return;
  try {
    xhr.abort();
  } catch (e) {
    // Already settled; nothing to withdraw.
  }
}
