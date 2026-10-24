/* Flitnix phone page — no network requests, no external scripts. */
(function () {
  "use strict";

  var status = document.getElementById("status");
  if (!status) return;
  var text = status.querySelector(".status__text");
  if (!text) return;

  // Getting here means the page itself was downloaded from the Flitnix
  // server, so the phone is connected. Transfer endpoints will be added in
  // a later stage.
  status.dataset.state = "connected";
  text.textContent = "Connected to Flitnix";
})();
