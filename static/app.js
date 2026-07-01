// Browser-side registry client. When a registry serves CORS headers (e.g. a self-hosted
// registry:2/:3 configured with Access-Control-* + an exposed WWW-Authenticate), the
// browser resolves manifests itself and posts the result to the server, offloading the
// server's outbound registry traffic. Any failure (CORS, network, auth) is caught and the
// request proceeds without resolved data, so the server fetches as usual — full fallback.
(function () {
  "use strict";

  var DOCKER_HUB = "registry-1.docker.io";
  var ACCEPT = [
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
    "application/vnd.oci.image.manifest.v1+json",
  ].join(", ");
  var OUR_PATHS = ["/add", "/remove", "/compare"];
  var MAX_IMAGES = window.DIC_MAX_IMAGES || 10;

  // parseRef mirrors the server's reference parsing (Docker Hub defaults included).
  function parseRef(s) {
    s = (s || "").trim();
    var name = s, ref = "";
    var at = name.indexOf("@");
    if (at >= 0) {
      ref = name.slice(at + 1);
      name = name.slice(0, at);
    } else {
      var colon = name.lastIndexOf(":");
      if (colon >= 0 && name.lastIndexOf("/") < colon) {
        ref = name.slice(colon + 1);
        name = name.slice(0, colon);
      }
    }
    if (!ref) ref = "latest";

    var registry = DOCKER_HUB, repo = name;
    var slash = name.indexOf("/");
    if (slash >= 0) {
      var first = name.slice(0, slash);
      if (first.indexOf(".") >= 0 || first.indexOf(":") >= 0 || first === "localhost") {
        registry = first;
        repo = name.slice(slash + 1);
      }
    }
    if (registry === DOCKER_HUB && repo.indexOf("/") < 0) repo = "library/" + repo;
    // Repository names must be lowercase (tags stay as-is — they're case-sensitive).
    return { registry: registry.toLowerCase(), repo: repo.toLowerCase(), ref: ref };
  }

  // normalizeName lowercases the registry+repository part, leaving the tag/digest untouched.
  function normalizeName(s) {
    s = (s || "").trim();
    var at = s.indexOf("@");
    if (at >= 0) return s.slice(0, at).toLowerCase() + s.slice(at);
    var colon = s.lastIndexOf(":");
    if (colon >= 0 && s.lastIndexOf("/") < colon) return s.slice(0, colon).toLowerCase() + s.slice(colon);
    return s.toLowerCase();
  }

  function platStr(os, arch, variant) {
    var p = (os || "") + "/" + (arch || "");
    return variant ? p + "/" + variant : p;
  }

  // parseChallenge extracts realm/service from a Bearer WWW-Authenticate header.
  function parseChallenge(h) {
    var out = { realm: "", service: "" };
    if (!h || h.toLowerCase().indexOf("bearer ") !== 0) return out;
    var rest = h.slice(7);
    var re = /(\w+)="([^"]*)"/g, m;
    while ((m = re.exec(rest))) {
      if (m[1] === "realm") out.realm = m[2];
      else if (m[1] === "service") out.service = m[2];
    }
    return out;
  }

  async function getToken(wwwAuth, repo) {
    var ch = parseChallenge(wwwAuth || "");
    if (!ch.realm) return "";
    var u = new URL(ch.realm);
    if (ch.service) u.searchParams.set("service", ch.service);
    u.searchParams.set("scope", "repository:" + repo + ":pull");
    var resp = await fetch(u.toString());
    if (!resp.ok) throw new Error("token " + resp.status);
    var j = await resp.json();
    return j.token || j.access_token || "";
  }

  function manifestURL(ref, reference) {
    return "https://" + ref.registry + "/v2/" + ref.repo + "/manifests/" + encodeURIComponent(reference);
  }

  async function fetchManifest(ref, reference, token) {
    var headers = { Accept: ACCEPT };
    if (token) headers.Authorization = "Bearer " + token;
    var resp = await fetch(manifestURL(ref, reference), { headers: headers });
    if (!resp.ok) throw new Error("manifest " + resp.status);
    return resp;
  }

  // getManifest fetches the top manifest, performing the 401 -> token -> retry handshake.
  async function getManifest(ref, reference) {
    var resp = await fetch(manifestURL(ref, reference), { headers: { Accept: ACCEPT } });
    var token = "";
    if (resp.status === 401) {
      token = await getToken(resp.headers.get("WWW-Authenticate"), ref.repo);
      resp = await fetchManifest(ref, reference, token);
    } else if (!resp.ok) {
      throw new Error("manifest " + resp.status);
    }
    return { json: await resp.json(), contentType: resp.headers.get("Content-Type") || "", token: token };
  }

  async function fetchConfig(ref, digest, token) {
    if (!digest) return { os: "linux", arch: "amd64", variant: "", created: "" };
    var headers = {};
    if (token) headers.Authorization = "Bearer " + token;
    var resp = await fetch("https://" + ref.registry + "/v2/" + ref.repo + "/blobs/" + digest, { headers: headers });
    if (!resp.ok) throw new Error("config " + resp.status);
    var j = await resp.json();
    return { os: j.os, arch: j.architecture, variant: j.variant || "", created: j.created || "" };
  }

  function isIndex(ct, m) {
    if (ct.indexOf("manifest.list") >= 0 || ct.indexOf("image.index") >= 0) return true;
    return Array.isArray(m.manifests) && m.manifests.length > 0 && !(m.layers && m.layers.length);
  }

  // resolveImage resolves an image's available platforms (and, for single-arch images,
  // its layers). Layers for a chosen index platform are fetched later by layersFor.
  async function resolveImage(name) {
    var ref = parseRef(name);
    var top = await getManifest(ref, ref.ref);
    if (isIndex(top.contentType, top.json)) {
      var platforms = [], digestByPlat = {};
      (top.json.manifests || []).forEach(function (e) {
        var p = e.platform || {};
        if (!p.os || p.os === "unknown" || p.architecture === "unknown") return;
        var ps = platStr(p.os, p.architecture, p.variant);
        if (!(ps in digestByPlat)) {
          digestByPlat[ps] = e.digest;
          platforms.push(ps);
        }
      });
      if (!platforms.length) throw new Error("no runnable platforms");
      return { ref: ref, token: top.token, kind: "index", platforms: platforms, digestByPlat: digestByPlat };
    }
    if (!top.json.layers) throw new Error("unsupported manifest");
    var cp = await fetchConfig(ref, (top.json.config || {}).digest, top.token);
    return {
      ref: ref, token: top.token, kind: "single",
      platforms: [platStr(cp.os, cp.arch, cp.variant)],
      layers: top.json.layers.map(function (l) { return { digest: l.digest, size: l.size }; }),
      created: cp.created,
    };
  }

  // layersFor returns { layers, created } for the chosen platform.
  async function layersFor(img, chosen) {
    if (img.kind === "single") {
      return img.platforms[0] === chosen
        ? { layers: img.layers, created: img.created || "" }
        : { layers: [], created: "" };
    }
    var digest = img.digestByPlat[chosen];
    if (!digest) return { layers: [], created: "" };
    var resp = await fetchManifest(img.ref, digest, img.token);
    var m = await resp.json();
    var cfg = await fetchConfig(img.ref, (m.config || {}).digest, img.token);
    return {
      layers: (m.layers || []).map(function (l) { return { digest: l.digest, size: l.size }; }),
      created: cfg.created,
    };
  }

  function pickPlatform(common, selected) {
    if (selected && common.indexOf(selected) >= 0) return selected;
    if (common.indexOf("linux/amd64") >= 0) return "linux/amd64";
    return common[0];
  }

  // currentImageNames = existing cards + the textarea's lines, normalized, deduped, capped.
  function currentImageNames() {
    var seen = {}, names = [];
    function add(v) {
      var n = normalizeName(v);
      if (n && !seen[n]) {
        seen[n] = true;
        names.push(n);
      }
    }
    document.querySelectorAll('#app input[name="image"]').forEach(function (i) { add(i.value); });
    var ni = document.getElementById("new-image-input");
    if (ni) ni.value.split("\n").forEach(add);
    return names.slice(0, MAX_IMAGES); // silently capped at the max
  }

  function selectedPlatform() {
    var el = document.getElementById("dic-platform");
    return el ? el.value : "";
  }

  // resolveState resolves every current image in the browser, mirroring the server's
  // platform-selection logic so the chosen platform matches. Throws on any failure.
  async function resolveState() {
    var names = currentImageNames();
    if (!names.length) return null;
    var selected = selectedPlatform();

    var imgs = await Promise.all(names.map(resolveImage));

    var common = imgs[0].platforms.slice();
    for (var i = 1; i < imgs.length; i++) {
      var set = new Set(imgs[i].platforms);
      common = common.filter(function (p) { return set.has(p); });
    }
    var chosen = common.length ? pickPlatform(common, selected) : "";

    return Promise.all(imgs.map(async function (img, idx) {
      var info = chosen ? await layersFor(img, chosen) : { layers: [], created: "" };
      return {
        name: names[idx],
        platforms: img.platforms,
        platform: chosen,
        layers: info.layers,
        created: info.created,
      };
    }));
  }

  function showAddError(msg) {
    var el = document.getElementById("add-error");
    if (!el) return;
    el.textContent = msg;
    el.hidden = false;
  }

  function clearAddError() {
    var el = document.getElementById("add-error");
    if (el) {
      el.textContent = "";
      el.hidden = true;
    }
  }

  function setAddLoading(on) {
    var btn = document.getElementById("add-submit");
    if (!btn) return;
    btn.classList.toggle("is-loading", on);
    btn.disabled = on;
  }

  // Intercept our htmx requests, resolve in the browser when possible, and inject the
  // result into the request. On any failure, proceed without it (server-side fallback).
  document.addEventListener("htmx:confirm", function (evt) {
    var path = evt.detail.path || "";
    if (OUR_PATHS.indexOf(path) < 0) return;

    // Validate the add-modal input before issuing the request. Duplicates are skipped
    // server-side (no error); we only guard against an entirely empty submission.
    if (path === "/add") {
      var ni = document.getElementById("new-image-input");
      var hasContent = !!ni && ni.value.split("\n").some(function (l) { return l.trim() !== ""; });
      if (!hasContent) {
        evt.preventDefault();
        showAddError("Enter an image name.");
        return;
      }
      clearAddError();
      setAddLoading(true); // covers browser resolution + the server round-trip
    }

    evt.preventDefault();
    (async function () {
      var field = document.getElementById("dic-resolved");
      try {
        var resolved = await resolveState();
        if (field) field.value = resolved ? JSON.stringify(resolved) : "";
      } catch (e) {
        if (field) field.value = ""; // fall back to server-side fetch
      }
      evt.detail.issueRequest(true);
    })();
  });

  // Modal wiring (static elements present at load): Enter submits, typing clears the
  // error, and closing resets it.
  var input = document.getElementById("new-image-input");
  if (input) {
    // In a textarea plain Enter inserts a newline; ⌘/Ctrl+Enter submits.
    input.addEventListener("keydown", function (e) {
      if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        var btn = document.getElementById("add-submit");
        if (btn) btn.click();
      }
    });
    input.addEventListener("input", clearAddError);
  }
  var dialog = document.getElementById("add-dialog");
  if (dialog) {
    dialog.addEventListener("close", function () {
      clearAddError();
      setAddLoading(false);
    });
  }

  // Clear the Add button's loading state once its request completes (success closes the
  // dialog via hx-on::after-request; on error the modal stays open, restored and editable).
  document.body.addEventListener("htmx:afterRequest", function (e) {
    if (e.detail && e.detail.elt && e.detail.elt.id === "add-submit") {
      setAddLoading(false);
    }
  });
})();
