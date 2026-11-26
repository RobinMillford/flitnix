(function (root) {
  "use strict";

  var TRANSCRIPT_TAG = "flitnix-sas-v1\0";
  var EXPAND_INFO_TAG = "flitnix-sas-expand-v1\0";
  var HOST_PROOF_TAG = "flitnix-host-proof-v1\0";
  var CLIENT_PROOF_TAG = "flitnix-client-proof-v1\0";
  var REJECTION_THRESHOLD = 4294000000;

  function getNacl() {
    if (root && root.nacl) return root.nacl;
    if (typeof nacl !== "undefined") return nacl;
    throw new Error("nacl library not loaded");
  }

  function getSha256() {
    if (root && root.sha256) return root.sha256;
    if (typeof sha256 !== "undefined") return sha256;
    throw new Error("sha256 library not loaded");
  }

  function utf8ToBytes(str) {
    if (typeof TextEncoder !== "undefined") {
      return new TextEncoder().encode(str);
    }
    var utf8 = unescape(encodeURIComponent(str));
    var out = new Uint8Array(utf8.length);
    for (var i = 0; i < utf8.length; i++) {
      out[i] = utf8.charCodeAt(i);
    }
    return out;
  }

  function bytesToHex(bytes) {
    var out = "";
    for (var i = 0; i < bytes.length; i++) {
      var h = (bytes[i] & 0xff).toString(16);
      out += h.length === 1 ? "0" + h : h;
    }
    return out;
  }

  function hexToBytes(hex) {
    if (typeof hex !== "string" || hex.length % 2 !== 0 || !/^[0-9a-fA-F]*$/.test(hex)) {
      throw new Error("invalid hex string");
    }
    var out = new Uint8Array(hex.length / 2);
    for (var i = 0; i < out.length; i++) {
      out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16);
    }
    return out;
  }

  function concatBytes() {
    var total = 0;
    for (var i = 0; i < arguments.length; i++) {
      total += arguments[i].length;
    }
    var out = new Uint8Array(total);
    var offset = 0;
    for (var j = 0; j < arguments.length; j++) {
      var arr = arguments[j];
      out.set(arr, offset);
      offset += arr.length;
    }
    return out;
  }

  function constantTimeEqual(a, b) {
    if (!a || !b || a.length !== b.length) return false;
    var diff = 0;
    for (var i = 0; i < a.length; i++) {
      diff |= a[i] ^ b[i];
    }
    return diff === 0;
  }

  function sha256Bytes(bytes) {
    return new Uint8Array(getSha256().array(bytes));
  }

  function hmacSha256Bytes(keyBytes, msgBytes) {
    return new Uint8Array(getSha256().hmac.array(keyBytes, msgBytes));
  }

  function validateIdentityRecord(rec) {
    if (!rec || typeof rec.publicKeyHex !== "string" || typeof rec.secretKeyHex !== "string") {
      return false;
    }
    try {
      var pub = hexToBytes(rec.publicKeyHex);
      var sec = hexToBytes(rec.secretKeyHex);
      if (pub.length !== 32 || sec.length !== 64) {
        return false;
      }
      var derived = getNacl().sign.keyPair.fromSecretKey(sec);
      return constantTimeEqual(derived.publicKey, pub);
    } catch (_) {
      return false;
    }
  }

  function computePublicIdentityView(rec) {
    var pub = hexToBytes(rec.publicKeyHex);
    return {
      publicKeyHex: bytesToHex(pub),
      deviceId: bytesToHex(sha256Bytes(pub)),
      createdAt: rec.createdAt || ""
    };
  }

  function generateDeviceIdentity() {
    var lib = getNacl();
    var seed = lib.randomBytes(32);
    var kp = lib.sign.keyPair.fromSeed(seed);
    return {
      publicKeyHex: bytesToHex(kp.publicKey),
      secretKeyHex: bytesToHex(kp.secretKey)
    };
  }

  function createEphemeralPairingData() {
    var lib = getNacl();
    var ephem = lib.box.keyPair();
    var salt = lib.randomBytes(16);
    var commitment = sha256Bytes(concatBytes(ephem.publicKey, salt));
    return {
      ephemeralPublicKey: ephem.publicKey,
      ephemeralSecretKey: ephem.secretKey,
      salt: salt,
      commitment: commitment,
      commitmentHex: bytesToHex(commitment),
      ephemeralPublicKeyHex: bytesToHex(ephem.publicKey),
      saltHex: bytesToHex(salt)
    };
  }

  function deriveSASFromTranscript(nhBytes, kSharedBytes, transcriptHashBytes) {
    var prk = hmacSha256Bytes(nhBytes, kSharedBytes);
    var info = concatBytes(utf8ToBytes(EXPAND_INFO_TAG), transcriptHashBytes);
    var okm = hmacSha256Bytes(prk, concatBytes(info, new Uint8Array([0x01])));
    for (var i = 0; i < 8; i++) {
      var off = i * 4;
      var candidate = (
        ((okm[off] << 24) | (okm[off + 1] << 16) | (okm[off + 2] << 8) | okm[off + 3]) >>> 0
      );
      if (candidate < REJECTION_THRESHOLD) {
        var num = candidate % 1000000;
        var s = String(num);
        while (s.length < 6) s = "0" + s;
        return s.slice(0, 3) + "-" + s.slice(3);
      }
    }
    throw new Error("SAS derivation exhausted; retry pairing");
  }

  function completeHostReveal(opts) {
    var lib = getNacl();
    var hostPub = hexToBytes(opts.hostPublicKeyHex);
    var hostCommit = hexToBytes(opts.hostCommitmentHex);
    var hostEphemeral = hexToBytes(opts.hostEphemeralHex);
    var hostSalt = hexToBytes(opts.hostSaltHex);
    var hostProof = hexToBytes(opts.hostProofHex);
    var pairingNonce = hexToBytes(opts.pairingNonceHex);
    var clientPub = hexToBytes(opts.clientPublicKeyHex);
    var clientCommit = hexToBytes(opts.clientCommitmentHex);
    var clientSecret = hexToBytes(opts.clientSecretKeyHex);
    var tokenBytes = utf8ToBytes(opts.token);

    if (hostPub.length !== 32 || clientPub.length !== 32 || clientSecret.length !== 64) {
      throw new Error("invalid key dimensions");
    }
    if (hostEphemeral.length !== 32 || hostSalt.length !== 16 || hostCommit.length !== 32) {
      throw new Error("invalid host reveal dimensions");
    }

    var hostIDDigest = sha256Bytes(hostPub);
    var hostIdHex = bytesToHex(hostIDDigest);
    if (opts.expectedHostIdHex) {
      var expectedHostBytes = hexToBytes(opts.expectedHostIdHex);
      if (!constantTimeEqual(hostIDDigest, expectedHostBytes)) {
        throw new Error("hostId does not match host public key");
      }
    }

    var expectedHostCommit = sha256Bytes(concatBytes(hostEphemeral, hostSalt));
    if (!constantTimeEqual(expectedHostCommit, hostCommit)) {
      throw new Error("host commitment opening verification failed");
    }

    var clientIDDigest = sha256Bytes(clientPub);
    var sessionKeyHash = sha256Bytes(tokenBytes);

    var transcript = concatBytes(
      utf8ToBytes(TRANSCRIPT_TAG),
      hostIDDigest,
      clientIDDigest,
      clientCommit,
      hostCommit,
      opts.clientEphemeralPublicKey,
      hostEphemeral,
      pairingNonce,
      sessionKeyHash
    );
    if (transcript.length !== 271) {
      throw new Error("unexpected transcript length: " + transcript.length);
    }

    var transcriptHash = sha256Bytes(transcript);
    var hostProofPayload = concatBytes(utf8ToBytes(HOST_PROOF_TAG), transcriptHash);
    if (!lib.sign.detached.verify(hostProofPayload, hostProof, hostPub)) {
      throw new Error("host identity signature verification failed");
    }

    var kShared = lib.scalarMult(opts.clientEphemeralSecretKey, hostEphemeral);
    var sas = deriveSASFromTranscript(pairingNonce, kShared, transcriptHash);

    var clientProofPayload = concatBytes(utf8ToBytes(CLIENT_PROOF_TAG), transcriptHash);
    var clientProof = lib.sign.detached(clientProofPayload, clientSecret);

    return {
      sas: sas,
      clientProofHex: bytesToHex(clientProof),
      hostIdHex: hostIdHex
    };
  }

  function signDeviceSessionChallenge(opts) {
    var lib = getNacl();
    var msg = "flitnix-session-auth-v1:" + opts.hostId + ":" + opts.deviceId + ":" + String(opts.timestamp) + ":" + opts.nonce;
    var sig = lib.sign.detached(utf8ToBytes(msg), hexToBytes(opts.secretKeyHex));
    return bytesToHex(sig);
  }

  // IndexedDB persistence for plain-HTTP browser pairing identity & paired hosts.
  // Fail-closed: NEVER fall back to localStorage, sessionStorage, cookies, or memory-only keys.
  var DB_NAME = "flitnix-device-store";
  var DB_VERSION = 1;

  function getIDBFactory() {
    if (typeof indexedDB !== "undefined") return indexedDB;
    if (root && root.indexedDB) return root.indexedDB;
    return null;
  }

  function openDeviceDB() {
    return new Promise(function (resolve, reject) {
      var idb = getIDBFactory();
      if (!idb) {
        reject(new Error("IndexedDB unavailable in this browser"));
        return;
      }
      var req;
      try {
        req = idb.open(DB_NAME, DB_VERSION);
      } catch (e) {
        reject(e || new Error("IndexedDB blocked by browser policy"));
        return;
      }
      req.onupgradeneeded = function (ev) {
        var db = ev.target.result;
        if (!db.objectStoreNames.contains("identity")) {
          db.createObjectStore("identity", { keyPath: "id" });
        }
        if (!db.objectStoreNames.contains("hosts")) {
          var hostStore = db.createObjectStore("hosts", { keyPath: "key" });
          hostStore.createIndex("byDeviceId", "deviceId", { unique: false });
        }
      };
      req.onsuccess = function () { resolve(req.result); };
      req.onblocked = function () { reject(new Error("IndexedDB access blocked by another tab")); };
      req.onerror = function () { reject(req.error || new Error("failed to open device store")); };
    });
  }

  function withStoreTx(storeName, mode, fn) {
    return openDeviceDB().then(function (db) {
      return new Promise(function (resolve, reject) {
        var settled = false;
        var tx;
        function safeClose() {
          try { db.close(); } catch (_) {}
        }
        try {
          tx = db.transaction(storeName, mode);
        } catch (err) {
          safeClose();
          reject(err || new Error("IndexedDB transaction failed"));
          return;
        }
        var resultVal;
        tx.oncomplete = function () {
          safeClose();
          if (!settled) {
            settled = true;
            resolve(resultVal);
          }
        };
        tx.onerror = function () {
          safeClose();
          if (!settled) {
            settled = true;
            reject(tx.error || new Error("IndexedDB transaction error"));
          }
        };
        tx.onabort = function () {
          safeClose();
          if (!settled) {
            settled = true;
            reject(tx.error || new Error("IndexedDB transaction aborted"));
          }
        };

        Promise.resolve().then(function () {
          return fn(tx.objectStore(storeName));
        }).then(function (val) {
          resultVal = val;
        }).catch(function (err) {
          if (!settled) {
            settled = true;
            try { tx.abort(); } catch (_) {}
            safeClose();
            reject(err);
          }
        });
      });
    });
  }

  function idbRequest(req) {
    return new Promise(function (resolve, reject) {
      req.onsuccess = function () { resolve(req.result); };
      req.onerror = function () { reject(req.error || new Error("IndexedDB operation failed")); };
    });
  }

  // Proactively test IndexedDB read+write capability (catches private browsing where
  // global indexedDB is defined yet readwrite transactions throw InvalidStateError/SecurityError).
  function probeStorage() {
    return withStoreTx("identity", "readwrite", function (store) {
      var probeRec = { id: "__storage_probe__", ts: Date.now() };
      return idbRequest(store.put(probeRec)).then(function () {
        return idbRequest(store.delete("__storage_probe__"));
      }).then(function () {
        return true;
      });
    });
  }

  function loadValidatedSecretIdentityInternal() {
    return withStoreTx("identity", "readonly", function (store) {
      return idbRequest(store.get("default")).then(function (rec) {
        if (!rec) return null;
        if (!validateIdentityRecord(rec)) {
          throw new Error("Stored browser device key is corrupt; trusted pairing failed closed");
        }
        return rec;
      });
    });
  }

  function hasValidIdentity() {
    return loadValidatedSecretIdentityInternal().then(function (rec) {
      return !!rec;
    }).catch(function () {
      return false;
    });
  }

  function getPublicIdentity() {
    return loadValidatedSecretIdentityInternal().then(function (rec) {
      return rec ? computePublicIdentityView(rec) : null;
    });
  }

  function ensurePublicIdentity() {
    return withStoreTx("identity", "readwrite", function (store) {
      return idbRequest(store.get("default")).then(function (existing) {
        if (existing) {
          if (!validateIdentityRecord(existing)) {
            throw new Error("Stored browser device key is corrupt; cannot reuse identity");
          }
          return computePublicIdentityView(existing);
        }
        var created = generateDeviceIdentity();
        created.id = "default";
        created.createdAt = new Date().toISOString();
        if (!validateIdentityRecord(created)) {
          throw new Error("Generated device identity failed self-validation");
        }
        return idbRequest(store.put(created)).then(function () {
          return computePublicIdentityView(created);
        });
      });
    });
  }

  function completeHostRevealWithStoredIdentity(opts) {
    return loadValidatedSecretIdentityInternal().then(function (idRec) {
      if (!idRec) {
        throw new Error("Browser device identity missing in persistent IndexedDB storage");
      }
      var merged = {};
      for (var k in opts) {
        if (Object.prototype.hasOwnProperty.call(opts, k)) merged[k] = opts[k];
      }
      merged.clientPublicKeyHex = idRec.publicKeyHex;
      merged.clientSecretKeyHex = idRec.secretKeyHex;
      return completeHostReveal(merged);
    });
  }

  function savePairedHost(record) {
    if (!record || !record.origin || !record.hostId || !record.deviceId || !record.hostPublicKey) {
      return Promise.reject(new Error("incomplete paired host record"));
    }
    var derivedHostId = bytesToHex(sha256Bytes(hexToBytes(record.hostPublicKey)));
    if (derivedHostId.toLowerCase() !== String(record.hostId).toLowerCase()) {
      return Promise.reject(new Error("paired hostId/publicKey mismatch"));
    }
    var key = String(record.origin) + "|" + String(record.deviceId);
    var toStore = {
      key: key,
      origin: String(record.origin),
      hostId: String(record.hostId).toLowerCase(),
      deviceId: String(record.deviceId).toLowerCase(),
      deviceName: record.deviceName || "",
      hostPublicKey: String(record.hostPublicKey).toLowerCase(),
      pairedAt: record.pairedAt || new Date().toISOString()
    };
    return withStoreTx("hosts", "readwrite", function (store) {
      return idbRequest(store.put(toStore)).then(function () {
        return toStore;
      });
    });
  }

  function listPairedHostsForOrigin(origin) {
    return withStoreTx("hosts", "readonly", function (store) {
      return idbRequest(store.getAll()).then(function (all) {
        return (all || []).filter(function (rec) {
          return !origin || rec.origin === origin;
        });
      });
    });
  }

  function getPairedHostByDeviceId(deviceId) {
    return withStoreTx("hosts", "readonly", function (store) {
      var idx = store.index("byDeviceId");
      return idbRequest(idx.get(deviceId)).then(function (rec) {
        return rec || null;
      });
    });
  }

  function signDeviceSessionChallengeWithStoredIdentity(opts) {
    if (!opts || !opts.hostId || !opts.deviceId || !opts.nonce) {
      return Promise.reject(new Error("missing challenge parameters"));
    }
    return getPairedHostByDeviceId(opts.deviceId).then(function (pinned) {
      if (!pinned) {
        throw new Error("Device pairing record not found in IndexedDB");
      }
      if (opts.expectedOrigin && pinned.origin !== opts.expectedOrigin) {
        throw new Error("Paired host origin mismatch");
      }
      if (String(pinned.hostId).toLowerCase() !== String(opts.hostId).toLowerCase()) {
        throw new Error("Refused to sign challenge for mismatched or unpinned hostId");
      }
      if (pinned.hostPublicKey) {
        var expectedHostId = bytesToHex(sha256Bytes(hexToBytes(pinned.hostPublicKey)));
        if (expectedHostId !== String(pinned.hostId).toLowerCase()) {
          throw new Error("Pinned host public key integrity check failed");
        }
      }
      return loadValidatedSecretIdentityInternal().then(function (idRec) {
        if (!idRec) {
          throw new Error("Device signing key not found in browser storage. Pair this browser again.");
        }
        var expectedDeviceId = bytesToHex(sha256Bytes(hexToBytes(idRec.publicKeyHex)));
        if (expectedDeviceId !== String(opts.deviceId).toLowerCase()) {
          throw new Error("Challenge deviceId does not match this browser's key");
        }
        return signDeviceSessionChallenge({
          hostId: opts.hostId,
          deviceId: opts.deviceId,
          nonce: opts.nonce,
          timestamp: opts.timestamp,
          secretKeyHex: idRec.secretKeyHex
        });
      });
    });
  }

  function removePairedHost(keyOrDeviceId) {
    return withStoreTx("hosts", "readwrite", function (store) {
      return idbRequest(store.getAll()).then(function (all) {
        var removals = (all || [])
          .filter(function (rec) {
            return rec.key === keyOrDeviceId || rec.deviceId === keyOrDeviceId;
          })
          .map(function (rec) { return idbRequest(store.delete(rec.key)); });
        return Promise.all(removals);
      });
    });
  }

  var api = {
    utf8ToBytes: utf8ToBytes,
    bytesToHex: bytesToHex,
    hexToBytes: hexToBytes,
    probeStorage: probeStorage,
    hasValidIdentity: hasValidIdentity,
    getPublicIdentity: getPublicIdentity,
    ensurePublicIdentity: ensurePublicIdentity,
    createEphemeralPairingData: createEphemeralPairingData,
    deriveSASFromTranscript: deriveSASFromTranscript,
    completeHostReveal: completeHostReveal,
    completeHostRevealWithStoredIdentity: completeHostRevealWithStoredIdentity,
    signDeviceSessionChallenge: signDeviceSessionChallenge,
    signDeviceSessionChallengeWithStoredIdentity: signDeviceSessionChallengeWithStoredIdentity,
    savePairedHost: savePairedHost,
    listPairedHostsForOrigin: listPairedHostsForOrigin,
    getPairedHostByDeviceId: getPairedHostByDeviceId,
    removePairedHost: removePairedHost,
    // Test helper for Node interoperability (never stores keys in IndexedDB or localStorage):
    generateDeviceIdentity: generateDeviceIdentity
  };

  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.FlitnixDeviceCrypto = api;
  }
})(typeof window !== "undefined" ? window : globalThis);
