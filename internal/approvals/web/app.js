// Kenaz approval page. Every value from the relay is inserted with
// textContent; nothing is ever parsed as HTML.
"use strict";

(function () {
  const csrf = document.querySelector('meta[name="kenaz-csrf"]').content;
  const $ = (id) => document.getElementById(id);
  const say = (t) => { $("message").textContent = t || ""; };

  function b64uToBuf(s) {
    s = s.replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    const bin = atob(s);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out.buffer;
  }
  function bufToB64u(buf) {
    const b = new Uint8Array(buf);
    let s = "";
    for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  async function api(method, path, body) {
    const opt = { method: method, headers: {}, credentials: "same-origin", cache: "no-store" };
    if (method !== "GET") {
      opt.headers["Content-Type"] = "application/json";
      opt.headers["X-Kenaz-CSRF"] = csrf;
      opt.body = JSON.stringify(body || {});
    }
    const res = await fetch("/approvals/api/" + path, opt);
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      const e = new Error(data.message || ("Request failed (" + res.status + ")"));
      e.code = data.code;
      throw e;
    }
    return data;
  }

  function webauthnAvailable() {
    return typeof window.PublicKeyCredential === "function" && navigator.credentials;
  }

  async function getAssertion(opts) {
    const pk = opts.publicKey;
    pk.challenge = b64uToBuf(pk.challenge);
    (pk.allowCredentials || []).forEach((c) => { c.id = b64uToBuf(c.id); });
    const cred = await navigator.credentials.get({ publicKey: pk });
    return {
      id: cred.id,
      rawId: bufToB64u(cred.rawId),
      type: cred.type,
      response: {
        clientDataJSON: bufToB64u(cred.response.clientDataJSON),
        authenticatorData: bufToB64u(cred.response.authenticatorData),
        signature: bufToB64u(cred.response.signature),
        userHandle: cred.response.userHandle ? bufToB64u(cred.response.userHandle) : undefined,
      },
      clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
    };
  }

  async function createCredential(opts) {
    const pk = opts.publicKey;
    pk.challenge = b64uToBuf(pk.challenge);
    pk.user.id = b64uToBuf(pk.user.id);
    (pk.excludeCredentials || []).forEach((c) => { c.id = b64uToBuf(c.id); });
    const cred = await navigator.credentials.create({ publicKey: pk });
    return {
      id: cred.id,
      rawId: bufToB64u(cred.rawId),
      type: cred.type,
      response: {
        clientDataJSON: bufToB64u(cred.response.clientDataJSON),
        attestationObject: bufToB64u(cred.response.attestationObject),
        transports: cred.response.getTransports ? cred.response.getTransports() : [],
      },
      clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
    };
  }

  const statusText = {
    pending: "Waiting for your approval",
    approved: "Approved",
    executing: "Writing…",
    done: "Done",
    denied: "Canceled by you",
    expired: "Expired (nothing was written)",
    canceled: "Canceled (nothing was written)",
    reconciling: "Checking whether the write completed. It will not be repeated blindly.",
    failed: "The destination refused the write",
  };

  // ---- details: each item with a preview and its full text ----
  const previewChars = 280;
  function showDetails(items) {
    const list = $("r-details");
    list.replaceChildren();
    list.hidden = items.length === 0;
    for (const d of items) {
      const li = document.createElement("li");
      const label = document.createElement("div");
      label.className = "detail-label";
      label.textContent = d.label + (d.target ? " (" + d.target + ")" : "");
      li.appendChild(label);
      const text = d.text || "";
      if (text) {
        const body = document.createElement("pre");
        body.className = "detail-text";
        const long = text.length > previewChars;
        body.textContent = long ? text.slice(0, previewChars) + "…" : text;
        li.appendChild(body);
        if (long) {
          const more = document.createElement("button");
          more.type = "button";
          more.className = "secondary detail-more";
          more.textContent = "Show full text";
          more.onclick = () => {
            const full = more.dataset.full === "1";
            body.textContent = full ? text.slice(0, previewChars) + "…" : text;
            more.dataset.full = full ? "0" : "1";
            more.textContent = full ? "Show full text" : "Show less";
          };
          li.appendChild(more);
        }
      }
      list.appendChild(li);
    }
  }

  // ---- one request ----
  async function showRequest(id) {
    $("request").hidden = false;
    let v;
    try {
      v = await api("GET", "requests/" + encodeURIComponent(id));
    } catch (e) {
      $("request").hidden = true;
      say(e.message);
      return;
    }
    $("r-op").textContent = v.operation;
    $("r-status").textContent = statusText[v.status] || v.status;
    $("r-account").textContent = v.env + (v.org_id ? " / organization " + v.org_id : "");
    $("r-targets").textContent = (v.targets || []).join(", ") || "-";
    $("r-expires").textContent = new Date(v.expires_at).toLocaleTimeString();
    $("r-summary").textContent = v.summary || "";
    if (v.impact) { $("r-impact").textContent = v.impact; $("r-impact").hidden = false; }
    showDetails(v.details || []);
    $("r-payload").textContent = v.payload_text || "(content removed)";
    $("r-hash").textContent = v.payload_sha256;
    const open = v.status === "pending";
    $("r-actions").hidden = !open;
    if (v.result) { $("r-result").textContent = "Result: " + JSON.stringify(v.result); $("r-result").hidden = false; }
    if (open && !v.enrolled) {
      $("approve").disabled = true;
      say(v.can_enroll ? "Set up an authenticator first (below), then reload this page." : "approval_unavailable: this build cannot set up an authenticator, so this request cannot be approved.");
      if (v.can_enroll) showEnroll();
    }
    $("approve").onclick = async () => {
      if (!webauthnAvailable()) { say("approval_unavailable: this browser has no WebAuthn support."); return; }
      $("approve").disabled = true;
      try {
        const opts = await api("POST", "requests/" + encodeURIComponent(id) + "/begin");
        const assertion = await getAssertion(opts);
        const r = await api("POST", "requests/" + encodeURIComponent(id) + "/finish", assertion);
        say(statusText[r.status] || r.status);
        showRequest(id);
      } catch (e) {
        say(e.message || "Approval was not completed. Nothing was written.");
        $("approve").disabled = false;
      }
    };
    $("deny").onclick = async () => {
      try { await api("POST", "requests/" + encodeURIComponent(id) + "/deny"); say("Canceled. Nothing was written."); showRequest(id); }
      catch (e) { say(e.message); }
    };
  }

  // ---- enrollment ----
  // Sign-in for setup happens in a new tab. The identity provider sends that
  // tab back here with a one-time link, which binds the ceremony to this
  // browser (an HttpOnly cookie); registration then continues in that tab.
  async function register(eid) {
    say("Now use your security key or device unlock.");
    const copts = await api("POST", "enroll/" + eid + "/register/begin");
    await api("POST", "enroll/" + eid + "/register/finish", await createCredential(copts));
    say("Authenticator set up. You can approve requests now.");
    showEnroll();
  }

  async function runEnrollment(mode) {
    if (!webauthnAvailable()) { say("approval_unavailable: this browser has no WebAuthn support."); return; }
    try {
      let st = await api("POST", "enroll", { mode: mode });
      const eid = st.enroll_id;
      if (st.needs_assertion) {
        say("Confirm with an authenticator you already set up.");
        const opts = await api("POST", "enroll/" + eid + "/assert/begin");
        await api("POST", "enroll/" + eid + "/assert/finish", await getAssertion(opts));
        await register(eid);
        return;
      }
      say("Confirm your Fleet account to continue.");
      for (;;) {
        st = await api("GET", "enroll/" + eid);
        if (st.error) { say(st.error === "account_mismatch" ? "That sign-in is a different account from the one Kenaz is signed in with." : "Identity verification did not complete."); return; }
        if (st.signin_ready) { $("e-signin-link").href = "/approvals/enroll/signin/" + eid; $("e-signin").hidden = false; }
        if (st.browser_bound) { $("e-signin").hidden = true; say("Signed in. Finish setup in the tab that opened after sign-in."); return; }
        await new Promise((r) => setTimeout(r, 1500));
      }
    } catch (e) {
      say(e.message || "Setup was not completed.");
    }
  }

  async function resumeEnrollment(eid) {
    if (!webauthnAvailable()) { say("approval_unavailable: this browser has no WebAuthn support."); return; }
    try { await register(eid); } catch (e) { say(e.message || "Setup was not completed."); }
  }

  async function showEnroll() {
    $("enroll").hidden = false;
    try {
      const info = await api("GET", "enroll");
      $("e-state").textContent = info.enrolled ? "An authenticator is set up for this account." : (info.can_enroll ? "No authenticator is set up yet." : "approval_unavailable: this build cannot set up an authenticator.");
      $("e-start").hidden = info.enrolled || !info.can_enroll;
      $("e-add").hidden = !info.enrolled;
      $("e-recover").hidden = !info.enrolled || !info.can_enroll;
    } catch (e) { say(e.message); }
    $("e-start").onclick = () => runEnrollment("first");
    $("e-add").onclick = () => runEnrollment("add");
    $("e-recover").onclick = () => runEnrollment("recover");
  }

  const m = location.pathname.match(/^\/approvals\/r\/(apr_[a-z2-7]{26})$/);
  const e = new URLSearchParams(location.search).get("e");
  if (m) showRequest(m[1]);
  else {
    showEnroll();
    if (location.pathname === "/approvals/enroll" && e && /^enr_[a-z2-7]{26}$/.test(e)) {
      $("e-start").hidden = true;
      const b = document.createElement("button");
      b.type = "button";
      b.textContent = "Finish setup with your security key or device unlock";
      b.onclick = () => resumeEnrollment(e);
      $("enroll").appendChild(b);
    }
  }
})();
