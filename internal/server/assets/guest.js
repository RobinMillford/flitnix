(function () {
  "use strict";

  var POLL_MS = 1500;
  var inviteSecret = null;
  var guestToken = null;
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

  function showJoinForm() {
    hideAllViews();
    if (joinView) joinView.hidden = false;
    setStatus("checking", "Enter your name to join");
  }

  function checkExistingSession() {
    fetch("/guest-api/status", {
      headers: guestToken ? { "Authorization": "Bearer " + guestToken } : {}
    })
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
          guestToken = data.token;
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
        headers: guestToken ? { "Authorization": "Bearer " + guestToken } : {}
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

    var payload = { name: job.name, size: job.size };
    if (job.formValues && Object.keys(job.formValues).length > 0) {
      payload.formValues = job.formValues;
    }

    fetch("/guest-api/uploads", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Authorization": guestToken ? "Bearer " + guestToken : ""
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
        pumpQueue();
      });
  }

  function pollUploadApproval(job) {
    function check() {
      fetch("/guest-api/uploads/" + encodeURIComponent(job.uploadId), {
        headers: guestToken ? { "Authorization": "Bearer " + guestToken } : {}
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
            pumpQueue();
          } else if (data.state === "pending") {
            setTimeout(check, 1000);
          } else {
            job.state = data.state || "failed";
            updateJobUI(job);
            isUploading = false;
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

    var xhr = new XMLHttpRequest();
    var lastLoaded = 0;
    var lastTime = Date.now();
    var currentSpeed = 0;

    xhr.upload.onprogress = function (e) {
      if (e.lengthComputable) {
        job.transferred = e.loaded;
        var now = Date.now();
        var dt = (now - lastTime) / 1000;
        if (dt >= 0.3) {
          var instantSpeed = (e.loaded - lastLoaded) / dt;
          currentSpeed = currentSpeed === 0 ? instantSpeed : (0.7 * currentSpeed + 0.3 * instantSpeed);
          job.speed = currentSpeed;
          lastLoaded = e.loaded;
          lastTime = now;
        }
        updateJobUI(job);
      }
    };

    xhr.onload = function () {
      if (xhr.status >= 200 && xhr.status < 300) {
        job.state = "completed";
        job.transferred = job.size;
        updateJobUI(job);
      } else {
        job.state = "failed";
        job.error = "Upload failed (" + xhr.status + ")";
        updateJobUI(job);
      }
      isUploading = false;
      pumpQueue();
    };

    xhr.onerror = function () {
      job.state = "failed";
      job.error = "Network error during upload";
      updateJobUI(job);
      isUploading = false;
      pumpQueue();
    };

    xhr.open("PUT", "/guest-api/uploads/" + encodeURIComponent(job.uploadId) + "/content");
    if (guestToken) {
      xhr.setRequestHeader("Authorization", "Bearer " + guestToken);
    }
    xhr.send(job.file);
  }

})();
