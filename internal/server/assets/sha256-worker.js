/**
 * Flitnix Resumable Upload Web Worker (sha256-worker.js)
 *
 * Responsibilities:
 * - Worker-centric continuous streaming uploads via Fetch with duplex: "half"
 * - In-worker incremental SHA-256 hashing via TransformStream
 * - Streaming capability detection inside the worker context
 * - Direct Blob XHR fallback for browsers lacking request streaming (configurable 8, 16, 32, 64 MB; default 16 MB)
 * - Hasher prefix reconstruction (REBUILD_HASHER) and prefix verification
 * - Active cancellation coordination (AbortController / XHR abort -> PATCH_ABORTED)
 * - Bounded memory execution: no full-file buffers, 1 MB hashing slices, zero buffer detaching
 */

(function (workerScope) {
  "use strict";

  // ---------------------------------------------------------------------------
  // Embedded Pure JS Streaming SHA-256 (FIPS 180-4) with clone() support
  // ---------------------------------------------------------------------------
  // Exact embedded js-sha256 library
  var jsSha256 = (function() {
    var module = { exports: {} };
    var exports = module.exports;
    /**
 * [js-sha256]{@link https://github.com/emn178/js-sha256}
 *
 * @version 1.0.0
 * @author Chen, Yi-Cyuan [emn178@gmail.com]
 * @copyright Chen, Yi-Cyuan 2014-2026
 * @license MIT
 */
!function(t,h){"object"==typeof exports&&"undefined"!=typeof module?module.exports=h():"function"==typeof define&&define.amd?define(h):(t="undefined"!=typeof globalThis?globalThis:t||self).sha256=h()}(this,function(){"use strict";var t="undefined"!=typeof ArrayBuffer,h=function(h){if("string"===typeof h)return[h,!0];if(Array.isArray(h))return[h,!1];if(t&&h){if(h.constructor===ArrayBuffer)return[new Uint8Array(h),!1];if(ArrayBuffer.isView(h))return[h,!1]}throw new Error("input is invalid type")},i="0123456789abcdef".split(""),s=[-2147483648,8388608,32768,128],e=[24,16,8,0],r=[1116352408,1899447441,3049323471,3921009573,961987163,1508970993,2453635748,2870763221,3624381080,310598401,607225278,1426881987,1925078388,2162078206,2614888103,3248222580,3835390401,4022224774,264347078,604807628,770255983,1249150122,1555081692,1996064986,2554220882,2821834349,2952996808,3210313671,3336571891,3584528711,113926993,338241895,666307205,773529912,1294757372,1396182291,1695183700,1986661051,2177026350,2456956037,2730485921,2820302411,3259730800,3345764771,3516065817,3600352804,4094571909,275423344,430227734,506948616,659060556,883997877,958139571,1322822218,1537002063,1747873779,1955562222,2024104815,2227730452,2361852424,2428436474,2756734187,3204031479,3329325298],n=["hex","array","digest","arrayBuffer"],o=[],a=function(t,h){return function(i){return new c(h,!0).update(i)[t]()}},f=function(t){var h=a("hex",t);h.create=function(){return new c(t)},h.update=function(t){return h.create().update(t)};for(var i=0;i<n.length;++i){var s=n[i];h[s]=a(s,t)}return h},u=function(t,h){return function(i,s){return new y(i,h,!0).update(s)[t]()}},l=function(t){var h=u("hex",t);h.create=function(h){return new y(h,t)},h.update=function(t,i){return h.create(t).update(i)};for(var i=0;i<n.length;++i){var s=n[i];h[s]=u(s,t)}return h};function c(t,h){h?(o[0]=o[16]=o[1]=o[2]=o[3]=o[4]=o[5]=o[6]=o[7]=o[8]=o[9]=o[10]=o[11]=o[12]=o[13]=o[14]=o[15]=0,this.blocks=o):this.blocks=[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0],t?(this.h0=3238371032,this.h1=914150663,this.h2=812702999,this.h3=4144912697,this.h4=4290775857,this.h5=1750603025,this.h6=1694076839,this.h7=3204075428):(this.h0=1779033703,this.h1=3144134277,this.h2=1013904242,this.h3=2773480762,this.h4=1359893119,this.h5=2600822924,this.h6=528734635,this.h7=1541459225),this.block=this.start=this.bytes=this.hBytes=0,this.finalized=this.hashed=!1,this.first=!0,this.is224=t}function y(t,i,s){var e,r=h(t);if(t=r[0],r[1]){var n,o=[],a=t.length,f=0;for(e=0;e<a;++e)(n=t.charCodeAt(e))<128?o[f++]=n:n<2048?(o[f++]=192|n>>>6,o[f++]=128|63&n):n<55296||n>=57344?(o[f++]=224|n>>>12,o[f++]=128|n>>>6&63,o[f++]=128|63&n):(n=65536+((1023&n)<<10|1023&t.charCodeAt(++e)),o[f++]=240|n>>>18,o[f++]=128|n>>>12&63,o[f++]=128|n>>>6&63,o[f++]=128|63&n);t=o}t.length>64&&(t=new c(i,!0).update(t).array());var u=[],l=[];for(e=0;e<64;++e){var y=t[e]||0;u[e]=92^y,l[e]=54^y}c.call(this,i,s),this.update(l),this.oKeyPad=u,this.inner=!0,this.sharedMemory=s}c.prototype.update=function(t){if(this.finalized)throw new Error("finalize already called");var i=h(t);t=i[0];for(var s,r,n=i[1],o=0,a=t.length,f=this.blocks;o<a;){if(this.hashed&&(this.hashed=!1,f[0]=this.block,this.block=f[16]=f[1]=f[2]=f[3]=f[4]=f[5]=f[6]=f[7]=f[8]=f[9]=f[10]=f[11]=f[12]=f[13]=f[14]=f[15]=0),n)for(r=this.start;o<a&&r<64;++o)(s=t.charCodeAt(o))<128?f[r>>>2]|=s<<e[3&r++]:s<2048?(f[r>>>2]|=(192|s>>>6)<<e[3&r++],f[r>>>2]|=(128|63&s)<<e[3&r++]):s<55296||s>=57344?(f[r>>>2]|=(224|s>>>12)<<e[3&r++],f[r>>>2]|=(128|s>>>6&63)<<e[3&r++],f[r>>>2]|=(128|63&s)<<e[3&r++]):(s=65536+((1023&s)<<10|1023&t.charCodeAt(++o)),f[r>>>2]|=(240|s>>>18)<<e[3&r++],f[r>>>2]|=(128|s>>>12&63)<<e[3&r++],f[r>>>2]|=(128|s>>>6&63)<<e[3&r++],f[r>>>2]|=(128|63&s)<<e[3&r++]);else for(r=this.start;o<a&&r<64;++o)f[r>>>2]|=t[o]<<e[3&r++];this.lastByteIndex=r,this.bytes+=r-this.start,r>=64?(this.block=f[16],this.start=r-64,this.hash(),this.hashed=!0):this.start=r}return this.bytes>4294967295&&(this.hBytes+=this.bytes/4294967296|0,this.bytes=this.bytes%4294967296),this},c.prototype.finalize=function(){if(!this.finalized){this.finalized=!0;var t=this.blocks,h=this.lastByteIndex;t[16]=this.block,t[h>>>2]|=s[3&h],this.block=t[16],h>=56&&(this.hashed||this.hash(),t[0]=this.block,t[16]=t[1]=t[2]=t[3]=t[4]=t[5]=t[6]=t[7]=t[8]=t[9]=t[10]=t[11]=t[12]=t[13]=t[14]=t[15]=0),t[14]=this.hBytes<<3|this.bytes>>>29,t[15]=this.bytes<<3,this.hash()}},c.prototype.hash=function(){var t,h,i,s,e,n,o,a,f,u=this.h0,l=this.h1,c=this.h2,y=this.h3,p=this.h4,d=this.h5,b=this.h6,v=this.h7,w=this.blocks;for(t=16;t<64;++t)h=((e=w[t-15])>>>7|e<<25)^(e>>>18|e<<14)^e>>>3,i=((e=w[t-2])>>>17|e<<15)^(e>>>19|e<<13)^e>>>10,w[t]=w[t-16]+h+w[t-7]+i|0;for(f=l&c,t=0;t<64;t+=4)this.first?(this.is224?(n=300032,v=(e=w[0]-1413257819)-150054599|0,y=e+24177077|0):(n=704751109,v=(e=w[0]-210244248)-1521486534|0,y=e+143694565|0),this.first=!1):(h=(u>>>2|u<<30)^(u>>>13|u<<19)^(u>>>22|u<<10),s=(n=u&l)^u&c^f,v=y+(e=v+(i=(p>>>6|p<<26)^(p>>>11|p<<21)^(p>>>25|p<<7))+(p&d^~p&b)+r[t]+w[t])|0,y=e+(h+s)|0),h=(y>>>2|y<<30)^(y>>>13|y<<19)^(y>>>22|y<<10),s=(o=y&u)^y&l^n,b=c+(e=b+(i=(v>>>6|v<<26)^(v>>>11|v<<21)^(v>>>25|v<<7))+(v&p^~v&d)+r[t+1]+w[t+1])|0,h=((c=e+(h+s)|0)>>>2|c<<30)^(c>>>13|c<<19)^(c>>>22|c<<10),s=(a=c&y)^c&u^o,d=l+(e=d+(i=(b>>>6|b<<26)^(b>>>11|b<<21)^(b>>>25|b<<7))+(b&v^~b&p)+r[t+2]+w[t+2])|0,h=((l=e+(h+s)|0)>>>2|l<<30)^(l>>>13|l<<19)^(l>>>22|l<<10),s=(f=l&c)^l&y^a,p=u+(e=p+(i=(d>>>6|d<<26)^(d>>>11|d<<21)^(d>>>25|d<<7))+(d&b^~d&v)+r[t+3]+w[t+3])|0,u=e+(h+s)|0,this.chromeBugWorkAround=!0;this.h0=this.h0+u|0,this.h1=this.h1+l|0,this.h2=this.h2+c|0,this.h3=this.h3+y|0,this.h4=this.h4+p|0,this.h5=this.h5+d|0,this.h6=this.h6+b|0,this.h7=this.h7+v|0},c.prototype.hex=function(){this.finalize();var t=this.h0,h=this.h1,s=this.h2,e=this.h3,r=this.h4,n=this.h5,o=this.h6,a=this.h7,f=i[t>>>28&15]+i[t>>>24&15]+i[t>>>20&15]+i[t>>>16&15]+i[t>>>12&15]+i[t>>>8&15]+i[t>>>4&15]+i[15&t]+i[h>>>28&15]+i[h>>>24&15]+i[h>>>20&15]+i[h>>>16&15]+i[h>>>12&15]+i[h>>>8&15]+i[h>>>4&15]+i[15&h]+i[s>>>28&15]+i[s>>>24&15]+i[s>>>20&15]+i[s>>>16&15]+i[s>>>12&15]+i[s>>>8&15]+i[s>>>4&15]+i[15&s]+i[e>>>28&15]+i[e>>>24&15]+i[e>>>20&15]+i[e>>>16&15]+i[e>>>12&15]+i[e>>>8&15]+i[e>>>4&15]+i[15&e]+i[r>>>28&15]+i[r>>>24&15]+i[r>>>20&15]+i[r>>>16&15]+i[r>>>12&15]+i[r>>>8&15]+i[r>>>4&15]+i[15&r]+i[n>>>28&15]+i[n>>>24&15]+i[n>>>20&15]+i[n>>>16&15]+i[n>>>12&15]+i[n>>>8&15]+i[n>>>4&15]+i[15&n]+i[o>>>28&15]+i[o>>>24&15]+i[o>>>20&15]+i[o>>>16&15]+i[o>>>12&15]+i[o>>>8&15]+i[o>>>4&15]+i[15&o];return this.is224||(f+=i[a>>>28&15]+i[a>>>24&15]+i[a>>>20&15]+i[a>>>16&15]+i[a>>>12&15]+i[a>>>8&15]+i[a>>>4&15]+i[15&a]),f},c.prototype.toString=c.prototype.hex,c.prototype.digest=function(){this.finalize();var t=this.h0,h=this.h1,i=this.h2,s=this.h3,e=this.h4,r=this.h5,n=this.h6,o=this.h7,a=[t>>>24&255,t>>>16&255,t>>>8&255,255&t,h>>>24&255,h>>>16&255,h>>>8&255,255&h,i>>>24&255,i>>>16&255,i>>>8&255,255&i,s>>>24&255,s>>>16&255,s>>>8&255,255&s,e>>>24&255,e>>>16&255,e>>>8&255,255&e,r>>>24&255,r>>>16&255,r>>>8&255,255&r,n>>>24&255,n>>>16&255,n>>>8&255,255&n];return this.is224||a.push(o>>>24&255,o>>>16&255,o>>>8&255,255&o),a},c.prototype.array=c.prototype.digest,c.prototype.arrayBuffer=function(){this.finalize();var t=new ArrayBuffer(this.is224?28:32),h=new DataView(t);return h.setUint32(0,this.h0),h.setUint32(4,this.h1),h.setUint32(8,this.h2),h.setUint32(12,this.h3),h.setUint32(16,this.h4),h.setUint32(20,this.h5),h.setUint32(24,this.h6),this.is224||h.setUint32(28,this.h7),t},y.prototype=new c,y.prototype.finalize=function(){if(c.prototype.finalize.call(this),this.inner){this.inner=!1;var t=this.array();c.call(this,this.is224,this.sharedMemory),this.update(this.oKeyPad),this.update(t),c.prototype.finalize.call(this)}};var p=f(),d=f(!0);p.sha256=p,p.sha224=d,p.hmac=l(),d.hmac=l(!0);const b="object"==typeof globalThis?globalThis:"object"==typeof self?self:"object"==typeof window?window:"object"==typeof global?global:void 0;return b&&(b.sha224=d),p});
    return typeof module.exports === "function" ? module.exports : (typeof sha256 !== "undefined" ? sha256 : this.sha256);
  })();

  function createSHA256() {
    var h = jsSha256.create();
    h.clone = function () {
      var copy = jsSha256.create();
      copy.blocks = this.blocks.slice();
      copy.h0 = this.h0;
      copy.h1 = this.h1;
      copy.h2 = this.h2;
      copy.h3 = this.h3;
      copy.h4 = this.h4;
      copy.h5 = this.h5;
      copy.h6 = this.h6;
      copy.h7 = this.h7;
      copy.hBytes = this.hBytes;
      copy.bytes = this.bytes;
      copy.start = this.start;
      copy.block = this.block;
      copy.hashed = this.hashed;
      copy.finalized = this.finalized;
      copy.first = this.first;
      copy.is224 = this.is224;
      copy.lastByteIndex = this.lastByteIndex;
      return copy;
    };
    return h;
  }

  // ---------------------------------------------------------------------------
  // Feature Detection for Fetch Request Body Streaming
  // ---------------------------------------------------------------------------
  var cachedStreamingSupport = null;

  async function testWorkerRequestStreamingSupport() {
    if (cachedStreamingSupport !== null) {
      return cachedStreamingSupport;
    }
    if (typeof Request === "undefined" || typeof ReadableStream === "undefined") {
      cachedStreamingSupport = false;
      return false;
    }
    try {
      var streamConstructed = false;
      var dummyStream = new ReadableStream({
        start: function (controller) {
          controller.enqueue(new Uint8Array([0]));
          controller.close();
          streamConstructed = true;
        }
      });
      var req = new Request("http://localhost:0", {
        method: "POST",
        body: dummyStream,
        duplex: "half"
      });
      // In WebKit / browsers without request streaming, constructor throws TypeError
      // or strips body. Check both duplex and stream attachment:
      cachedStreamingSupport = !!(req && (streamConstructed || "duplex" in req));
    } catch (e) {
      cachedStreamingSupport = false;
    }
    return cachedStreamingSupport;
  }

  // ---------------------------------------------------------------------------
  // Worker Upload State Management
  // ---------------------------------------------------------------------------
  var activeUploads = {}; // keyed by transferId

  function getOrCreateUpload(transferId) {
    if (!activeUploads[transferId]) {
      activeUploads[transferId] = {
        transferId: transferId,
        hasher: null,
        file: null,
        uploadUrl: null,
        authHeader: null,
        serverOffset: 0,
        serverPrefixDigest: null,
        abortController: null,
        xhr: null,
        cancelled: false,
        fallbackBodySize: 16 * 1024 * 1024,
        forceFallback: false
      };
    }
    return activeUploads[transferId];
  }

  function cleanupUpload(transferId) {
    delete activeUploads[transferId];
  }

  // ---------------------------------------------------------------------------
  // Execution Paths: Streaming & Fallback
  // ---------------------------------------------------------------------------

  /**
   * Continuous Streaming Upload via Fetch with TransformStream
   */
  async function executeStreamingUpload(upload) {
    var transferId = upload.transferId;
    if (upload.cancelled) {
      upload.hasher = null;
      workerScope.postMessage({ type: "PATCH_ABORTED", transferId: transferId });
      return;
    }

    var file = upload.file;
    var serverOffset = upload.serverOffset;
    var uploadUrl = upload.uploadUrl;
    var authHeader = upload.authHeader;
    var totalSize = file.size;

    if (!upload.abortController) {
      upload.abortController = new AbortController();
    }
    var lastProgressTime = 0;
    var currentUploaded = serverOffset;
    var lastProgressBytes = serverOffset;
    var lastTime = Date.now();
    var smoothedSpeed = 0;

    // Slice file from server offset to end
    var fileSlice = file.slice(serverOffset);
    var inputStream = fileSlice.stream();

    var transformStream = new TransformStream({
      transform: function (chunk, controller) {
        if (upload.cancelled) {
          controller.error(new Error("upload cancelled"));
          return;
        }

        // Incremental SHA-256 update directly in the worker
        upload.hasher.update(chunk);
        currentUploaded += chunk.byteLength;

        var now = Date.now();
        var dt = (now - lastTime) / 1000;
        if (dt >= 0.25) {
          var instant = (currentUploaded - lastProgressBytes) / dt;
          smoothedSpeed = smoothedSpeed === 0 ? instant : (0.7 * smoothedSpeed + 0.3 * instant);
          lastProgressBytes = currentUploaded;
          lastTime = now;
        }

        if (now - lastProgressTime >= 100 || currentUploaded === totalSize) {
          lastProgressTime = now;
          var eta = (smoothedSpeed > 0 && totalSize > currentUploaded)
            ? Math.round((totalSize - currentUploaded) / smoothedSpeed)
            : 0;
          workerScope.postMessage({
            type: "PROGRESS",
            transferId: transferId,
            uploaded: currentUploaded,
            total: totalSize,
            speed: smoothedSpeed,
            eta: eta,
            serverOffset: currentUploaded
          });
        }

        controller.enqueue(chunk);
      }
    });

    var uploadStream = inputStream.pipeThrough(transformStream);
    var headers = {
      "Tus-Resumable": "1.0.0",
      "Upload-Offset": String(serverOffset),
      "Content-Type": "application/offset+octet-stream"
    };
    if (authHeader) {
      headers["Authorization"] = authHeader;
    }

    try {
      var resp = await fetch(uploadUrl, {
        method: "PATCH",
        headers: headers,
        body: uploadStream,
        duplex: "half",
        signal: upload.abortController.signal
      });

      if (upload.cancelled) {
        upload.hasher = null;
        workerScope.postMessage({ type: "PATCH_ABORTED", transferId: transferId });
        return;
      }

      if (resp.status === 204) {
        var respOffsetStr = resp.headers.get("Upload-Offset");
        var respPrefixDigest = resp.headers.get("Upload-Prefix-Digest") || "";
        var committedOffset = parseInt(respOffsetStr, 10);
        if (isNaN(committedOffset)) committedOffset = currentUploaded;

        if (committedOffset < totalSize) {
          // Stream ended or connection closed before full file transfer was complete.
          // This is an interrupted partial upload, NOT a full upload completion.
          upload.hasher = null;
          workerScope.postMessage({
            type: "UPLOAD_INTERRUPTED",
            transferId: transferId,
            status: 204,
            serverOffset: committedOffset,
            serverPrefixDigest: respPrefixDigest,
            error: "Stream ended before all bytes were transferred (" + committedOffset + "/" + totalSize + ")",
            clientOffset: currentUploaded
          });
          return;
        }

        var finalDigest = upload.hasher.hex();
        workerScope.postMessage({
          type: "UPLOAD_BYTES_COMPLETE",
          transferId: transferId,
          serverOffset: committedOffset,
          serverPrefixDigest: respPrefixDigest,
          expectedSHA256: finalDigest
        });
      } else if (resp.status === 423) {
        upload.hasher = null;
        var retryAfter = parseInt(resp.headers.get("Retry-After") || "1", 10);
        workerScope.postMessage({
          type: "UPLOAD_INTERRUPTED",
          transferId: transferId,
          status: 423,
          retryAfter: retryAfter,
          error: "Upload resource is actively settling",
          clientOffset: currentUploaded
        });
      } else {
        // Any non-204 invalidates the in-memory hasher state
        upload.hasher = null;
        var errBody = "";
        try { errBody = await resp.text(); } catch (e) {}
        workerScope.postMessage({
          type: "UPLOAD_INTERRUPTED",
          transferId: transferId,
          status: resp.status,
          error: "Server returned HTTP " + resp.status + (errBody ? ": " + errBody : ""),
          clientOffset: currentUploaded
        });
      }
    } catch (err) {
      upload.hasher = null;
      if (upload.cancelled || (err && err.name === "AbortError")) {
        workerScope.postMessage({ type: "PATCH_ABORTED", transferId: transferId });
      } else {
        workerScope.postMessage({
          type: "UPLOAD_INTERRUPTED",
          transferId: transferId,
          error: err ? err.message : "Fetch network streaming failure",
          clientOffset: currentUploaded
        });
      }
    }
  }

  /**
   * Direct Blob XHR Fallback Upload (for browsers lacking request streaming)
   */
  async function executeFallbackUpload(upload) {
    var transferId = upload.transferId;
    var file = upload.file;
    var currentOffset = upload.serverOffset;
    var uploadUrl = upload.uploadUrl;
    var authHeader = upload.authHeader;
    var totalSize = file.size;
    var bodySize = upload.fallbackBodySize || (16 * 1024 * 1024);

    var lastProgressTime = 0;
    var lastProgressBytes = currentOffset;
    var lastTime = Date.now();
    var smoothedSpeed = 0;

    while (currentOffset < totalSize) {
      if (upload.cancelled) {
        upload.hasher = null;
        workerScope.postMessage({ type: "PATCH_ABORTED", transferId: transferId });
        return;
      }

      var sliceEnd = Math.min(totalSize, currentOffset + bodySize);
      var sliceBlob = file.slice(currentOffset, sliceEnd);

      // In-worker bounded hashing of slice in 1 MB chunks
      var subChunkSize = 1024 * 1024;
      for (var pos = 0; pos < sliceBlob.size; pos += subChunkSize) {
        if (upload.cancelled) {
          upload.hasher = null;
          workerScope.postMessage({ type: "PATCH_ABORTED", transferId: transferId });
          return;
        }
        var subEnd = Math.min(sliceBlob.size, pos + subChunkSize);
        var subSlice = sliceBlob.slice(pos, subEnd);
        var subBuf = await subSlice.arrayBuffer();
        upload.hasher.update(new Uint8Array(subBuf));
      }

      // Transmit slice directly via XHR
      try {
        var committedOffset = await new Promise(function (resolve, reject) {
          var xhr = new XMLHttpRequest();
          upload.xhr = xhr;

          xhr.open("PATCH", uploadUrl);
          xhr.setRequestHeader("Tus-Resumable", "1.0.0");
          xhr.setRequestHeader("Upload-Offset", String(currentOffset));
          xhr.setRequestHeader("Content-Type", "application/offset+octet-stream");
          if (authHeader) {
            xhr.setRequestHeader("Authorization", authHeader);
          }

          xhr.upload.onprogress = function (ev) {
            if (!ev.lengthComputable || upload.cancelled) return;
            var sentSoFar = currentOffset + ev.loaded;
            var now = Date.now();
            var dt = (now - lastTime) / 1000;
            if (dt >= 0.25) {
              var instant = (sentSoFar - lastProgressBytes) / dt;
              smoothedSpeed = smoothedSpeed === 0 ? instant : (0.7 * smoothedSpeed + 0.3 * instant);
              lastProgressBytes = sentSoFar;
              lastTime = now;
            }

            if (now - lastProgressTime >= 100 || sentSoFar === totalSize) {
              lastProgressTime = now;
              var eta = (smoothedSpeed > 0 && totalSize > sentSoFar)
                ? Math.round((totalSize - sentSoFar) / smoothedSpeed)
                : 0;
              workerScope.postMessage({
                type: "PROGRESS",
                transferId: transferId,
                uploaded: sentSoFar,
                total: totalSize,
                speed: smoothedSpeed,
                eta: eta,
                serverOffset: currentOffset
              });
            }
          };

          xhr.onload = function () {
            upload.xhr = null;
            if (upload.cancelled) {
              reject({ cancelled: true });
              return;
            }
            if (xhr.status === 204) {
              var offHeader = xhr.getResponseHeader("Upload-Offset");
              var parsed = parseInt(offHeader, 10);
              var nextOffset = !isNaN(parsed) ? parsed : sliceEnd;
              var pDigest = xhr.getResponseHeader("Upload-Prefix-Digest") || "";
              resolve({ offset: nextOffset, prefixDigest: pDigest });
            } else {
              var msg = "XHR PATCH failed (" + xhr.status + ")";
              try {
                var json = JSON.parse(xhr.responseText);
                if (json && json.error) msg = json.error;
              } catch (e) {}
              reject({ status: xhr.status, message: msg });
            }
          };

          xhr.onerror = function () {
            upload.xhr = null;
            reject({ network: true, message: "XHR network error" });
          };

          xhr.ontimeout = function () {
            upload.xhr = null;
            reject({ timeout: true, message: "XHR upload timed out" });
          };

          xhr.onabort = function () {
            upload.xhr = null;
            reject({ cancelled: true });
          };

          xhr.send(sliceBlob);
        });

        currentOffset = committedOffset.offset;
      } catch (err) {
        upload.hasher = null;
        if (upload.cancelled || (err && err.cancelled)) {
          workerScope.postMessage({ type: "PATCH_ABORTED", transferId: transferId });
          return;
        }
        workerScope.postMessage({
          type: "UPLOAD_INTERRUPTED",
          transferId: transferId,
          status: err ? err.status : 0,
          error: err ? err.message : "XHR fallback transfer interrupted",
          clientOffset: currentOffset
        });
        return;
      }
    }

    // Complete fallback upload
    var finalDigest = upload.hasher.hex();
    workerScope.postMessage({
      type: "UPLOAD_BYTES_COMPLETE",
      transferId: transferId,
      serverOffset: totalSize,
      serverPrefixDigest: "",
      expectedSHA256: finalDigest
    });
  }

  /**
   * Hasher Prefix Reconstruction (REBUILD_HASHER)
   */
  async function rebuildHasher(upload, expectedPrefixDigest) {
    var transferId = upload.transferId;
    var file = upload.file;
    var serverOffset = upload.serverOffset;

    var hasher = createSHA256();
    var chunkSize = 1024 * 1024; // 1 MB slices

    if (serverOffset > 0) {
      for (var readOffset = 0; readOffset < serverOffset; readOffset += chunkSize) {
        if (upload.cancelled) {
          upload.hasher = null;
          workerScope.postMessage({ type: "PATCH_ABORTED", transferId: transferId });
          return;
        }
        var end = Math.min(serverOffset, readOffset + chunkSize);
        var chunkSlice = file.slice(readOffset, end);
        var buf = await chunkSlice.arrayBuffer();
        hasher.update(new Uint8Array(buf));

        workerScope.postMessage({
          type: "REBUILD_PROGRESS",
          transferId: transferId,
          hashed: end,
          total: serverOffset
        });
      }
    }

    var localPrefixDigest = hasher.clone().hex();
    var matches = false;

    if (!expectedPrefixDigest || expectedPrefixDigest === "") {
      // If server did not set prefix digest, match if offset is 0
      matches = (serverOffset === 0);
    } else {
      matches = (localPrefixDigest.toLowerCase() === expectedPrefixDigest.toLowerCase());
    }

    if (matches) {
      // Retain the exact verified hasher instance for subsequent upload bytes
      upload.hasher = hasher;
      workerScope.postMessage({
        type: "PREFIX_VERIFIED",
        transferId: transferId,
        serverOffset: serverOffset,
        prefixDigest: localPrefixDigest
      });
    } else {
      upload.hasher = null;
      workerScope.postMessage({
        type: "FILE_MODIFIED",
        transferId: transferId,
        serverOffset: serverOffset,
        expectedPrefixDigest: expectedPrefixDigest,
        localPrefixDigest: localPrefixDigest
      });
    }
  }

  // ---------------------------------------------------------------------------
  // Worker Message Dispatcher
  // ---------------------------------------------------------------------------
  workerScope.onmessage = async function (e) {
    var msg = e.data;
    if (!msg || !msg.type) return;

    switch (msg.type) {
      case "DETECT_CAPABILITY": {
        var supported = await testWorkerRequestStreamingSupport();
        workerScope.postMessage({
          type: "CAPABILITY_RESULT",
          supportsStreaming: supported
        });
        break;
      }

      case "REBUILD_HASHER": {
        var upload = getOrCreateUpload(msg.transferId);
        upload.file = msg.file;
        upload.serverOffset = msg.serverOffset || 0;
        upload.cancelled = false;
        await rebuildHasher(upload, msg.expectedPrefixDigest);
        break;
      }

      case "START_UPLOAD":
      case "RESUME_UPLOAD": {
        var up = getOrCreateUpload(msg.transferId);
        up.file = msg.file;
        up.uploadUrl = msg.uploadUrl;
        up.authHeader = msg.authHeader || null;
        up.serverOffset = msg.serverOffset || 0;
        up.fallbackBodySize = msg.fallbackBodySize || (16 * 1024 * 1024);
        up.forceFallback = !!msg.forceFallback;
        up.cancelled = false;
        up.abortController = new AbortController();

        // If hasher not already verified or offset is 0, initialize clean hasher
        if (!up.hasher || up.serverOffset === 0) {
          up.hasher = createSHA256();
        }

        var streamingOk = !up.forceFallback && (await testWorkerRequestStreamingSupport());
        if (streamingOk) {
          await executeStreamingUpload(up);
        } else {
          await executeFallbackUpload(up);
        }
        break;
      }

      case "CANCEL_UPLOAD": {
        var target = activeUploads[msg.transferId];
        if (target) {
          target.cancelled = true;
          target.hasher = null;
          var inFlight = false;
          if (target.abortController) {
            inFlight = true;
            try { target.abortController.abort(); } catch (err) {}
          }
          if (target.xhr) {
            inFlight = true;
            try { target.xhr.abort(); } catch (err) {}
          }
          if (!inFlight) {
            workerScope.postMessage({
              type: "PATCH_ABORTED",
              transferId: msg.transferId
            });
          }
        } else {
          workerScope.postMessage({
            type: "PATCH_ABORTED",
            transferId: msg.transferId
          });
        }
        break;
      }

      case "RESET_UPLOAD":
      case "TERMINATE_UPLOAD": {
        var toClean = activeUploads[msg.transferId];
        if (toClean) {
          toClean.cancelled = true;
          toClean.hasher = null;
          if (toClean.abortController) {
            try { toClean.abortController.abort(); } catch (err) {}
          }
          if (toClean.xhr) {
            try { toClean.xhr.abort(); } catch (err) {}
          }
          cleanupUpload(msg.transferId);
        }
        break;
      }

      default:
        break;
    }
  };

  workerScope.createSHA256 = createSHA256;
  workerScope.testWorkerRequestStreamingSupport = testWorkerRequestStreamingSupport;

  // Signal worker readiness
  if (typeof workerScope.postMessage === "function") {
    workerScope.postMessage({ type: "READY" });
  }

})(typeof self !== "undefined" ? self : this);
