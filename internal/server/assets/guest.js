(function () {
  "use strict";

  var POLL_MS = 1500;
  var inviteSecret = null;
  // Guest authentication rides on the HttpOnly flitnix_guest_token cookie,
  // which the browser attaches automatically to same-origin requests. The
  // long-lived guest token is never handed to JavaScript.
  var guestInfo = null;
  var isPolling = false;
  var uploadQueue = []; // Array of file upload jobs
  var activeUploadIndex = 0;
  var isUploading = false;

  // Extract invite secret from fragment
  var frag = window.location.hash || "";
  if (frag.length > 1) {
    inviteSecret = decodeURIComponent(frag.slice(1));
    // Immediately scrub fragment from address bar
    window.history.replaceState(null, "", window.location.pathname + window.location.search);
  }

  // Handshake ping to verify phone-to-desktop reachability
  fetch("/ping").catch(function () {});

  // DOM elements
  var statusCard = document.getElementById("status-card");
  var statusText = document.getElementById("status-text");
  var rescanView = document.getElementById("rescan-view");
  var joinView = document.getElementById("join-view");
  var joinForm = document.getElementById("join-form");
  var nicknameInput = document.getElementById("nickname-input");
  var joinBtn = document.getElementById("join-btn");
  var waitingView = document.getElementById("waiting-view");
  var waitingMsg = document.getElementById("waiting-msg");
  var rejectedView = document.getElementById("rejected-view");
  var rejectedTitle = document.getElementById("rejected-title");
  var rejectedMsg = document.getElementById("rejected-msg");
  var uploadView = document.getElementById("upload-view");
  var guestBadge = document.getElementById("guest-badge");
  var dropZone = document.getElementById("drop-zone");
  var fileSelector = document.getElementById("file-selector");
  var queueEl = document.getElementById("upload-queue");

  // Event & Collection metadata DOM elements
  var roomHeaderCard = document.getElementById("room-header-card");
  var roomHeaderTitle = document.getElementById("room-header-title");
  var roomHeaderDesc = document.getElementById("room-header-desc");
  var roomDeadlineBadge = document.getElementById("room-deadline-badge");
  var rosterSelectGroup = document.getElementById("roster-select-group");
  var participantSelect = document.getElementById("participant-select");
  var submissionFormCard = document.getElementById("submission-form-card");
  var dynamicFormFields = document.getElementById("dynamic-form-fields");
  var roomInfo = null;

  // ------------------------------------------------------------------
  // Screen Wake Lock & Background Awareness
  // ------------------------------------------------------------------
  var awarenessBanner = document.getElementById("awareness-banner");
  var awarenessBannerText = document.getElementById("awareness-banner-text");
  var awarenessBannerClose = document.getElementById("awareness-banner-close");
  var wakeLockBadge = document.getElementById("wake-lock-badge");
  var wakeLockContainer = document.getElementById("wake-lock-container");
  var bannerTimer = null;

  var isWakeLockSupported = !!(window.isSecureContext && navigator.wakeLock && typeof navigator.wakeLock.request === "function");
  var userWakeLockRequested = false;
  var wakeLockSentinel = null;
  var wakeLockAcquiring = false;
  var wasBackgroundedDuringTransfer = false;

  function showAwarenessBanner(msg) {
    if (!awarenessBanner || !awarenessBannerText) return;
    if (bannerTimer) { clearTimeout(bannerTimer); bannerTimer = null; }
    awarenessBannerText.textContent = msg;
    awarenessBanner.hidden = false;
    bannerTimer = setTimeout(function () {
      if (awarenessBanner) awarenessBanner.hidden = true;
      bannerTimer = null;
    }, 7000);
  }

  function hideAwarenessBanner() {
    if (bannerTimer) { clearTimeout(bannerTimer); bannerTimer = null; }
    if (awarenessBanner) awarenessBanner.hidden = true;
  }

  if (awarenessBannerClose) {
    awarenessBannerClose.addEventListener("click", hideAwarenessBanner);
  }

  function hasActiveTransfers() {
    if (isUploading) return true;
    for (var i = 0; i < uploadQueue.length; i++) {
      var s = uploadQueue[i].state;
      if (s === "queued" || s === "declaring" || s === "pending" || s === "approved" || s === "uploading") {
        return true;
      }
    }
    return false;
  }

  function updateAwarenessUI() {
    if (!wakeLockBadge || !wakeLockContainer) return;

    if (!isWakeLockSupported) {
      wakeLockBadge.textContent = "Best-effort";
      wakeLockBadge.className = "badge badge--waiting";
      wakeLockContainer.innerHTML = '<p class="wake-lock-unsupported"><span aria-hidden="true">🔒</span> Screen wake lock requires HTTPS or localhost and is unavailable on this local HTTP connection. Keep this tab open and visible during transfers.</p>';
      return;
    }

    var cb = document.getElementById("wake-lock-cb");
    if (!cb) {
      wakeLockContainer.innerHTML = [
        '<label class="wake-lock-label" for="wake-lock-cb">',
        '  <input type="checkbox" id="wake-lock-cb">',
        '  <span>Keep screen awake during transfer</span>',
        '</label>',
        '<p class="wake-lock-note">Prevents display timeout while transfers are active. Uses more battery. Transfers may still pause if the browser is backgrounded.</p>'
      ].join("");
      cb = document.getElementById("wake-lock-cb");
      if (cb) {
        cb.checked = userWakeLockRequested;
        cb.addEventListener("change", function () {
          userWakeLockRequested = !!cb.checked;
          syncWakeLock();
        });
      }
    } else {
      cb.checked = userWakeLockRequested;
    }

    var active = wakeLockSentinel !== null && !wakeLockSentinel.released;
    var transfersActive = hasActiveTransfers();

    if (active) {
      wakeLockBadge.textContent = "Screen wake active";
      wakeLockBadge.className = "badge badge--approved";
    } else if (userWakeLockRequested) {
      if (wakeLockAcquiring) {
        wakeLockBadge.textContent = "Acquiring wake lock…";
        wakeLockBadge.className = "badge badge--uploading";
      } else {
        wakeLockBadge.textContent = "Screen wake standby";
        wakeLockBadge.className = "badge badge--waiting";
      }
    } else {
      if (transfersActive) {
        wakeLockBadge.textContent = "Active transfer";
        wakeLockBadge.className = "badge badge--uploading";
      } else {
        wakeLockBadge.textContent = "Idle";
        wakeLockBadge.className = "badge badge--waiting";
      }
    }
  }

  function acquireWakeLock() {
    if (!isWakeLockSupported || !userWakeLockRequested) return;
    if (wakeLockSentinel !== null || wakeLockAcquiring) return;
    if (document.visibilityState !== "visible") return;
    if (!hasActiveTransfers()) return;

    wakeLockAcquiring = true;
    updateAwarenessUI();

    navigator.wakeLock.request("screen").then(function (sentinel) {
      wakeLockAcquiring = false;
      wakeLockSentinel = sentinel;
      sentinel.addEventListener("release", function () {
        wakeLockSentinel = null;
        updateAwarenessUI();
      });
      updateAwarenessUI();
    }).catch(function (err) {
      wakeLockAcquiring = false;
      wakeLockSentinel = null;
      if (window.console && typeof window.console.warn === "function") {
        window.console.warn("Screen wake lock request failed:", err);
      }
      updateAwarenessUI();
    });
  }

  function releaseWakeLock() {
    if (wakeLockSentinel !== null) {
      var s = wakeLockSentinel;
      wakeLockSentinel = null;
      try { s.release(); } catch (err) {}
    }
    updateAwarenessUI();
  }

  function syncWakeLock() {
    if (!isWakeLockSupported) {
      updateAwarenessUI();
      return;
    }
    if (userWakeLockRequested && hasActiveTransfers() && document.visibilityState === "visible") {
      acquireWakeLock();
    } else {
      if (wakeLockSentinel !== null) {
        releaseWakeLock();
      } else {
        updateAwarenessUI();
      }
    }
  }

  function reconcileActiveTransfers() {
    for (var i = 0; i < uploadQueue.length; i++) {
      var job = uploadQueue[i];
      if (job.uploadId && (job.state === "pending" || job.state === "uploading")) {
        (function (j) {
          fetch("/guest-api/uploads/" + encodeURIComponent(j.uploadId), {
          }).then(function (res) { return res.json(); }).then(function (data) {
            if (data.state === "completed" && j.state !== "completed") {
              j.state = "completed";
              j.transferred = j.size;
              updateJobUI(j);
              isUploading = false;
              syncWakeLock();
              pumpQueue();
            } else if (data.state === "rejected" || data.state === "failed") {
              j.state = data.state;
              if (data.error) j.error = data.error;
              updateJobUI(j);
              isUploading = false;
              syncWakeLock();
              pumpQueue();
            }
          }).catch(function () {});
        })(job);
      }
    }
  }

  function onVisibilityChange() {
    if (document.visibilityState === "hidden") {
      if (hasActiveTransfers()) {
        wasBackgroundedDuringTransfer = true;
        if (window.console && typeof window.console.info === "function") {
          window.console.info("Flitnix Guest: page moved to background during transfer");
        }
      }
    } else if (document.visibilityState === "visible") {
      if (wasBackgroundedDuringTransfer) {
        wasBackgroundedDuringTransfer = false;
        showAwarenessBanner("Flitnix was moved to the background. Transfer status has been refreshed.");
        reconcileActiveTransfers();
      }
      syncWakeLock();
    }
    updateAwarenessUI();
  }

  document.addEventListener("visibilitychange", onVisibilityChange);

  function setStatus(state, msg) {
    if (statusCard) statusCard.dataset.state = state;
    if (statusText) statusText.textContent = msg;
  }

  function hideAllViews() {
    if (rescanView) rescanView.hidden = true;
    if (joinView) joinView.hidden = true;
    if (waitingView) waitingView.hidden = true;
    if (rejectedView) rejectedView.hidden = true;
    if (uploadView) uploadView.hidden = true;
    releaseWakeLock();
  }

  function formatEta(seconds) {
    if (typeof seconds !== "number" || !isFinite(seconds) || seconds <= 0) return "< 1s";
    if (seconds < 60) return seconds + "s";
    var m = Math.floor(seconds / 60);
    var s = seconds % 60;
    return m + "m " + (s < 10 ? "0" : "") + s + "s";
  }

  function formatBytes(bytes) {
    if (typeof bytes !== "number" || isNaN(bytes) || bytes <= 0) return "0 B";
    var units = ["B", "KB", "MB", "GB", "TB"];
    var idx = 0;
    var val = bytes;
    while (val >= 1000 && idx < units.length - 1) {
      val /= 1000;
      idx++;
    }
    return (idx === 0 ? val.toFixed(0) : val.toFixed(1)) + " " + units[idx];
  }

  function renderFormFields(schema) {
    if (!dynamicFormFields) return;
    dynamicFormFields.innerHTML = "";
    if (!schema || schema.length === 0) {
      if (submissionFormCard) submissionFormCard.hidden = true;
      return;
    }
    if (submissionFormCard) submissionFormCard.hidden = false;

    for (var i = 0; i < schema.length; i++) {
      var field = schema[i];
      var group = document.createElement("div");
      group.className = "input-group";

      var label = document.createElement("label");
      label.className = "input-label";
      label.htmlFor = "field-" + field.id;
      label.textContent = field.label + (field.required ? " *" : "");
      group.appendChild(label);

      var inputEl;
      if (field.type === "textarea") {
        inputEl = document.createElement("textarea");
        inputEl.rows = 3;
      } else if (field.type === "select" && field.options && field.options.length > 0) {
        inputEl = document.createElement("select");
        var defOpt = document.createElement("option");
        defOpt.value = "";
        defOpt.textContent = "-- Select an option --";
        inputEl.appendChild(defOpt);
        for (var o = 0; o < field.options.length; o++) {
          var opt = document.createElement("option");
          opt.value = field.options[o];
          opt.textContent = field.options[o];
          inputEl.appendChild(opt);
        }
      } else {
        inputEl = document.createElement("input");
        inputEl.type = "text";
      }

      inputEl.id = "field-" + field.id;
      inputEl.className = "text-input";
      if (field.maxLength) inputEl.maxLength = field.maxLength;
      if (field.required) inputEl.required = true;
      group.appendChild(inputEl);
      dynamicFormFields.appendChild(group);
    }
  }

  function fetchRoomInfo() {
    if (!inviteSecret) return;
    fetch("/guest-api/room-info?secret=" + encodeURIComponent(inviteSecret))
      .then(function (res) {
        if (!res.ok) return null;
        return res.json();
      })
      .then(function (info) {
        if (!info) return;
        roomInfo = info;

        if (roomHeaderCard) {
          if (info.title) {
            roomHeaderCard.hidden = false;
            if (roomHeaderTitle) roomHeaderTitle.textContent = info.title;
            if (roomHeaderDesc) {
              if (info.description) {
                roomHeaderDesc.textContent = info.description;
                roomHeaderDesc.hidden = false;
              } else {
                roomHeaderDesc.hidden = true;
              }
            }
          }
        }

        if (info.deadline && roomDeadlineBadge) {
          var dl = new Date(info.deadline);
          var now = new Date();
          var diffMs = dl - now;
          if (diffMs > 0) {
            var diffMins = Math.round(diffMs / 60000);
            roomDeadlineBadge.textContent = "Deadline: " + dl.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) + " (" + diffMins + "m remaining)";
            roomDeadlineBadge.className = "badge badge--pending";
          } else {
            roomDeadlineBadge.textContent = "Deadline: Passed (" + dl.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) + ")";
            roomDeadlineBadge.className = "badge badge--failed";
          }
          roomDeadlineBadge.hidden = false;
        }

        if (info.hasRoster && info.roster && info.roster.length > 0 && participantSelect && rosterSelectGroup) {
          rosterSelectGroup.hidden = false;
          while (participantSelect.options.length > 1) {
            participantSelect.remove(1);
          }
          for (var pIdx = 0; pIdx < info.roster.length; pIdx++) {
            var part = info.roster[pIdx];
            var pOpt = document.createElement("option");
            pOpt.value = part.id;
            pOpt.textContent = part.name + (part.identifier ? " (" + part.identifier + ")" : "");
            participantSelect.appendChild(pOpt);
          }
          participantSelect.addEventListener("change", function () {
            var selectedIdx = participantSelect.selectedIndex;
            if (selectedIdx > 0 && nicknameInput && !nicknameInput.value.trim()) {
              var pName = info.roster[selectedIdx - 1].name;
              nicknameInput.value = pName;
            }
          });
        }

        if (info.formSchema) {
          renderFormFields(info.formSchema);
        }
      })
      .catch(function () {
        // Non-fatal if room-info fails
      });
  }

  // Initial routing
  if (!inviteSecret) {
    // Check if we already have an active guest cookie
    checkExistingSession();
  } else {
    fetchRoomInfo();
    showJoinForm();
  }
  updateAwarenessUI();

  function showJoinForm() {
    hideAllViews();
    if (joinView) joinView.hidden = false;
    setStatus("checking", "Enter your name to join");
  }

  function checkExistingSession() {
    fetch("/guest-api/status")
      .then(function (res) {
        if (res.ok) return res.json();
        throw new Error("unauthorized");
      })
      .then(function (data) {
        handleStatusResponse(data);
      })
      .catch(function () {
        hideAllViews();
        if (rescanView) rescanView.hidden = false;
        setStatus("ended", "Scan Guest QR code to join");
      });
  }

  if (joinForm) {
    joinForm.addEventListener("submit", function (e) {
      e.preventDefault();
      if (!inviteSecret) return;

      var nickname = nicknameInput ? nicknameInput.value.trim() : "";
      var participantId = participantSelect ? participantSelect.value : "";
      if (joinBtn) joinBtn.disabled = true;
      setStatus("checking", "Sending join request…");

      var reqBody = { secret: inviteSecret, nickname: nickname };
      if (participantId) {
        reqBody.participantId = participantId;
      }

      fetch("/guest-api/join", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(reqBody)
      })
        .then(function (res) {
          if (!res.ok) throw new Error("Join request failed (" + res.status + ")");
          return res.json();
        })
        .then(function (data) {
          // The join response sets the HttpOnly guest cookie; nothing about
          // the credential is returned to this script.
          guestInfo = data;
          hideAllViews();
          if (waitingView) waitingView.hidden = false;
          setStatus("checking", "Waiting for host approval…");
          startStatusPolling();
        })
        .catch(function (err) {
          if (joinBtn) joinBtn.disabled = false;
          setStatus("ended", err.message || "Failed to join");
          alert("Could not join: " + (err.message || "Invalid or expired invitation."));
        });
    });
  }

  function startStatusPolling() {
    if (isPolling) return;
    isPolling = true;

    function poll() {
      fetch("/guest-api/status", {
      })
        .then(function (res) { return res.json(); })
        .then(function (data) {
          handleStatusResponse(data);
          if (data.state === "pending" || data.state === "approved") {
            setTimeout(poll, POLL_MS);
          } else {
            isPolling = false;
          }
        })
        .catch(function () {
          setTimeout(poll, POLL_MS * 2);
        });
    }

    poll();
  }

  function handleStatusResponse(data) {
    if (data.state === "approved") {
      hideAllViews();
      if (uploadView) uploadView.hidden = false;
      updateAwarenessUI();
      if (guestBadge) guestBadge.textContent = "Connected: " + (data.nickname || "Guest");
      setStatus("ready", "Connected to Guest Drop Box");
      pumpQueue();
    } else if (data.state === "pending") {
      hideAllViews();
      if (waitingView) waitingView.hidden = false;
      setStatus("checking", "Waiting for host approval…");
    } else if (data.state === "rejected") {
      hideAllViews();
      if (rejectedView) rejectedView.hidden = false;
      if (rejectedTitle) rejectedTitle.textContent = "Request Rejected";
      if (rejectedMsg) rejectedMsg.textContent = "The desktop owner rejected your join request.";
      setStatus("ended", "Join request rejected");
    } else if (data.state === "revoked") {
      hideAllViews();
      if (rejectedView) rejectedView.hidden = false;
      if (rejectedTitle) rejectedTitle.textContent = "Access Revoked";
      if (rejectedMsg) rejectedMsg.textContent = "The desktop owner revoked access for this device.";
      setStatus("ended", "Access revoked");
    } else if (data.state === "ended" || data.state === "expired") {
      hideAllViews();
      if (rejectedView) rejectedView.hidden = false;
      if (rejectedTitle) rejectedTitle.textContent = "Drop Box Ended";
      if (rejectedMsg) rejectedMsg.textContent = "This Guest Drop Box has closed or expired.";
      setStatus("ended", "Drop Box session ended");
    }
  }

  // File picking and drag & drop
  if (dropZone && fileSelector) {
    dropZone.addEventListener("click", function () {
      fileSelector.click();
    });

    dropZone.addEventListener("keydown", function (e) {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        if (e.repeat) return;
        fileSelector.click();
      }
    });

    dropZone.addEventListener("dragover", function (e) {
      e.preventDefault();
      dropZone.classList.add("drop-zone--dragover");
    });

    dropZone.addEventListener("dragleave", function () {
      dropZone.classList.remove("drop-zone--dragover");
    });

    dropZone.addEventListener("drop", function (e) {
      e.preventDefault();
      dropZone.classList.remove("drop-zone--dragover");
      if (e.dataTransfer && e.dataTransfer.files) {
        enqueueFiles(e.dataTransfer.files);
      }
    });

    fileSelector.addEventListener("change", function () {
      if (fileSelector.files) {
        enqueueFiles(fileSelector.files);
      }
    });
  }

  function getFormValues() {
    var values = {};
    if (!roomInfo || !roomInfo.formSchema) return values;
    for (var i = 0; i < roomInfo.formSchema.length; i++) {
      var field = roomInfo.formSchema[i];
      var input = document.getElementById("field-" + field.id);
      if (input) {
        values[field.id] = input.value.trim();
      }
    }
    return values;
  }

  function validateFormSchema() {
    if (!roomInfo || !roomInfo.formSchema) return true;
    for (var i = 0; i < roomInfo.formSchema.length; i++) {
      var field = roomInfo.formSchema[i];
      if (field.required) {
        var input = document.getElementById("field-" + field.id);
        if (!input || !input.value.trim()) {
          alert("Please fill out the required field: " + field.label);
          if (input) input.focus();
          return false;
        }
      }
    }
    return true;
  }

  function enqueueFiles(fileList) {
    if (!validateFormSchema()) return;
    var currentValues = getFormValues();

    for (var i = 0; i < fileList.length; i++) {
      var f = fileList[i];
      var job = {
        file: f,
        name: f.name,
        size: f.size,
        formValues: currentValues,
        state: "queued", // queued, declaring, pending, approved, uploading, completed, rejected, failed
        uploadId: null,
        transferred: 0,
        speed: 0,
        error: null,
        el: null
      };
      uploadQueue.push(job);
      renderJob(job);
    }
    syncWakeLock();
    pumpQueue();
  }

  function renderJob(job) {
    var li = document.createElement("li");
    li.className = "queue-item";

    var top = document.createElement("div");
    top.style.display = "flex";
    top.style.justifyContent = "space-between";
    top.style.alignItems = "center";

    var nameSpan = document.createElement("span");
    nameSpan.style.fontWeight = "500";
    nameSpan.style.wordBreak = "break-all";
    nameSpan.textContent = job.name;

    var badgeSpan = document.createElement("span");
    badgeSpan.className = "badge badge--pending";
    badgeSpan.textContent = "Queued";

    top.appendChild(nameSpan);
    top.appendChild(badgeSpan);
    li.appendChild(top);

    var barBg = document.createElement("div");
    barBg.className = "progress-bar-bg";
    barBg.setAttribute("role", "progressbar");
    barBg.setAttribute("aria-label", "Upload progress for " + (job.file ? job.file.name : "file"));
    barBg.setAttribute("aria-valuemin", "0");
    barBg.setAttribute("aria-valuemax", "100");
    barBg.setAttribute("aria-valuenow", "0");
    var barFill = document.createElement("div");
    barFill.className = "progress-bar-fill";
    barBg.appendChild(barFill);
    li.appendChild(barBg);

    var meta = document.createElement("div");
    meta.className = "meta-row";
    var sizeSpan = document.createElement("span");
    sizeSpan.textContent = formatBytes(job.size);
    var detailSpan = document.createElement("span");
    detailSpan.textContent = "Waiting…";
    meta.appendChild(sizeSpan);
    meta.appendChild(detailSpan);
    li.appendChild(meta);

    var retryBtn = document.createElement("button");
    retryBtn.type = "button";
    retryBtn.textContent = "Retry Upload";
    retryBtn.className = "badge badge--failed";
    retryBtn.style.marginTop = "0.4rem";
    retryBtn.style.cursor = "pointer";
    retryBtn.style.border = "1px solid var(--danger)";
    retryBtn.style.background = "rgba(248, 113, 113, 0.15)";
    retryBtn.hidden = true;
    retryBtn.addEventListener("click", function () {
      retryBtn.hidden = true;
      job.state = "queued";
      job.uploadId = null;
      job.transferred = 0;
      job.speed = 0;
      job.error = null;
      updateJobUI(job);
      pumpQueue();
    });
    li.appendChild(retryBtn);

    job.el = {
      root: li,
      badge: badgeSpan,
      bar: barBg,
      fill: barFill,
      detail: detailSpan,
      retryBtn: retryBtn
    };

    if (queueEl) queueEl.appendChild(li);
  }

  function updateJobUI(job) {
    if (!job.el) return;
    var b = job.el.badge;
    var fill = job.el.fill;
    var detail = job.el.detail;

    switch (job.state) {
      case "declaring":
      case "pending":
        b.className = "badge badge--pending";
        b.textContent = "Awaiting Host Approval";
        detail.textContent = "Waiting for host to accept file…";
        fill.style.width = "0%";
        if (job.el.bar) job.el.bar.setAttribute("aria-valuenow", "0");
        break;
      case "approved":
        b.className = "badge badge--approved";
        b.textContent = "Approved";
        detail.textContent = "Starting stream…";
        break;
      case "uploading":
        b.className = "badge badge--uploading";
        var pct = job.size > 0 ? Math.min(100, (job.transferred / job.size) * 100) : 0;
        b.textContent = "Uploading " + pct.toFixed(0) + "%";
        fill.style.width = pct + "%";
        if (job.el.bar) {
          var step = Math.floor(pct / 20) * 20;
          if (job.el.bar.getAttribute("aria-valuenow") !== String(step)) {
            job.el.bar.setAttribute("aria-valuenow", String(step));
          }
        }
        var speedText = "";
        if (job.speed > 0) {
          speedText = " (" + formatBytes(job.speed) + "/s";
          if (job.size > job.transferred) {
            var rem = Math.round((job.size - job.transferred) / job.speed);
            speedText += " · ETA " + formatEta(rem);
          }
          speedText += ")";
        }
        detail.textContent = formatBytes(job.transferred) + " / " + formatBytes(job.size) + speedText;
        if (job.el.retryBtn) job.el.retryBtn.hidden = true;
        break;
      case "completed":
        b.className = "badge badge--completed";
        b.textContent = "Completed";
        fill.style.width = "100%";
        fill.style.background = "var(--accent-teal)";
        detail.textContent = "Saved to host folder";
        if (job.el.bar) job.el.bar.setAttribute("aria-valuenow", "100");
        if (job.el.retryBtn) job.el.retryBtn.hidden = true;
        break;
      case "rejected":
        b.className = "badge badge--rejected";
        b.textContent = "Host Rejected";
        detail.textContent = "Host declined to receive this file";
        if (job.el.retryBtn) job.el.retryBtn.hidden = true;
        break;
      case "failed":
        b.className = "badge badge--failed";
        b.textContent = "Failed";
        detail.textContent = job.error || "Upload failed";
        if (job.el.retryBtn) job.el.retryBtn.hidden = false;
        break;
    }
  }

  function pumpQueue() {
    syncWakeLock();
    if (isUploading) return;
    var nextJob = null;
    for (var i = 0; i < uploadQueue.length; i++) {
      if (uploadQueue[i].state === "queued") {
        nextJob = uploadQueue[i];
        break;
      }
    }
    if (!nextJob) return;

    isUploading = true;
    processJob(nextJob);
  }

  function processJob(job) {
    job.state = "declaring";
    updateJobUI(job);
    syncWakeLock();

    var payload = { name: job.name, size: job.size };
    if (job.formValues && Object.keys(job.formValues).length > 0) {
      payload.formValues = job.formValues;
    }

    fetch("/guest-api/uploads", {
      method: "POST",
      headers: {
        "Content-Type": "application/json"
      },
      body: JSON.stringify(payload)
    })
      .then(function (res) {
        if (res.status === 409) {
          throw new Error("File already exists. In this room, duplicate file names are rejected.");
        }
        if (!res.ok) throw new Error("Upload declaration rejected (" + res.status + ")");
        return res.json();
      })
      .then(function (data) {
        job.uploadId = data.id;
        job.state = "pending";
        updateJobUI(job);
        pollUploadApproval(job);
      })
      .catch(function (err) {
        job.state = "failed";
        job.error = err.message || "Declaration failed";
        updateJobUI(job);
        isUploading = false;
        syncWakeLock();
        pumpQueue();
      });
  }

  function pollUploadApproval(job) {
    function check() {
      fetch("/guest-api/uploads/" + encodeURIComponent(job.uploadId), {
      })
        .then(function (res) { return res.json(); })
        .then(function (data) {
          if (data.state === "approved") {
            job.state = "approved";
            updateJobUI(job);
            streamJobContent(job);
          } else if (data.state === "rejected") {
            job.state = "rejected";
            updateJobUI(job);
            isUploading = false;
            syncWakeLock();
            pumpQueue();
          } else if (data.state === "pending") {
            setTimeout(check, 1000);
          } else {
            job.state = data.state || "failed";
            updateJobUI(job);
            isUploading = false;
            syncWakeLock();
            pumpQueue();
          }
        })
        .catch(function () {
          setTimeout(check, 2000);
        });
    }

    check();
  }

  function streamJobContent(job) {
    job.state = "uploading";
    updateJobUI(job);
    syncWakeLock();

    var lastLoaded = 0;
    var lastTime = Date.now();
    var currentSpeed = 0;
    var lastPaint = 0;

    var stream = FlitnixUpload.stream({
      url: "/guest-api/uploads/" + encodeURIComponent(job.uploadId) + "/content",
      file: job.file,
      onProgress: function (loaded) {
        job.transferred = loaded;
        var now = Date.now();
        var dt = (now - lastTime) / 1000;
        if (dt >= 0.3) {
          var instantSpeed = (loaded - lastLoaded) / dt;
          currentSpeed = currentSpeed === 0 ? instantSpeed : (0.7 * currentSpeed + 0.3 * instantSpeed);
          job.speed = currentSpeed;
          lastLoaded = loaded;
          lastTime = now;
        }
        // Progress events arrive far faster than the screen refreshes;
        // repainting on every one of them starves the transfer itself.
        if (now - lastPaint >= 200 || loaded >= job.size) {
          lastPaint = now;
          updateJobUI(job);
          updateAwarenessUI();
        }
      }
    });

    stream.promise.then(function (res) {
      if (res.status >= 200 && res.status < 300) {
        job.state = "completed";
        job.transferred = job.size;
        updateJobUI(job);
      } else {
        job.state = "failed";
        job.error = "Upload failed (" + res.status + ")";
        updateJobUI(job);
      }
      isUploading = false;
      syncWakeLock();
      pumpQueue();
    }, function () {
      job.state = "failed";
      job.error = "Network error during upload";
      updateJobUI(job);
      isUploading = false;
      syncWakeLock();
      pumpQueue();
    });
  }

})();
