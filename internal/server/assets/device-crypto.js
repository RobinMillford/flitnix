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

    if (hostEphemeral.length !== 32 || hostSalt.length !== 16 || hostCommit.length !== 32) {
      throw new Error("invalid host reveal dimensions");
    }

    var expectedHostCommit = sha256Bytes(concatBytes(hostEphemeral, hostSalt));
    if (!constantTimeEqual(expectedHostCommit, hostCommit)) {
      throw new Error("host commitment opening verification failed");
    }

    var hostIDDigest = sha256Bytes(hostPub);
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
      hostIdHex: bytesToHex(hostIDDigest)
    };
  }

  function signDeviceSessionChallenge(opts) {
    var lib = getNacl();
    var msg = "flitnix-session-auth-v1:" + opts.hostId + ":" + opts.deviceId + ":" + String(opts.timestamp) + ":" + opts.nonce;
    var sig = lib.sign.detached(utf8ToBytes(msg), hexToBytes(opts.secretKeyHex));
    return bytesToHex(sig);
  }

  // IndexedDB persistence for plain-HTTP browser pairing identity & paired hosts
  var DB_NAME = "flitnix-device-store";
  var DB_VERSION = 1;

  function openDeviceDB() {
    return new Promise(function (resolve, reject) {
      if (typeof indexedDB === "undefined") {
        reject(new Error("IndexedDB unavailable in this browser"));
        return;
      }
      var req = indexedDB.open(DB_NAME, DB_VERSION);
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
      req.onerror = function () { reject(req.error || new Error("failed to open device store")); };
    });
  }

  function idbRequest(req) {
    return new Promise(function (resolve, reject) {
      req.onsuccess = function () { resolve(req.result); };
      req.onerror = function () { reject(req.error || new Error("IndexedDB operation failed")); };
    });
  }

  function loadIdentity() {
    return openDeviceDB().then(function (db) {
      var tx = db.transaction("identity", "readonly");
      return idbRequest(tx.objectStore("identity").get("default")).then(function (rec) {
        db.close();
        return rec || null;
      });
    });
  }

  function getOrCreateIdentity() {
    return openDeviceDB().then(function (db) {
      var tx = db.transaction("identity", "readwrite");
      var store = tx.objectStore("identity");
      return idbRequest(store.get("default")).then(function (existing) {
        if (existing && existing.publicKeyHex && existing.secretKeyHex) {
          db.close();
          return existing;
        }
        var created = generateDeviceIdentity();
        created.id = "default";
        created.createdAt = new Date().toISOString();
        return idbRequest(store.put(created)).then(function () {
          db.close();
          return created;
        });
      });
    });
  }

  function savePairedHost(record) {
    var key = String(record.origin || "") + "|" + String(record.deviceId || "");
    var toStore = {
      key: key,
      origin: record.origin || "",
      hostId: record.hostId || "",
      deviceId: record.deviceId || "",
      deviceName: record.deviceName || "",
      hostPublicKey: record.hostPublicKey || "",
      pairedAt: record.pairedAt || new Date().toISOString()
    };
    return openDeviceDB().then(function (db) {
      var tx = db.transaction("hosts", "readwrite");
      return idbRequest(tx.objectStore("hosts").put(toStore)).then(function () {
        db.close();
        return toStore;
      });
    });
  }

  function listPairedHostsForOrigin(origin) {
    return openDeviceDB().then(function (db) {
      var tx = db.transaction("hosts", "readonly");
      return idbRequest(tx.objectStore("hosts").getAll()).then(function (all) {
        db.close();
        return (all || []).filter(function (rec) {
          return !origin || rec.origin === origin;
        });
      });
    });
  }

  function getPairedHostByDeviceId(deviceId) {
    return openDeviceDB().then(function (db) {
      var tx = db.transaction("hosts", "readonly");
      var idx = tx.objectStore("hosts").index("byDeviceId");
      return idbRequest(idx.get(deviceId)).then(function (rec) {
        db.close();
        return rec || null;
      });
    });
  }

  function removePairedHost(keyOrDeviceId) {
    return openDeviceDB().then(function (db) {
      var tx = db.transaction("hosts", "readwrite");
      var store = tx.objectStore("hosts");
      return idbRequest(store.getAll()).then(function (all) {
        var removals = (all || [])
          .filter(function (rec) {
            return rec.key === keyOrDeviceId || rec.deviceId === keyOrDeviceId;
          })
          .map(function (rec) { return idbRequest(store.delete(rec.key)); });
        return Promise.all(removals).then(function () { db.close(); });
      });
    });
  }

  var api = {
    utf8ToBytes: utf8ToBytes,
    bytesToHex: bytesToHex,
    hexToBytes: hexToBytes,
    generateDeviceIdentity: generateDeviceIdentity,
    createEphemeralPairingData: createEphemeralPairingData,
    deriveSASFromTranscript: deriveSASFromTranscript,
    completeHostReveal: completeHostReveal,
    signDeviceSessionChallenge: signDeviceSessionChallenge,
    loadIdentity: loadIdentity,
    getOrCreateIdentity: getOrCreateIdentity,
    savePairedHost: savePairedHost,
    listPairedHostsForOrigin: listPairedHostsForOrigin,
    getPairedHostByDeviceId: getPairedHostByDeviceId,
    removePairedHost: removePairedHost
  };

  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.FlitnixDeviceCrypto = api;
  }
})(typeof window !== "undefined" ? window : globalThis);
