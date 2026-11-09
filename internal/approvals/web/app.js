// Kenaz approval page. Every value from the relay is inserted with
// textContent; nothing is ever parsed as HTML.
"use strict";

(function () {
  const csrf = document.querySelector('meta[name="kenaz-csrf"]').content;
  const $ = (id) => document.getElementById(id);
  const say = (t) => { $("message").textContent = t || ""; };

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

  const statusText = {
    pending: "Waiting for your approval",
    approved: "Approved",
    executing: "Writing…",
    done: "Done",
    denied: "Declined (nothing was written)",
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
    $("r-exact").open = !(v.details || []).length;
    $("r-payload").textContent = v.payload_text || "(content removed)";
    $("r-hash").textContent = v.payload_sha256;
    const open = v.status === "pending";
    $("r-actions").hidden = !open;
    if (v.result) { $("r-result").textContent = "Result: " + JSON.stringify(v.result); $("r-result").hidden = false; }
    $("approve").onclick = async () => {
      $("approve").disabled = true;
      $("deny").disabled = true;
      try {
        const r = await api("POST", "requests/" + encodeURIComponent(id) + "/approve");
        say(statusText[r.status] || r.status);
        showRequest(id);
      } catch (e) {
        say(e.message || "Nothing was written.");
        $("approve").disabled = false;
        $("deny").disabled = false;
      }
    };
    $("deny").onclick = async () => {
      try { await api("POST", "requests/" + encodeURIComponent(id) + "/deny"); say("Declined. Nothing was written."); showRequest(id); }
      catch (e) { say(e.message); }
    };
  }

  // ---- what is waiting ----
  async function showList() {
    $("list").hidden = false;
    try {
      const r = await api("GET", "requests");
      const open = (r.requests || []).filter((q) => q.status === "pending");
      $("l-empty").hidden = open.length > 0;
      for (const q of open) {
        const li = document.createElement("li");
        const a = document.createElement("a");
        a.href = "/approvals/r/" + q.request_id;
        a.textContent = q.operation;
        li.appendChild(a);
        $("l-items").appendChild(li);
      }
    } catch (e) { say(e.message); }
  }

  const m = location.pathname.match(/^\/approvals\/r\/(apr_[a-z2-7]{26})$/);
  if (m) showRequest(m[1]);
  else showList();
})();
