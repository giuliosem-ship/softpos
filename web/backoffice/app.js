// Kerno backoffice (MVP-18): plain JS, no build step, no third-party code.
// Every value from the API is rendered with textContent (never innerHTML).
// The access token lives in memory; the refresh token in sessionStorage
// (cleared when the tab closes). Requires a platform operator account.
(function () {
  "use strict";

  const SCOPE = "kerno:backoffice";
  const $ = (id) => document.getElementById(id);
  let accessToken = null;
  let phone = "";
  let current = null; // merchant id shown in the detail panel
  let payCursor = null;

  function msg(el, text, isError) {
    $(el).textContent = text || "";
    $(el).classList.toggle("error", Boolean(isError));
  }

  function el(tag, text, cls) {
    const e = document.createElement(tag);
    if (text !== undefined && text !== null) e.textContent = String(text);
    if (cls) e.className = cls;
    return e;
  }

  function money(cents) {
    const n = Number(cents || 0);
    const sign = n < 0 ? "-" : "";
    const abs = Math.abs(n);
    return sign + "$" + Math.floor(abs / 100).toLocaleString("es-MX") + "." + String(abs % 100).padStart(2, "0");
  }

  function row(tbody, cells, actions) {
    const tr = document.createElement("tr");
    cells.forEach((c) => tr.appendChild(el("td", c)));
    const td = document.createElement("td");
    (actions || []).forEach((a) => {
      const b = el("button", a.label, a.cls);
      b.type = "button";
      b.addEventListener("click", a.onClick);
      td.appendChild(b);
    });
    tr.appendChild(td);
    tbody.appendChild(tr);
  }

  async function refreshSession() {
    const rt = sessionStorage.getItem("kerno_bo_rt");
    if (!rt) return false;
    const res = await fetch("/api/v1/auth/refresh", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: rt, merchant_id: SCOPE }),
    });
    if (!res.ok) {
      sessionStorage.removeItem("kerno_bo_rt");
      return false;
    }
    const tp = await res.json();
    accessToken = tp.access_token;
    sessionStorage.setItem("kerno_bo_rt", tp.refresh_token);
    return true;
  }

  async function api(method, path, body, raw) {
    const call = () =>
      fetch(path, {
        method,
        headers: Object.assign({ Authorization: "Bearer " + accessToken }, body ? { "Content-Type": "application/json" } : {}),
        body: body ? JSON.stringify(body) : undefined,
      });
    let res = await call();
    if (res.status === 401 && (await refreshSession())) res = await call();
    if (res.status === 401) {
      showLogin();
      throw new Error("Sesión expirada");
    }
    if (raw) return res;
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error((data && (data.error || data.message)) || "HTTP " + res.status);
    return data;
  }

  function showLogin() {
    accessToken = null;
    $("app-view").classList.add("hidden");
    $("logout").classList.add("hidden");
    $("login-view").classList.remove("hidden");
    $("who").textContent = "";
  }

  async function showApp() {
    const me = await api("GET", "/api/v1/backoffice/me");
    $("who").textContent = "Operador " + me.user_id;
    $("login-view").classList.add("hidden");
    $("app-view").classList.remove("hidden");
    $("logout").classList.remove("hidden");
    showTab("merchants");
    loadMerchants();
  }

  // ---- Login (OTP) ----
  $("otp-request").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    phone = $("phone").value.trim();
    const res = await fetch("/api/v1/auth/otp/request", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ phone }),
    });
    if (res.status === 202) {
      $("otp-verify").classList.remove("hidden");
      msg("login-msg", "Código enviado.");
    } else {
      const d = await res.json().catch(() => ({}));
      msg("login-msg", d.error || "No se pudo enviar el código", true);
    }
  });

  $("otp-verify").addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const res = await fetch("/api/v1/auth/otp/verify", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ phone, code: $("code").value.trim(), merchant_id: SCOPE }),
    });
    const d = await res.json().catch(() => ({}));
    if (!res.ok || d.role !== "operator") {
      msg("login-msg", d.error === "NOT_A_MERCHANT_MEMBER" ? "Este teléfono no es operador." : d.error || "Código inválido", true);
      return;
    }
    accessToken = d.access_token;
    sessionStorage.setItem("kerno_bo_rt", d.refresh_token);
    $("code").value = "";
    msg("login-msg", "");
    showApp().catch((e) => msg("login-msg", e.message, true));
  });

  $("logout").addEventListener("click", async () => {
    const rt = sessionStorage.getItem("kerno_bo_rt");
    sessionStorage.removeItem("kerno_bo_rt");
    if (rt) {
      await fetch("/api/v1/auth/logout", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ refresh_token: rt }) });
    }
    showLogin();
  });

  // ---- Tabs ----
  function showTab(name) {
    document.querySelectorAll(".tab").forEach((t) => t.classList.add("hidden"));
    $("tab-" + name).classList.remove("hidden");
    msg("app-msg", "");
  }
  document.querySelectorAll("nav button").forEach((b) =>
    b.addEventListener("click", () => {
      showTab(b.dataset.tab);
      if (b.dataset.tab === "payouts") loadPayouts();
      if (b.dataset.tab === "recon") loadRecon();
      if (b.dataset.tab === "audit") loadAudit();
    })
  );

  const fail = (e) => msg("app-msg", e.message, true);

  // ---- Merchants / KYB ----
  async function loadMerchants() {
    const q = new URLSearchParams();
    if ($("kyb-status").value) q.set("kyb_status", $("kyb-status").value);
    if ($("blocked").value) q.set("blocked", $("blocked").value);
    const data = await api("GET", "/api/v1/backoffice/merchants?" + q);
    const tbody = $("merchant-table").querySelector("tbody");
    tbody.replaceChildren();
    data.items.forEach((m) =>
      row(tbody, [m.merchant_id, m.name, m.rfc, m.kyb_status + (m.reason ? " (" + m.reason + ")" : ""), m.list_69b, m.identity_check, m.blocked ? "sí" : "no"], [
        { label: "Abrir", onClick: () => openMerchant(m.merchant_id).catch(fail) },
      ])
    );
    if (!data.items.length) msg("app-msg", "Sin resultados.");
  }
  $("merchant-filter").addEventListener("submit", (ev) => {
    ev.preventDefault();
    loadMerchants().catch(fail);
  });

  async function openMerchant(id) {
    const d = await api("GET", "/api/v1/backoffice/merchants/" + encodeURIComponent(id));
    current = id;
    $("merchant-detail").classList.remove("hidden");
    $("ledger-view").replaceChildren();
    $("detail-title").textContent = id + " · " + d.kyb.status + (d.kyb.blocked ? " · BLOQUEADO" : "");
    const a = d.kyb.application || {};
    const ad = a.address || {};
    const fields = [
      ["Razón social", a.legal_name], ["RFC", a.rfc], ["CURP", a.curp], ["Régimen", a.tax_regime], ["MCC", a.mcc],
      ["Domicilio", [ad.street, ad.ext_number, ad.int_number, ad.neighborhood, ad.city, ad.state, ad.postal_code].filter(Boolean).join(", ")],
      ["Contacto", [a.contact_email, a.contact_phone].filter(Boolean).join(" · ")],
      ["RFC válido", d.kyb.checks.rfc_valid ? "sí" : "no"], ["69-B", d.kyb.checks.list_69b],
      ["Documentos completos", d.kyb.checks.documents_complete ? "sí" : "no"],
      ["Verificación de identidad", d.kyb.identity_check + (d.kyb.identity_detail ? " — " + d.kyb.identity_detail : "") + (d.kyb.identity_reference ? " [" + d.kyb.identity_reference + "]" : "")],
      ["Motivo", d.kyb.reason], ["Enviado", d.kyb.submitted_at], ["Decidido", [d.kyb.decided_at, d.kyb.decided_by].filter(Boolean).join(" por ")],
      ["Sub-afiliado procesador", d.kyb.acquirer_merchant_id], ["CLABE virtual", d.merchant.clabe],
      ["Cuenta de liquidación", d.settlement_account ? d.settlement_account.clabe + " (" + d.settlement_account.status + ")" : ""],
      ["Bloqueo", d.kyb.blocked ? d.kyb.blocked_reason + " (" + (d.kyb.blocked_source || "OPERATOR") + ", " + d.kyb.blocked_at + ")" : "no"],
    ];
    const dl = $("detail-fields");
    dl.replaceChildren();
    fields.forEach(([k, v]) => {
      dl.appendChild(el("dt", k));
      dl.appendChild(el("dd", v || "—"));
    });
    const ul = $("detail-docs");
    ul.replaceChildren();
    d.documents.forEach((doc) => {
      const li = el("li", doc.type + " · " + doc.content_type + " · " + Math.round(doc.size_bytes / 1024) + " KiB · " + doc.status + " · " + doc.created_at + " ");
      const b = el("button", "Descargar");
      b.type = "button";
      b.addEventListener("click", () => downloadDoc(id, doc).catch(fail));
      li.appendChild(b);
      ul.appendChild(li);
    });
  }

  async function downloadDoc(id, doc) {
    const res = await api("GET", "/api/v1/backoffice/merchants/" + encodeURIComponent(id) + "/documents/" + encodeURIComponent(doc.id), null, true);
    if (!res.ok) throw new Error("No se pudo descargar (" + res.status + ")");
    const url = URL.createObjectURL(await res.blob());
    const a = document.createElement("a");
    a.href = url;
    a.download = doc.type + "_" + doc.id;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 30000);
  }

  async function decide(action, needsReason) {
    if (!current) return;
    const text = $("decision-text").value.trim();
    if (needsReason && !text) {
      msg("app-msg", "Escribe el motivo.", true);
      return;
    }
    if (!window.confirm("¿Confirmas " + action + " para " + current + "?")) return;
    const body = action === "approve" ? { note: text } : { reason: text };
    const path = action === "approve" || action === "reject" ? "/kyb/" + action : "/" + action;
    await api("POST", "/api/v1/backoffice/merchants/" + encodeURIComponent(current) + path, body);
    $("decision-text").value = "";
    msg("app-msg", "Hecho: " + action);
    await openMerchant(current);
    await loadMerchants();
  }
  $("btn-approve").addEventListener("click", () => decide("approve", false).catch(fail));
  $("btn-reject").addEventListener("click", () => decide("reject", true).catch(fail));
  $("btn-block").addEventListener("click", () => decide("block", true).catch(fail));
  $("btn-unblock").addEventListener("click", () => decide("unblock", false).catch(fail));

  $("btn-ledger").addEventListener("click", async () => {
    try {
      const d = await api("GET", "/api/v1/backoffice/merchants/" + encodeURIComponent(current) + "/ledger");
      const box = $("ledger-view");
      box.replaceChildren();
      box.appendChild(el("p", "Disponible " + money(d.balances.available_cents) + " · Apartado fiscal " + money(d.balances.tax_escrow_cents) + " · RBF " + money(d.balances.rbf_debt_cents)));
      const table = document.createElement("table");
      const tb = document.createElement("tbody");
      d.entries.forEach((e) => row(tb, [e.created_at, e.journal_id, e.account_id.split(":").slice(-1)[0], e.direction, money(e.amount_cents), e.description]));
      table.appendChild(tb);
      box.appendChild(table);
    } catch (e) {
      fail(e);
    }
  });

  // ---- Payments ----
  async function loadPayments(append) {
    const q = new URLSearchParams({ limit: "100" });
    [["merchant_id", "pay-merchant"], ["id", "pay-id"], ["kind", "pay-kind"], ["from", "pay-from"], ["to", "pay-to"]].forEach(([k, id]) => {
      const v = $(id).value.trim();
      if (v) q.set(k, v);
    });
    if (append && payCursor) q.set("before", payCursor);
    const d = await api("GET", "/api/v1/backoffice/payments?" + q);
    const tbody = $("payment-table").querySelector("tbody");
    if (!append) tbody.replaceChildren();
    d.items.forEach((p) => row(tbody, [p.created_at, p.merchant_id, p.kind, p.id, p.status, money(p.amount_cents)]));
    payCursor = d.next_cursor;
    $("pay-more").classList.toggle("hidden", !payCursor);
  }
  $("payment-filter").addEventListener("submit", (ev) => {
    ev.preventDefault();
    loadPayments(false).catch(fail);
  });
  $("pay-more").addEventListener("click", () => loadPayments(true).catch(fail));

  // ---- Payouts ----
  async function loadPayouts() {
    const q = new URLSearchParams();
    if ($("po-merchant").value.trim()) q.set("merchant_id", $("po-merchant").value.trim());
    if ($("po-status").value) q.set("status", $("po-status").value);
    const d = await api("GET", "/api/v1/backoffice/payouts?" + q);
    const tbody = $("payout-table").querySelector("tbody");
    tbody.replaceChildren();
    d.items.forEach((p) => {
      const actions = p.status === "PENDING_RECONCILIATION" || p.status === "RESERVED"
        ? [{ label: "Reintentar conciliación", onClick: () => retryPayout(p.clave_rastreo).catch(fail) }]
        : [];
      row(tbody, [p.clave_rastreo, p.merchant_id, money(p.amount_cents), p.status, p.last_error, p.updated_at], actions);
    });
  }
  $("payout-filter").addEventListener("submit", (ev) => {
    ev.preventDefault();
    loadPayouts().catch(fail);
  });
  async function retryPayout(clave) {
    if (!window.confirm("¿Consultar STP y resolver el payout " + clave + "?")) return;
    const d = await api("POST", "/api/v1/backoffice/payouts/" + encodeURIComponent(clave) + "/retry");
    msg("app-msg", "Resultado: " + d.outcome + " → " + d.payout.status);
    await loadPayouts();
  }

  // ---- Reconciliation ----
  async function loadRecon() {
    const d = await api("GET", "/api/v1/backoffice/reconciliation?status=OPEN");
    const tbody = $("recon-table").querySelector("tbody");
    tbody.replaceChildren();
    d.items.forEach((it) =>
      row(tbody, [it.id, it.kind, it.payout_id, it.card_payment_id || it.provider_payment_id, money(it.expected_cents), money(it.actual_cents), it.detail, it.status], [
        { label: "Resolver", onClick: () => resolveRecon(it.id).catch(fail) },
      ])
    );
  }
  $("recon-load").addEventListener("click", () => loadRecon().catch(fail));
  async function resolveRecon(id) {
    const note = window.prompt("Nota de resolución");
    if (!note) return;
    await api("POST", "/api/v1/backoffice/reconciliation/" + encodeURIComponent(id) + "/resolve", { note });
    await loadRecon();
  }

  // ---- Audit ----
  async function loadAudit() {
    const q = new URLSearchParams();
    if ($("audit-merchant").value.trim()) q.set("merchant_id", $("audit-merchant").value.trim());
    const d = await api("GET", "/api/v1/backoffice/audit-log?" + q);
    const tbody = $("audit-table").querySelector("tbody");
    tbody.replaceChildren();
    d.items.forEach((e) =>
      row(tbody, [e.created_at, e.operator_id, e.action, e.merchant_id, e.target, (e.before ? JSON.stringify(e.before) : "") + (e.after ? " → " + JSON.stringify(e.after) : ""), e.detail])
    );
  }
  $("audit-filter").addEventListener("submit", (ev) => {
    ev.preventDefault();
    loadAudit().catch(fail);
  });

  // Resume a session of this tab, if any.
  refreshSession().then((ok) => (ok ? showApp() : showLogin())).catch(showLogin);
})();
