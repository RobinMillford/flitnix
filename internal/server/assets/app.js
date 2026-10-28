/* Flitnix phone page — plain JavaScript, no external scripts.
 *
 * Session token handling: the token arrives only in the URL fragment
 * (#43-char-base64url). It is copied into a closure variable and the
 * fragment is scrubbed from the address bar immediately. It is never
 * stored, never logged, and never placed in a query string; every API
 * call carries it as a Bearer header.
 */
(function () {
  "use strict";

  var TOKEN_RE = /^[A-Za-z0-9_-]{43}$/;
  var POLL_MS = 1200;           // approval polling cadence
  var POLL_BACKOFF_MAX_MS = 5000; // ceiling for retry delay after a failed poll
  var PROGRESS_POLL_MS = 500;   // download progress cadence while it is live
  var MAX_PROGRESS_FAILURES = 20; // consecutive failed progress polls before giving up
  var PAINT_MIN_MS = 200;       // upload progress repaint throttle
  var CANCEL_RETRIES = 3;       // DELETE attempts before showing the real state
  var FIREFOX_LARGE_UPLOAD_BYTES = 256 * 1024 * 1024;

  // ------------------------------------------------------------------
  // Token: read once, then scrub the fragment.
  // ------------------------------------------------------------------
  var token = null;
  var frag = window.location.hash || "";
  if (frag.length > 1) {
    var cand = frag.slice(1);
    try { cand = decodeURIComponent(cand); } catch (err) { /* keep raw */ }
    if (TOKEN_RE.test(cand)) token = cand;
  }

  function diagnoseUpload(kind, job) {
    if (!window.console || typeof window.console.info !== "function") return;
    window.console.info("flitnix upload", {
      kind: kind, phase: job.phase, size: job.size,
      computable: job.size > 0
    });
  }

  function firefoxLargeUploadWarning(bytes) {
    return /Firefox/i.test(navigator.userAgent || "") &&
      bytes >= FIREFOX_LARGE_UPLOAD_BYTES;
  }
  if (frag) {
    // One-time use: the link disappears from the address bar (and from any
    // "reopen tab" paste target) the moment the page takes the token.
    window.history.replaceState(null, "",
      window.location.pathname + window.location.search);
  }

  // ------------------------------------------------------------------
  // Handles
  // ------------------------------------------------------------------
  var statusEl = document.getElementById("status");
  var statusText = statusEl ? statusEl.querySelector(".status__text") : null;
  var flowEl = document.getElementById("flow");
  var rescanCard = document.getElementById("rescan");
  var rescanText = document.getElementById("rescan-text");
  var pickBtn = document.getElementById("pick-btn");
  var pickFolderBtn = document.getElementById("pick-folder-btn");
  var fileInput = document.getElementById("file-input");
  var folderInput = document.getElementById("folder-input");
  var queueEl = document.getElementById("queue");
  var filesEl = document.getElementById("files");
  var filesEmptyEl = document.getElementById("files-empty");
  var refreshBtn = document.getElementById("refresh-btn");
  var downloadSelectedBtn = document.getElementById("download-selected-btn");
  var filesRefreshGeneration = 0;

  var appState = { ended: false };
  var queue = [];      // upload jobs, FIFO
  var pumpBusy = false; // one upload in flight at a time
  var activeBatch = null;

  // ------------------------------------------------------------------
  // Small helpers
  // ------------------------------------------------------------------
  function el(tag, className, text) {
    var n = document.createElement(tag);
    if (className) n.className = className;
    if (text != null) n.textContent = text;
    return n;
  }

  function fmtBytes(n) {
    if (typeof n !== "number" || !isFinite(n) || n < 0) return "";
    var units = ["B", "KB", "MB", "GB", "TB"];
    var v = n, u = 0;
    while (v >= 1000 && u < units.length - 1) { v /= 1000; u += 1; }
    var digits = u === 0 || v >= 100 ? 0 : v >= 10 ? 1 : 2;
    return v.toFixed(digits) + " " + units[u];
  }

  function setStatus(stateName, text) {
    if (statusEl) statusEl.dataset.state = stateName;
    if (statusText) statusText.textContent = text;
  }

  // statusLabel: the one-line status word. The default names a genuine
  // authority-side ending; callers whose cause is not an authority ending
  // (a reload without a pairing token, an unrecognized token) must pass
  // their own so the status never claims a session the server never
  // declared dead.
  function endSession(title, why, statusLabel) {
    if (appState.ended) return;
    appState.ended = true;
    teardownJobs();
    if (statusEl) statusEl.dataset.state = "ended";
    if (statusText) statusText.textContent = statusLabel || "Session ended";
    if (flowEl) flowEl.hidden = true;
    if (rescanCard) {
      var h = rescanCard.querySelector("h2");
      if (h && title) h.textContent = title;
      if (rescanText && why) rescanText.textContent = why;
      rescanCard.hidden = false;
    }
  }

  // A 401 from a token the desktop once issued means the session really ended
  // (idle expiry or revoked on the desktop). Anything else — a dropped network,
  // a 5xx, a timeout — is NOT a session ending and never reaches this path.
  function endUnauthorized(code) {
    if (code === "session_expired") {
      endSession("Session expired",
        "This session expired after sitting idle. Scan the QR code shown on the Flitnix desktop to start a new one.");
      return;
    }
    if (code === "session_revoked") {
      endSession("Session ended",
        "The desktop ended this transfer session. Scan the QR code shown on the Flitnix desktop again to start a fresh one.");
      return;
    }
    // The desktop rejected the token outright: this page is presenting a
    // token it never issued (stale reload, pair-of-sessions confusion). It
    // did NOT end any session, so the wording must not claim it did.
    endSession("Transfer stopped",
      "The desktop did not recognize this session. Open this page again from the QR code shown on the Flitnix desktop.",
      "Pairing required");
  }

  // The desktop is unreachable right now. This is transient: the established
  // session is kept, transfers stay as they are, and polling retries.
  //
  // Each polling loop reports for itself. An upload batch poll (1.2 s, backing
  // off to 5 s) and a download progress poll (0.5 s) run at the same time, so
  // one shared flag let a single successful download poll clear the banner the
  // failing batch poll had just raised — and the reverse. The banner stays up
  // while any loop reports the desktop unreachable, and clears when the last
  // of them gets through again.
  var reconnecting = false;
  var unreachable = {};
  function setReconnecting(on, who) {
    if (appState.ended) return;
    if (on) unreachable[who] = true;
    else delete unreachable[who];
    var lost = Object.keys(unreachable).length > 0;
    if (lost === reconnecting) return;
    reconnecting = lost;
    if (lost) setStatus("reconnecting", "Lost contact with the desktop — retrying…");
    else setStatus("ready", "Connected — pick files to send or download the shared ones");
  }

  // Delay before retry number n (1-based): grows from POLL_MS, bounded.
  function backoffMs(failures) {
    return Math.min(POLL_BACKOFF_MAX_MS, POLL_MS * Math.pow(1.6, Math.max(0, failures - 1)));
  }

  // Wake any waiting poll as soon as the network returns.
  var wakeups = [];
  window.addEventListener("online", function () {
    var fns = wakeups; wakeups = [];
    fns.forEach(function (fn) { fn(); });
  });
  function waitThen(holder, ms, fn) {
    var done = false;
    function run() {
      if (done) return;
      done = true;
      clearTimeout(holder.timer);
      holder.timer = null;
      var i = wakeups.indexOf(run);
      if (i >= 0) wakeups.splice(i, 1);
      fn();
    }
    holder.timer = setTimeout(run, ms);
    wakeups.push(run);
  }

  // ------------------------------------------------------------------
  // API plumbing (Bearer only — the token never rides in a URL)
  // ------------------------------------------------------------------
  function readBody(res) {
    return res.text().then(function (text) {
      if (!text) return null;
      try { return JSON.parse(text); } catch (err) { return null; }
    });
  }

  function api(path, opts) {
    if (token == null) {
      var denied = new Error("no session token");
      denied.silent = true;
      return Promise.reject(denied);
    }
    var o = opts || {};
    var headers = o.headers || {};
    headers["Authorization"] = "Bearer " + token;
    headers["Accept"] = "application/json";
    return fetch(path, {
      method: o.method || "GET",
      headers: headers,
      body: o.body,
      cache: "no-store"
    }).then(function (res) {
      return readBody(res).then(function (body) {
        if (res.status === 401) { endUnauthorized(body && body.code); }
        if (res.status >= 200 && res.status < 300) return body;
        var msg = body && body.error ? body.error
          : "Request failed (" + res.status + ")";
        var err = new Error(msg);
        err.status = res.status;
        err.silent = res.status === 401; // session UI already switching over
        throw err;
      });
    });
  }

  // ------------------------------------------------------------------
  // Upload queue
  //
  // Job phases: queued → declaring → waiting (pending approval) → sending
  // → done; or failed / canceled at any point. Everything from declare to
  // content PUT is sequential: the desktop approves one file at a time.
  // ------------------------------------------------------------------
  function phaseText(job) {
    switch (job.phase) {
      case "queued": return "Queued";
      case "declaring": return "Contacting the desktop…";
      case "waiting": return "Waiting for approval on the desktop…";
      case "sending": return "Transferring…";
      case "canceling": return "Canceling…";
      case "done": return "Sent";
      case "canceled": return "Canceled";
      case "failed": return "Failed";
    }
    return job.phase;
  }

  function paint(job) {
    var li = job.el;
    if (!li) return;
    li.dataset.phase = job.phase;
    var r = job.refs;
    var text = phaseText(job);
    var active = job.phase === "sending" || job.phase === "done" ||
                 (job.phase === "canceling" && job.uploaded > 0);
    r.bar.hidden = !active;
    var renderedPercent = job.size > 0
      ? Math.min(100, Math.max(0, Math.round((job.uploaded / job.size) * 100)))
      : (job.phase === "done" ? 100 : 0);
    if (job.phase === "done") renderedPercent = 100;
    r.fill.style.width = renderedPercent + "%";
    r.bar.setAttribute("aria-valuenow", String(renderedPercent));
    if (job.phase === "sending" && job.uploaded > 0 && job.size > 0) {
      text += " " + renderedPercent + "%";
    }
    r.state.textContent = text;
    r.err.hidden = !job.err;
    if (job.err) r.err.textContent = job.err;
    // Terminal jobs release their File object: on phones the browser's
    // blob backing is the largest store a finished job would still pin.
    // A failed-but-retryable job deliberately keeps the file — Retry
    // reuploads the same object. paint is idempotent-safe here because
    // the conditions depend only on the terminal phase, never progress.
    // A "canceling" job keeps it too: if the desktop refuses or the cancel
    // cannot be confirmed, the file may still have to be sent.
    if (job.file && (job.phase === "done" || job.phase === "canceled" ||
                     (job.phase === "failed" && !job.retryable))) {
      job.file = null;
    }
    // Button is state-dependent.
    var label, action;
    if (job.phase === "sending" || job.phase === "queued" ||
        job.phase === "declaring" || job.phase === "waiting") {
      label = "Cancel"; action = function () { requestCancel(job); };
    } else if (job.phase === "canceling") {
      label = "Canceling…"; action = function () { /* in flight */ };
    } else if (job.phase === "done") {
      label = "Dismiss"; action = function () { removeJob(job); };
    } else {
      label = job.retryable ? "Retry" : "Dismiss";
      action = job.retryable
        ? function () { retryJob(job); }
        : function () { removeJob(job); };
    }
    r.btn.textContent = label;
    r.btn.disabled = job.phase === "canceling";
    if (job.btnAction) r.btn.removeEventListener("click", job.btnAction);
    job.btnAction = action;
    r.btn.addEventListener("click", action);
  }

  function buildItem(job) {
    var li = el("li", "queue-item");
    var top = el("div", "queue-item__top");
    top.appendChild(el("span", "queue-item__name", job.name));
    top.appendChild(el("span", "queue-item__size", fmtBytes(job.size)));
    var row = el("div", "queue-item__row");
    var state = el("span", "queue-item__state");
    var bar = el("div", "queue-item__bar");
    bar.setAttribute("role", "progressbar");
    bar.setAttribute("aria-valuemin", "0");
    bar.setAttribute("aria-valuemax", "100");
    bar.setAttribute("aria-valuenow", "0");
    var fill = el("i", "queue-item__fill");
    bar.appendChild(fill);
    bar.hidden = true;
    var btn = el("button", "queue-item__btn");
    btn.type = "button";
    var errLine = el("p", "queue-item__err");
    errLine.hidden = true;
    row.appendChild(state);
    row.appendChild(bar);
    row.appendChild(btn);
    li.appendChild(top);
    li.appendChild(row);
    li.appendChild(errLine);
    job.el = li;
    job.refs = { state: state, bar: bar, fill: fill, btn: btn, err: errLine };
    paint(job);
    return li;
  }

  function addFiles(fileList, folder) {
    Array.prototype.slice.call(fileList).forEach(function (f) {
      var job = {
        // name/size are cached here: paint, buildItem and the manifest
        // read the cache, so a job never has to dereference its File
        // object again. File objects are the only large references a
        // job holds and can then be released at the terminal phase.
        file: f, name: f.name, size: f.size, id: null, phase: "queued", uploaded: 0,
        failures: 0, retryable: false, err: null,
        xhr: null, timer: null, removed: false, cancelRequested: false,
        cancelInFlight: false,
        el: null, refs: null, btnAction: null, batchID: null, itemID: null,
        relativePath: folder ? f.webkitRelativePath.slice(folder.length + 1) : "",
        rootName: folder || "", group: folder ? "folder:" + folder : "files"
      };
      queue.push(job);
      queueEl.appendChild(buildItem(job));
      if (firefoxLargeUploadWarning(job.size)) {
        setStatus("warning", "Firefox may pause during very large uploads. Keep this page visible; Chrome or Chromium is a temporary alternative if it stalls.");
      }
    });
    pump();
  }

  function teardownJobs() {
    queue.forEach(function (job) {
      job.removed = true;
      if (job.timer) { clearTimeout(job.timer); job.timer = null; }
      if (job.xhr) { job.cancelRequested = true; try { job.xhr.abort(); } catch (e) { /* gone */ } }
    });
  }

  function removeJob(job) {
    job.removed = true;
    if (job.timer) { clearTimeout(job.timer); job.timer = null; }
    if (job.xhr) { job.cancelRequested = true; try { job.xhr.abort(); } catch (e) { /* gone */ } }
    var i = queue.indexOf(job);
    if (i >= 0) queue.splice(i, 1);
    if (job.el && job.el.parentNode) job.el.parentNode.removeChild(job.el);
    // If the removed job was the blocker, let the queue advance.
    if (!pumpBusy && !appState.ended) pump();
  }

  function requestCancel(job) {
    if (job.removed || appState.ended) return;
    if (job.phase === "queued") {
      // Nothing was declared to the desktop yet, so there is nothing to
      // withdraw: removing the row is already the whole cancellation.
      removeJob(job);
      return;
    }
    if (job.phase === "declaring") {
      // The batch POST is in flight and this job has no IDs yet. The intent
      // is carried to the response handler, which withdraws the declared
      // item instead of uploading it.
      job.cancelRequested = true;
      job.phase = "canceling";
      paint(job);
      return;
    }
    if (job.phase === "waiting" || job.phase === "sending") {
      job.cancelRequested = true;
      job.phase = "canceling";
      paint(job);
      // An active PUT is deliberately not aborted here: the server's cancel
      // releases the lease and aborts the sink itself, so the DELETE wins the
      // race and the rest of the batch is untouched. Aborting first could
      // surface as a stream failure and end the whole batch.
      cancelDeclaredItem(job, 1);
      return;
    }
    // done / canceled / failed rows expose Retry or Dismiss instead.
  }

  // cancelDeclaredItem asks the desktop to withdraw one already declared
  // item. The response (or an authoritative refresh after a refusal) is the
  // only thing allowed to turn the row into "canceled": a cancel is never
  // shown as done from local intent alone.
  function cancelDeclaredItem(job, attempt) {
    if (job.removed || appState.ended) return;
    // A retry that lost its purpose (the snapshot already settled the row)
    // must not send another request.
    if (job.phase !== "canceling") return;
    if (!job.batchID || !job.itemID) { refreshBatchAfterCancel(); return; }
    job.cancelInFlight = true;
    api("/api/batches/" + encodeURIComponent(job.batchID) +
        "/items/" + encodeURIComponent(job.itemID), { method: "DELETE" })
      .then(function (batch) {
        job.cancelInFlight = false;
        if (job.removed || appState.ended) return;
        applyBatchSnapshot(batch);
        if (activeBatch) watchBatch();
        else pump();
      })
      .catch(function (err) {
        job.cancelInFlight = false;
        if (job.removed || appState.ended || (err && err.silent)) return;
        if (err && (err.status === 404 || err.status === 409)) {
          // 409 means the item moved past cancellation (completed, failed or
          // the batch ended); 404 means the desktop no longer owns it. Either
          // way the authoritative snapshot decides what to show.
          refreshBatchAfterCancel();
          return;
        }
        if (attempt < CANCEL_RETRIES) {
          setReconnecting(true, "batch");
          waitThen(job, backoffMs(attempt), function () {
            cancelDeclaredItem(job, attempt + 1);
          });
          return;
        }
        refreshBatchAfterCancel();
      });
  }

  function refreshBatchAfterCancel() {
    if (!activeBatch || appState.ended) return;
    api("/api/batches/" + encodeURIComponent(activeBatch.id)).then(function (batch) {
      if (!activeBatch || appState.ended) return;
      applyBatchSnapshot(batch);
      if (activeBatch) watchBatch();
      else pump();
    }).catch(function (err) {
      if (!activeBatch || appState.ended || (err && err.silent)) return;
      if (err && err.status === 404) {
        // The desktop no longer knows the batch. Nothing can be confirmed,
        // so the row says so instead of pretending it was canceled.
        activeBatch.jobs.forEach(function (other) {
          if (other.removed || other.phase === "done" || other.phase === "canceled") return;
          other.phase = "failed";
          other.err = "The desktop no longer has this transfer. Retry to send it again.";
          other.retryable = true;
          other.cancelRequested = false;
          paint(other);
        });
        activeBatch = null; pumpBusy = false; pump();
        return;
      }
      // Still unreachable: keep the row in "canceling" and let the regular
      // batch poll establish the truth when the connection returns.
      activeBatch.failures += 1;
      setReconnecting(true, "batch");
      var batch = activeBatch;
      waitThen(batch, backoffMs(batch.failures), pollBatch);
    });
  }


  function cancelJob(job, msg) {
    if (job.removed || appState.ended) return;
    job.xhr = null;
    job.phase = "canceled";
    job.err = msg || null;
    paint(job);
    pumpBusy = false;
    pump();
  }

  function failJob(job, msg, retryable) {
    if (job.removed || appState.ended) { pumpBusy = false; return; }
    job.xhr = null;
    job.phase = "failed";
    job.err = msg || "Failed";
    job.retryable = !!retryable;
    paint(job);
    pumpBusy = false;
    pump();
  }

  function doneJob(job) {
    if (job.removed || appState.ended) { pumpBusy = false; return; }
    job.xhr = null;
    job.timer = null;
    job.phase = "done";
    job.err = null;
    paint(job);
    pumpBusy = false;
    pump();
  }

  function retryJob(job) {
    if (appState.ended) return;
    job.id = null; job.phase = "queued"; job.err = null;
    job.uploaded = 0; job.failures = 0; job.retryable = false;
    job.cancelRequested = false; job.cancelInFlight = false;
    job.refs.fill.style.width = "0%";
    job.refs.bar.hidden = true;
    paint(job);
    pump();
  }

  function pump() {
    if (pumpBusy || appState.ended) return;
    var first = queue.find(function (job) { return job.phase === "queued"; });
    if (!first) return;
    var jobs = queue.filter(function (job) {
      return job.phase === "queued" && job.group === first.group;
    });
    if (!jobs.length) return;
    pumpBusy = true;
    jobs.forEach(function (job) { job.phase = "declaring"; paint(job); });
    api("/api/batches", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        direction: "phone_to_desktop",
        kind: jobs[0].rootName ? "folder" : "files",
        rootName: jobs[0].rootName || undefined,
        items: jobs.map(function (job) {
          return {
            relativePath: jobs[0].rootName ? job.relativePath : job.name,
            size: job.size
          };
        })
      })
    }).then(function (batch) {
      if (appState.ended) { pumpBusy = false; return; }
      if (!batch || !batch.id || !Array.isArray(batch.items) ||
          batch.items.length !== jobs.length) {
        throw new Error("The desktop gave an unexpected batch reply");
      }
      activeBatch = { id: batch.id, jobs: jobs, failures: 0, timer: null };
      var cancels = [];
      batch.items.forEach(function (item, index) {
        var job = jobs[index];
        if (job.removed) return;
        job.batchID = batch.id;
        job.itemID = item.id;
        if (job.cancelRequested) {
          // Withdrawn while the declaration was in flight: now that the item
          // has an ID, withdraw it on the desktop instead of uploading it.
          job.phase = "canceling";
          paint(job);
          cancels.push(job);
        } else {
          job.phase = "waiting";
          paint(job);
        }
      });
      pumpBusy = false;
      cancels.forEach(function (job) { cancelDeclaredItem(job, 1); });
      watchBatch();
    }).catch(function (err) {
      jobs.forEach(function (job) {
        if (job.removed) return;
        if (job.cancelRequested) {
          // The declaration itself failed, so nothing exists on the desktop:
          // a local cancellation is accurate and needs no server call.
          job.phase = "canceled";
          job.err = "Canceled before it was sent.";
          job.cancelRequested = false;
          paint(job);
          return;
        }
        job.phase = "failed";
        job.err = (!err || !err.status || err.status >= 500)
          ? "Could not reach the desktop. Check the Wi-Fi connection and retry."
          : (err.message || "The desktop refuses this batch.");
        job.retryable = true;
        paint(job);
      });
      pumpBusy = false;
    });
  }

  function watchBatch() {
    if (!activeBatch || appState.ended) return;
    activeBatch.timer = setTimeout(pollBatch, POLL_MS);
  }

  // applyBatchSnapshot folds one authoritative BatchView into the local job
  // rows. Only the desktop decides that a file is done, failed or canceled;
  // local intent never does. A row stays "canceling" while its DELETE is in
  // flight, and after an unconfirmed cancel it falls back to the real queue
  // state instead of claiming a cancellation that never happened.
  function applyBatchSnapshot(batch) {
    if (!activeBatch || !batch || !Array.isArray(batch.items)) return;
    batch.items.forEach(function (item) {
      var job = activeBatch.jobs[item.sequence];
      if (!job || job.removed) return;
      var changed = false;
      if (typeof item.transferred === "number" && item.transferred !== job.uploaded) {
        job.uploaded = item.transferred;
        changed = true;
      }
      var phase = job.phase;
      var err = job.err;
      var retryable = job.retryable;
      // A cancel is still progressing while its DELETE is in flight or a
      // retry is scheduled. A snapshot from that window must not silently
      // abandon the intent: the row stays "canceling" until the request is
      // answered or the retries are exhausted.
      var cancelPending = job.phase === "canceling" && (job.cancelInFlight || !!job.timer);
      var decided = true;
      switch (item.state) {
        case "completed":
          phase = "done"; err = null; retryable = false; break;
        case "canceled":
          phase = "canceled"; err = "Upload canceled."; retryable = false; break;
        case "rejected":
          phase = "failed"; err = "The desktop declined this file."; retryable = false; break;
        case "failed":
          phase = "failed"; err = item.error || "Transfer failed."; retryable = true; break;
        // The server's item vocabulary is queued / pending / approved /
        // transferring / completed / failed / canceled / rejected
        // (transfer.ItemState). Reading "uploading" here — the *upload
        // record* vocabulary — meant this branch never matched: an item that
        // was really transferring fell through to the idle default and the
        // row claimed its cancellation "was not confirmed; the file is still
        // queued" while the bytes were actually in flight.
        case "transferring":
          decided = false;
          if (!cancelPending) {
            if (job.phase === "canceling") {
              // The cancel was refused or could not be confirmed: reflect
              // the real state and drop the intent instead of claiming a
              // cancellation that never happened.
              job.cancelRequested = false;
              err = "Cancellation was not confirmed; the file is still transferring.";
            }
            phase = "sending";
          }
          break;
        default: // pending / approved / queued: not started yet
          decided = false;
          if (!cancelPending && job.phase === "canceling") {
            job.cancelRequested = false;
            phase = "waiting";
            err = "Cancellation was not confirmed; the file is still queued.";
          }
      }
      if (decided) job.cancelRequested = false;
      if (phase !== job.phase || err !== job.err) changed = true;
      job.phase = phase;
      job.err = err;
      job.retryable = retryable;
      if (changed) paint(job);
    });
  }

  function pollBatch() {
    if (!activeBatch || appState.ended) return;
    api("/api/batches/" + encodeURIComponent(activeBatch.id)).then(function (batch) {
      if (!activeBatch || appState.ended) return;
      activeBatch.failures = 0;
      setReconnecting(false, "batch");
      applyBatchSnapshot(batch);
      if (!activeBatch) return;
      if (batch.state === "paused") {
        activeBatch.jobs.forEach(function (job) {
          if (job.phase === "waiting") job.phase = "queued";
          paint(job);
        });
        activeBatch = null; pumpBusy = false; pump(); return;
      }
      if (batch.approvalState === "rejected" || batch.state === "rejected") {
        activeBatch = null; pumpBusy = false; pump(); return;
      }
      // Uploads are strictly FIFO across the batch. The head job — the first
      // row that is not terminal — decides what happens next: a canceling
      // head blocks later items until its DELETE is acknowledged (claiming a
      // later item would be refused by the desktop), a waiting head uploads
      // once approved, and no head at all means the batch is settled.
      var head = null;
      for (var i = 0; i < activeBatch.jobs.length; i++) {
        var job = activeBatch.jobs[i];
        if (job.removed || job.phase === "done" || job.phase === "canceled" ||
            job.phase === "failed") continue;
        head = job;
        break;
      }
      if (!head) {
        activeBatch = null; pumpBusy = false; pump(); return;
      }
      if (head.phase === "canceling") { watchBatch(); return; }
      if (head.phase === "waiting" &&
          (batch.approvalState === "approved" || batch.state === "queued")) {
        head.phase = "sending"; head.uploaded = 0; paint(head);
        sendBatchItem(head);
        return;
      }
      watchBatch();
    }).catch(function (err) {
      if (!activeBatch || appState.ended || (err && err.silent)) return;
      if (err && err.status === 404) {
        // The desktop genuinely no longer knows this batch.
        activeBatch.jobs.forEach(function (job) {
          if (job.removed || job.phase === "done" || job.phase === "canceled") return;
          job.phase = "failed"; job.err = "The desktop no longer has this transfer. Retry to send it again."; job.retryable = true;
          job.cancelRequested = false;
          paint(job);
        });
        activeBatch = null; pumpBusy = false;
        return;
      }
      // Anything else (network drop, timeout, 5xx) is transient. Keep the
      // session and the queue, show that we are reconnecting, and retry with
      // a bounded backoff. A real session end arrives as a 401 above.
      activeBatch.failures += 1;
      setReconnecting(true, "batch");
      var batch = activeBatch;
      waitThen(batch, backoffMs(batch.failures), pollBatch);
    });
  }

  function sendBatchItem(job) {
    putBatchContent(job).then(function () {
      if (job.phase === "done" || job.phase === "canceled") return;
      job.phase = "done"; job.err = null; paint(job);
      watchBatch();
    }).catch(function (e) {
      if (!activeBatch || job.removed || appState.ended) return;
      // A row the desktop already settled must not be overwritten by a late
      // PUT failure (e.g. the server canceled the sink after our DELETE).
      if (job.phase === "done" || job.phase === "canceled") return;
      if (job.cancelRequested) {
        // The phone withdrew this item; the DELETE response is the authority.
        // The aborted/failed PUT is its expected consequence and must not
        // tear down the batch or requeue the files behind it.
        job.phase = "canceling"; job.err = null; job.retryable = false;
        paint(job);
        if (activeBatch) watchBatch();
        return;
      }
      if (e && e.aborted) { job.phase = "canceled"; job.err = "Upload canceled."; }
      else { job.phase = "failed"; job.err = (e && e.message) || "Transfer failed."; job.retryable = true; }
      activeBatch.jobs.forEach(function (other) {
        if (other !== job && other.phase === "waiting") other.phase = "queued";
        if (other !== job) paint(other);
      });
      paint(job);
      activeBatch = null; pumpBusy = false;
    });
  }

  function errorCode(xhr) {
    try { var parsed = JSON.parse(xhr.responseText); return parsed && parsed.code; } catch (e) { return undefined; }
  }

  function putBatchContent(job) {
    return new Promise(function (resolve, reject) {
      var xhr = new XMLHttpRequest();
      job.xhr = xhr;
      var lastPaint = 0;
      xhr.open("PUT", "/api/batches/" + encodeURIComponent(job.batchID) +
        "/items/" + encodeURIComponent(job.itemID) + "/content");
      xhr.upload.addEventListener("progress", function (ev) {
        if (!ev.lengthComputable) return;
        job.uploaded = Math.min(job.size, ev.loaded);
        var now = Date.now();
        if (now - lastPaint >= PAINT_MIN_MS || ev.loaded === ev.total) { lastPaint = now; paint(job); }
      });
      xhr.setRequestHeader("Authorization", "Bearer " + token);
      xhr.setRequestHeader("Content-Type", "application/octet-stream");
      xhr.onload = function () {
        job.xhr = null;
        if (job.removed || appState.ended) { reject({ aborted: true }); return; }
        if (xhr.status === 401) { endUnauthorized(errorCode(xhr)); reject({ aborted: true }); return; }
        if (xhr.status >= 200 && xhr.status < 300) { diagnoseUpload("completed", job); resolve(); return; }
        var msg = "Upload failed (" + xhr.status + ")";
        try { var parsed = JSON.parse(xhr.responseText); if (parsed && parsed.error) msg = parsed.error; } catch (e) {}
        reject(new Error(msg));
      };
      xhr.onerror = function () { diagnoseUpload("network-error", job); job.xhr = null; reject(new Error("Network error while uploading. The desktop may have received part of this file; retry to send it again.")); };
      xhr.ontimeout = function () { diagnoseUpload("timeout", job); job.xhr = null; reject(new Error("Upload timed out. Keep the page visible and retry this file.")); };
      xhr.onabort = function () {
        diagnoseUpload(job.cancelRequested ? "canceled" : "aborted", job);
        job.xhr = null;
        reject({ aborted: true });
      };
      xhr.send(job.file);
    });
  }

  // ------------------------------------------------------------------
  // Shared files (desktop → phone)
  // ------------------------------------------------------------------
  function refreshFiles() {
    var generation = ++filesRefreshGeneration;
    refreshBtn.disabled = true;
    var prev = refreshBtn.textContent;
    refreshBtn.textContent = "Loading…";
    api("/api/sources").then(function (data) {
      if (appState.ended || generation !== filesRefreshGeneration) return;
      drawFiles(data || []);
    }).catch(function (err) {
      if (appState.ended || (err && err.silent)) return;
      if (generation === filesRefreshGeneration) {
        drawFiles([]);
        filesEmptyEl.textContent = err.message || "Could not load the desktop files.";
      }
    }).then(function () {
      if (generation === filesRefreshGeneration) {
        refreshBtn.disabled = false;
        refreshBtn.textContent = prev;
      }
    });
  }

  function drawFiles(list) {
    filesEl.textContent = "";
    var items = (Array.isArray(list) ? list : []).filter(function (f) {
      return f && f.id && f.name && (f.kind === "file" || f.kind === "folder");
    });
    filesEmptyEl.hidden = items.length > 0;
    var actions = document.getElementById("file-actions");
    if (actions) actions.hidden = items.length === 0;
    downloadSelectedBtn.hidden = items.length === 0;
    downloadSelectedBtn.disabled = items.length === 0;
    if (!items.length) {
      filesEmptyEl.textContent = "Nothing is shared with this session yet.";
      return;
    }
    items.forEach(function (f) {
      filesEl.appendChild(fileRow(f));
    });
  }

  function fileRow(f) {
    var li = el("li", "file-item");
    var left = el("div", "file-item__meta");
    left.appendChild(el("span", "file-item__name", f.name));
    left.appendChild(el("span", "file-item__size", f.kind === "folder"
      ? (f.itemCount + " items · " + fmtBytes(f.totalBytes))
      : fmtBytes(f.totalBytes)));
    var check = document.createElement("input");
    check.type = "checkbox";
    check.className = "source-check";
    check.value = f.id;
    check.setAttribute("aria-label", "Select " + f.name);
    check.addEventListener("change", updateDownloadSelection);
    li.appendChild(check);
    var btn = el("button", "btn btn--small");
    btn.type = "button";
    btn.textContent = "Download";
    btn.addEventListener("click", function () { downloadSources([f], btn); });
    li.appendChild(left);
    li.appendChild(btn);
    return li;
  }

  function downloadSources(sources, btn) {
    btn.disabled = true;
    var prev = btn.textContent;
    btn.textContent = "Preparing…";
    fetch("/api/download-grants", {
      method: "POST",
      headers: { "Authorization": "Bearer " + token, "Content-Type": "application/json" },
      body: JSON.stringify({ sourceIds: sources.map(function (source) { return source.id; }) }),
      cache: "no-store"
    }).then(function (res) {
      if (res.status === 401) {
        return res.json().catch(function () { return null; }).then(function (body) {
          endUnauthorized(body && body.code);
          throw new Error("silent");
        });
      }
      if (!res.ok) {
        return res.text().then(function (t) {
          var msg = "Download failed (" + res.status + ")";
          try {
            var parsed = JSON.parse(t);
            if (parsed && parsed.error) msg = parsed.error;
          } catch (e) { /* plain body */ }
          throw new Error(msg);
        });
      }
      return res.json().then(function (grant) {
        if (!grant || !grant.downloadUrl) throw new Error("The desktop returned an invalid download grant.");
        startNativeDownload(grant.downloadUrl, grant.progressId);
        if (grant.progressId) watchDownloadProgress(grant.progressId);
      });
    }).catch(function (err) {
      if (err && err.message === "silent") return;
      var msg = err && err.message ? err.message : "Download failed.";
      if (err instanceof TypeError) {
        msg = "Network error — could not reach the desktop.";
      }
      showDownloadError(sources[0], msg);
    }).then(function () {
      btn.disabled = false;
      btn.textContent = prev;
    });
  }

  // The browser's own download manager fetches the grant URL. It is loaded in
  // a hidden frame, never by navigating this page: if the server answers with
  // an error instead of an attachment, a top-level navigation would replace
  // this page and discard the session token held only in memory (the pairing
  // link is single-use), which looks like "Session ended". A frame can fail
  // without costing the session; the progress watcher reports the outcome.
  var downloadFrames = {};
  function startNativeDownload(url, progressId) {
    var frame = document.createElement("iframe");
    frame.hidden = true;
    frame.setAttribute("aria-hidden", "true");
    frame.tabIndex = -1;
    frame.src = url;
    document.body.appendChild(frame);
    var key = progressId || url;
    downloadFrames[key] = frame;
    // Safety net only; normally the watcher removes it at the terminal state.
    setTimeout(function () { removeDownloadFrame(key); }, 30 * 60 * 1000);
  }
  function removeDownloadFrame(key) {
    var frame = downloadFrames[key];
    if (!frame) return;
    delete downloadFrames[key];
    if (frame.parentNode) frame.parentNode.removeChild(frame);
  }

  function updateDownloadSelection() {
    var selected = filesEl.querySelectorAll(".source-check:checked");
    downloadSelectedBtn.disabled = selected.length === 0;
  }

  // Follows one archive/file download. It polls only while the download is
  // live and stops at the first terminal answer; a transient failure retries
  // with a bounded backoff instead of abandoning the watch.
  function watchDownloadProgress(id) {
    var failures = 0;
    function tick() {
      if (appState.ended) return;
      api("/api/download-progress/" + encodeURIComponent(id)).then(function (p) {
        failures = 0;
        setReconnecting(false, "download");
        if (p.state === "failed" || p.state === "expired") {
          removeDownloadFrame(id);
          // Report the reason on the list's status line, then let the list
          // reload into its own state after the message has been read. Without
          // the reload the failure copy stayed above a populated file list for
          // the rest of the session, contradicting every later download.
          filesEmptyEl.hidden = false;
          filesEmptyEl.textContent = "Download failed: " + (p.error || "source unavailable");
          setTimeout(function () { if (!appState.ended) refreshFiles(); }, 3500);
          return;
        }
        if (p.state === "completed") { removeDownloadFrame(id); return; }
        setTimeout(tick, PROGRESS_POLL_MS);
      }).catch(function (err) {
        if (appState.ended || (err && err.silent)) return;
        if (err && err.status === 404) return; // record already cleaned up
        failures += 1;
        if (failures >= MAX_PROGRESS_FAILURES) {
          setReconnecting(false, "download");
          return;
        }
        setTimeout(tick, backoffMs(failures));
      });
    }
    setTimeout(tick, PROGRESS_POLL_MS);
  }

  function showDownloadError(f, msg) {
    // Reuse the empty line as an inline error carrier.
    filesEmptyEl.hidden = false;
    filesEmptyEl.textContent = "Saving \"" + f.name + "\": " + msg;
    setTimeout(function () {
      if (!appState.ended) refreshFiles();
    }, 3500);
  }

  function saveBlob(blob, name) {
    var url = URL.createObjectURL(blob);
    var a = document.createElement("a");
    a.href = url;
    a.download = name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    window.setTimeout(function () { URL.revokeObjectURL(url); }, 10000);
  }

  /* Content-Disposition parsing (mime.FormatMediaType output):
   *   attachment; filename="a b.png"            (ASCII names)
   *   attachment; filename*=utf-8''a%20b.png    (non-ASCII names)
   * Percent-decodes RFC 5987 values; falls back to the listed name. */
  function dispositionName(cd, fallback) {
    if (!cd) return fallback;
    var m = cd.match(/filename\*\s*=\s*([^;]+)/i);
    if (m) {
      var starred = m[1].trim();
      var trio = starred.match(/^[^']*'[^']*'(.*)$/);
      var value = trio ? trio[1] : starred;
      try { value = decodeURIComponent(value); } catch (err) { /* raw */ }
      return value || fallback;
    }
    m = cd.match(/filename\s*=\s*("[^"]*"|[^;]+)/i);
    if (m) {
      var plain = m[1].trim();
      if (plain.charAt(0) === "\"") plain = plain.slice(1, -1);
      plain = plain.replace(/\\(.)/g, "$1");
      return plain || fallback;
    }
    return fallback;
  }

  // ------------------------------------------------------------------
  // Boot
  // ------------------------------------------------------------------
  function boot() {
    pickBtn.addEventListener("click", function () { fileInput.click(); });
    if (folderInput && pickFolderBtn &&
        "webkitdirectory" in HTMLInputElement.prototype) {
      pickFolderBtn.hidden = false;
      pickFolderBtn.addEventListener("click", function () { folderInput.click(); });
      folderInput.addEventListener("change", function () {
        var files = Array.prototype.slice.call(folderInput.files || []);
        var roots = files.map(function (f) {
          var path = f.webkitRelativePath || "";
          return path.split("/")[0] || "";
        });
        var root = roots[0];
        if (!root || roots.some(function (candidate) { return candidate !== root; }) ||
            files.some(function (f) {
              return !f.webkitRelativePath ||
                f.webkitRelativePath.split("/").length < 2;
            })) {
          setStatus("error", "The selected folder could not be read safely. Please try again.");
          folderInput.value = "";
          return;
        }
        addFiles(files, root);
        folderInput.value = "";
      });
    }
    fileInput.addEventListener("change", function () {
      addFiles(fileInput.files, "");
      fileInput.value = "";
    });
    refreshBtn.addEventListener("click", refreshFiles);
    downloadSelectedBtn.addEventListener("click", function () {
      var selected = Array.prototype.slice.call(filesEl.querySelectorAll(".source-check:checked"));
      var sources = selected.map(function (input) {
        return { id: input.value, name: input.getAttribute("aria-label") || "selected source" };
      });
      if (sources.length) downloadSources(sources, downloadSelectedBtn);
    });

    if (!token) {
      // No pairing token in this load. No session was ever opened here,
      // so nothing "ended": the status line names the real next step.
      endSession("Scan the QR code",
        "Open this page by scanning the QR code shown on the Flitnix desktop. If you reloaded an old tab, the old link was single-use — rescan to reconnect.",
        "Pairing required");
      return;
    }

    // The token passes the format check; the desktop itself validates it on
    // the first API call (fresh sessions die if they were revoked already).
    setStatus("ready", "Connected — pick files to send or download the shared ones");
    flowEl.hidden = false;
    refreshFiles();
  }

  boot();
})();
