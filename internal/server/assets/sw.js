/* Flitnix Service Worker (G12)
 *
 * Security & Cache Isolation Invariants:
 * - Caches ONLY public static app-shell assets (HTML/CSS/JS/icons/manifest).
 * - NEVER caches /api/*, /guest-api/*, /guest, /local-ca.crt, /ping, /healthz,
 *   Bearer-authenticated requests, or transfer payloads.
 * - Intercepts Web Share Target POST /share-target locally via IndexedDB File
 *   references so large files are not buffered in memory and require explicit
 *   user confirmation inside the app before uploading.
 */
(function () {
  "use strict";

  var CACHE_NAME = "flitnix-shell-v1";
  var SHARE_DB_NAME = "flitnix-share-target";
  var SHARE_STORE_NAME = "pending";
  var PRECACHE_URLS = [
    "/",
    "/offline.html",
    "/manifest.webmanifest",
    "/static/styles.css",
    "/static/app.js",
    "/static/upload-client.js",
    "/static/upload-worker.js",
    "/static/device-crypto.js",
    "/static/nacl-fast.min.js",
    "/static/sha256.min.js",
    "/static/favicon.png",
    "/static/apple-touch-icon.png",
    "/static/icon-192.png",
    "/static/icon-512.png",
    "/static/logo-mark.png"
  ];

  function isCacheableStaticPath(pathname) {
    if (pathname === "/" || pathname === "/offline.html" || pathname === "/manifest.webmanifest") {
      return true;
    }
    if (pathname.indexOf("/static/") === 0) {
      return true;
    }
    return false;
  }

  function openShareDB() {
    return new Promise(function (resolve, reject) {
      var req = indexedDB.open(SHARE_DB_NAME, 1);
      req.onupgradeneeded = function () {
        var db = req.result;
        if (!db.objectStoreNames.contains(SHARE_STORE_NAME)) {
          db.createObjectStore(SHARE_STORE_NAME, { keyPath: "id" });
        }
      };
      req.onsuccess = function () { resolve(req.result); };
      req.onerror = function () { reject(req.error || new Error("IndexedDB unavailable")); };
    });
  }

  function storePendingShare(record) {
    return openShareDB().then(function (db) {
      return new Promise(function (resolve, reject) {
        var tx = db.transaction(SHARE_STORE_NAME, "readwrite");
        var store = tx.objectStore(SHARE_STORE_NAME);
        // Keep only the latest pending share to avoid unbounded storage accumulation.
        store.clear();
        store.put(record);
        tx.oncomplete = function () {
          db.close();
          resolve();
        };
        tx.onerror = function () {
          db.close();
          reject(tx.error || new Error("Failed to store shared data"));
        };
      });
    });
  }

  function makeShareId() {
    if (self.crypto && typeof self.crypto.randomUUID === "function") {
      return self.crypto.randomUUID();
    }
    return "share-" + Date.now() + "-" + Math.random().toString(36).slice(2, 10);
  }

  self.addEventListener("install", function (event) {
    event.waitUntil(
      caches.open(CACHE_NAME).then(function (cache) {
        return cache.addAll(PRECACHE_URLS);
      }).then(function () {
        return self.skipWaiting();
      })
    );
  });

  self.addEventListener("activate", function (event) {
    event.waitUntil(
      caches.keys().then(function (keys) {
        return Promise.all(keys.map(function (key) {
          if (key.indexOf("flitnix-shell-") === 0 && key !== CACHE_NAME) {
            return caches.delete(key);
          }
          return Promise.resolve();
        }));
      }).then(function () {
        return self.clients.claim();
      })
    );
  });

  async function handleShareTargetPost(request) {
    try {
      var formData = await request.formData();
      var rawFiles = formData.getAll("files");
      var files = [];
      for (var i = 0; i < rawFiles.length; i++) {
        var entry = rawFiles[i];
        if (entry && typeof entry.size === "number" && typeof entry.name === "string" && typeof entry.slice === "function") {
          files.push(entry);
        }
      }
      var title = typeof formData.get("title") === "string" ? formData.get("title") : "";
      var text = typeof formData.get("text") === "string" ? formData.get("text") : "";
      var urlVal = typeof formData.get("url") === "string" ? formData.get("url") : "";

      var id = makeShareId();
      await storePendingShare({
        id: id,
        title: title.slice(0, 1024),
        text: text.slice(0, 65536),
        url: urlVal.slice(0, 8192),
        files: files,
        createdAt: Date.now()
      });

      return Response.redirect("/?shared=" + encodeURIComponent(id), 303);
    } catch (_) {
      return Response.redirect("/?share_error=1", 303);
    }
  }

  self.addEventListener("fetch", function (event) {
    var request = event.request;
    var reqURL;
    try {
      reqURL = new URL(request.url);
    } catch (_) {
      return;
    }

    // Only handle same-origin requests.
    if (reqURL.origin !== self.location.origin) {
      return;
    }

    // Web Share Target POST interception.
    if (request.method === "POST" && reqURL.pathname === "/share-target") {
      event.respondWith(handleShareTargetPost(request));
      return;
    }

    // Never intercept non-GET requests (uploads, batches, auth, etc.) or sensitive API/guest/CA paths.
    if (request.method !== "GET") {
      return;
    }
    if (request.headers.has("Authorization") || request.headers.has("Cookie")) {
      return;
    }
    if (reqURL.pathname.indexOf("/api/") === 0 ||
        reqURL.pathname.indexOf("/guest-api/") === 0 ||
        reqURL.pathname === "/guest" ||
        reqURL.pathname === "/sw.js" ||
        reqURL.pathname === "/local-ca.crt" ||
        reqURL.pathname === "/ping" ||
        reqURL.pathname === "/healthz") {
      return;
    }

    // Navigation requests: network-first, falling back to cached shell / offline page.
    if (request.mode === "navigate") {
      event.respondWith(
        fetch(request).then(function (resp) {
          if (resp && resp.ok && reqURL.pathname === "/") {
            var clone = resp.clone();
            caches.open(CACHE_NAME).then(function (cache) {
              cache.put("/", clone);
            }).catch(function () {});
          }
          return resp;
        }).catch(function () {
          return caches.match("/").then(function (cachedHome) {
            return cachedHome || caches.match("/offline.html");
          });
        })
      );
      return;
    }

    if (!isCacheableStaticPath(reqURL.pathname)) {
      return;
    }

    // Static app-shell assets: cache-first with network fallback.
    event.respondWith(
      caches.match(request).then(function (cached) {
        if (cached) {
          return cached;
        }
        return fetch(request).then(function (resp) {
          if (resp && resp.ok && resp.type === "basic") {
            var copy = resp.clone();
            caches.open(CACHE_NAME).then(function (cache) {
              cache.put(request, copy);
            }).catch(function () {});
          }
          return resp;
        });
      })
    );
  });
})();
