// Passkey sign-in and registration. The only script on the site.
(function () {
  "use strict";

  function toBytes(s) {
    s = s.replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    return Uint8Array.from(atob(s), function (c) { return c.charCodeAt(0); });
  }
  function toB64(buf) {
    var b = new Uint8Array(buf), s = "";
    for (var i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }
  function fixDescriptors(list) {
    return (list || []).map(function (d) { return Object.assign({}, d, { id: toBytes(d.id) }); });
  }

  var errorEl = document.getElementById("error");
  function showError(msg) {
    errorEl.textContent = msg;
    errorEl.hidden = false;
  }

  async function post(url, body) {
    var res = await fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: body ? { "Content-Type": "application/json" } : {},
      body: body ? JSON.stringify(body) : undefined,
    });
    var data = await res.json().catch(function () { return {}; });
    if (!res.ok) throw new Error(data.error || "Request failed (" + res.status + ")");
    return data;
  }

  async function login(form) {
    var opts = (await post(form.dataset.begin)).publicKey;
    opts.challenge = toBytes(opts.challenge);
    opts.allowCredentials = fixDescriptors(opts.allowCredentials);
    var cred = await navigator.credentials.get({ publicKey: opts });
    var r = cred.response;
    return post(form.dataset.finish, {
      id: cred.id,
      rawId: toB64(cred.rawId),
      type: cred.type,
      response: {
        clientDataJSON: toB64(r.clientDataJSON),
        authenticatorData: toB64(r.authenticatorData),
        signature: toB64(r.signature),
        userHandle: r.userHandle ? toB64(r.userHandle) : null,
      },
    });
  }

  async function register(form) {
    var opts = (await post(form.dataset.begin)).publicKey;
    opts.challenge = toBytes(opts.challenge);
    opts.user.id = toBytes(opts.user.id);
    opts.excludeCredentials = fixDescriptors(opts.excludeCredentials);
    var cred = await navigator.credentials.create({ publicKey: opts });
    var r = cred.response;
    var name = encodeURIComponent(form.elements.name.value.trim());
    return post(form.dataset.finish + "?name=" + name, {
      id: cred.id,
      rawId: toB64(cred.rawId),
      type: cred.type,
      response: {
        clientDataJSON: toB64(r.clientDataJSON),
        attestationObject: toB64(r.attestationObject),
        transports: r.getTransports ? r.getTransports() : [],
      },
    });
  }

  function wire(id, fn) {
    var form = document.getElementById(id);
    if (!form) return;
    if (!window.PublicKeyCredential) {
      showError("This browser does not support passkeys.");
      return;
    }
    form.addEventListener("submit", async function (ev) {
      ev.preventDefault();
      errorEl.hidden = true;
      var button = form.querySelector("button");
      button.disabled = true;
      try {
        var res = await fn(form);
        window.location.assign(res.redirect || "/");
      } catch (e) {
        if (e && e.name === "NotAllowedError") showError("The passkey prompt was cancelled or timed out.");
        else if (e && e.name === "InvalidStateError") showError("This device already has a passkey for this account.");
        else showError((e && e.message) || "Something went wrong.");
        button.disabled = false;
      }
    });
  }

  wire("passkey-login", login);
  wire("passkey-register", register);
})();
