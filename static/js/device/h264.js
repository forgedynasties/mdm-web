// H.264 helpers for the remote screen (25 Nov 2026; user: "on google chrome remote is fine but on
// zen and firefox edge it says fall back method not h264"). Devices send Annex-B (start codes,
// SPS/PPS in front of each key frame). Chrome decodes that with a loose config; Firefox-family
// browsers and some Edge builds want what the WebCodecs spec describes for "avc" streams: a codec
// string naming the stream's real profile and level, an avcC description built from its SPS/PPS,
// and length-prefixed NAL units. These helpers turn the device's frames into exactly that.
// Loaded before remote.js. A device's real SPS said avc1.428016 (Baseline, level 2.2) while the
// page used to declare avc1.42E01E; strict decoders (the system ones Firefox and Edge use on Windows
// and macOS) can refuse that mismatch. Checked 25 Nov with an ffmpeg-made stream and a live T7 in
// Firefox 132: all frames decode.
(function () {
  'use strict';

  // Split Annex-B into NAL units (start codes 00 00 01 / 00 00 00 01 removed). Returns [] for
  // data that doesn't start with a start code (already length-prefixed, or garbage).
  function nals(b) {
    var out = [], i = 0, n = b.length, start = -1;
    function scAt(k) { return k + 2 < n && b[k] === 0 && b[k + 1] === 0 && (b[k + 2] === 1 || (b[k + 2] === 0 && k + 3 < n && b[k + 3] === 1)); }
    if (!scAt(0)) return out;
    while (i < n) {
      if (scAt(i)) {
        if (start >= 0) {
          var end = i;
          while (end > start && b[end - 1] === 0) end--; // trailing zero bytes belong to the next start code
          if (end > start) out.push(b.subarray(start, end));
        }
        i += b[i + 2] === 1 ? 3 : 4;
        start = i;
        continue;
      }
      i++;
    }
    if (start >= 0 && start < n) out.push(b.subarray(start, n));
    return out;
  }

  function hex2(v) { return (v < 16 ? '0' : '') + v.toString(16).toUpperCase(); }

  // The decoder config for a key frame's SPS + PPS: codec "avc1.PPCCLL" from the SPS bytes, and
  // the avcC record (version 1, one SPS, one PPS, 4-byte lengths).
  function config(sps, pps) {
    var codec = 'avc1.' + hex2(sps[1]) + hex2(sps[2]) + hex2(sps[3]);
    var d = new Uint8Array(11 + sps.length + pps.length), o = 0;
    d[o++] = 1; d[o++] = sps[1]; d[o++] = sps[2]; d[o++] = sps[3];
    d[o++] = 0xFF; // 4-byte NAL lengths
    d[o++] = 0xE1; // one SPS
    d[o++] = sps.length >> 8; d[o++] = sps.length & 0xFF; d.set(sps, o); o += sps.length;
    d[o++] = 1;    // one PPS
    d[o++] = pps.length >> 8; d[o++] = pps.length & 0xFF; d.set(pps, o);
    return { codec: codec, description: d };
  }

  // One frame as length-prefixed NAL units, without SPS (7), PPS (8) and access-unit
  // delimiters (9): those live in the description.
  function toAvc(units) {
    var keep = units.filter(function (u) { var t = u[0] & 0x1F; return t !== 7 && t !== 8 && t !== 9; });
    var len = 0;
    keep.forEach(function (u) { len += 4 + u.length; });
    var out = new Uint8Array(len), o = 0;
    keep.forEach(function (u) {
      out[o++] = (u.length >>> 24) & 0xFF; out[o++] = (u.length >>> 16) & 0xFF; out[o++] = (u.length >>> 8) & 0xFF; out[o++] = u.length & 0xFF;
      out.set(u, o); o += u.length;
    });
    return out;
  }

  function find(units, type) {
    for (var i = 0; i < units.length; i++) if ((units[i][0] & 0x1F) === type) return units[i];
    return null;
  }

  function same(a, b) {
    if (!a || !b || a.length !== b.length) return false;
    for (var i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
    return true;
  }

  window.mdmH264 = { nals: nals, config: config, toAvc: toAvc, find: find, same: same };
})();
