"use strict";
// Cài đặt model & nhà cung cấp. Dùng chung tiện ích ($, el, api, toast…) từ app.js.

const ROLE_LABELS = [
  ["architect", "Kiến trúc sư", "dựng tiền đề, nhân vật, dàn ý"],
  ["writer", "Người viết", "viết chương + trợ lý chat"],
  ["editor", "Biên tập", "duyệt, chấm điểm chương"],
];

const ST = { data: null, editing: null, catalog: [] };
const sdlg = $("#settingsDialog");

document.querySelectorAll(".btn-settings").forEach((b) => (b.onclick = openSettings));
$("#btnSettingsClose").onclick = () => sdlg.close();

async function openSettings() {
  $("#providerForm").classList.add("hidden");
  $("#creditInfo").textContent = "Bấm để kiểm tra số dư và hạn mức của key.";
  if (!sdlg.open) sdlg.showModal();
  await loadSettings();
}

async function loadSettings() {
  try { ST.data = await api("api/settings"); } catch (e) { toast(e.message, "error"); return; }
  const d = ST.data;
  $("#creditBox").classList.toggle("hidden", !d.providers.some((p) => p.name === "openrouter" && p.has_key));
  $("#configPath").textContent = "File cấu hình: " + d.config_path;
  $("#budgetUsd").value = d.budget_usd || 0;
  renderRoles();
  renderProviders();
}

function modelOptions(includeDefault) {
  const opts = [];
  if (includeDefault) opts.push(el("option", { value: "" }, "— Theo model mặc định —"));
  for (const p of ST.data.providers) {
    const group = el("optgroup", { label: p.name + (p.has_key || !p.requires_key ? "" : " (thiếu API key)") });
    for (const m of p.models) group.append(el("option", { value: JSON.stringify({ provider: p.name, model: m }) }, m));
    if (p.models.length) opts.push(group);
  }
  return opts;
}

function renderRoles() {
  const d = ST.data, grid = $("#roleGrid");
  grid.replaceChildren();
  const def = el("select", { id: "roleDefault" }, modelOptions(false));
  def.value = JSON.stringify({ provider: d.default.provider, model: d.default.model });
  grid.append(el("label", { for: "roleDefault" }, "Mặc định", el("small", {}, "dùng cho mọi việc không chỉ định riêng")), def);
  for (const [role, label, desc] of ROLE_LABELS) {
    const sel = el("select", { id: "role-" + role, "data-role": role }, modelOptions(true));
    const cur = d.roles[role];
    sel.value = cur ? JSON.stringify({ provider: cur.provider, model: cur.model }) : "";
    grid.append(el("label", { for: "role-" + role }, label, el("small", {}, desc)), sel);
  }
}

$("#btnSaveModels").onclick = async () => {
  const roles = {};
  document.querySelectorAll("#roleGrid select[data-role]").forEach((s) => (roles[s.dataset.role] = s.value ? JSON.parse(s.value) : null));
  const body = { default: JSON.parse($("#roleDefault").value), roles, budget_usd: Number($("#budgetUsd").value) || 0 };
  try {
    const r = await api("api/settings/models", { method: "PUT", body });
    toast(r.running ? `Đã lưu. ${r.running} truyện đang chạy sẽ đổi model sau khi dừng.` : "Đã lưu lựa chọn model");
    await loadSettings();
    if (S.bookId) loadState();
  } catch (e) { toast(e.message, "error"); }
};

// ---- Số dư OpenRouter ----
$("#btnCredit").onclick = async () => {
  const box = $("#creditInfo");
  box.textContent = "Đang hỏi OpenRouter…";
  try {
    const c = await api("api/settings/openrouter");
    const money = (v) => (v === null || v === undefined ? "không giới hạn" : (v < 0 ? "-$" : "$") + Math.abs(Number(v)).toFixed(4));
    const grid = el("div", { class: "credit-grid" },
      el("span", {}, "Đã nạp"), el("b", {}, money(c.total_credits)),
      el("span", {}, "Đã dùng"), el("b", {}, money(c.total_usage)),
      el("span", {}, "Còn lại"), el("b", { class: c.balance <= 0 ? "neg" : "" }, money(c.balance)),
      el("span", {}, "Hạn mức của key"), el("b", {}, money(c.key_limit)));
    box.replaceChildren(grid);
    if (c.balance <= 0) {
      box.append(el("p", { class: "hint err" },
        "Tài khoản chưa có credit nên OpenRouter chỉ cho các request rất nhỏ (lỗi 402 khi engine chạy). Nạp credit tại openrouter.ai/settings/credits, hoặc chọn model có đuôi “:free”."));
    }
  } catch (e) { box.textContent = "Lỗi: " + e.message; }
};

// ---- Nhà cung cấp ----
function renderProviders() {
  const list = $("#providerList");
  list.replaceChildren();
  for (const p of ST.data.providers) {
    const key = p.has_key ? "key " + p.key_hint : p.requires_key ? "chưa có API key" : "không cần key";
    list.append(el("div", { class: "provider" },
      el("b", {}, p.name),
      el("span", { class: "info" }, el("span", { class: !p.has_key && p.requires_key ? "warn-key" : "" }, key), " · ",
        `${p.models.length} model`, p.base_url ? " · " + p.base_url : ""),
      el("button", { class: "btn tiny", onclick: () => editProvider(p) }, "Sửa")));
  }
  if (!ST.data.providers.length) list.append(el("div", { class: "hint" }, "Chưa có nhà cung cấp nào."));
}

$("#btnAddProvider").onclick = () => editProvider(null);
$("#pfCancel").onclick = () => $("#providerForm").classList.add("hidden");

function editProvider(p) {
  ST.editing = p;
  const f = $("#providerForm");
  f.classList.remove("hidden");
  $("#pfTitle").textContent = p ? `Sửa “${p.name}”` : "Thêm nhà cung cấp";
  $("#pfPresetField").classList.toggle("hidden", !!p);
  $("#pfTestResult").textContent = "";
  $("#pfTestResult").className = "hint";
  $("#pfKeyClear").checked = false;
  $("#pfKey").value = "";

  if (p) {
    fillProviderFields(p.name, p.type, p.base_url, p.models);
    $("#pfKey").placeholder = p.has_key ? `Để trống = giữ key hiện tại (${p.key_hint})` : "Dán API key";
    $("#pfNameField").classList.add("hidden");
  } else {
    const existing = new Set(ST.data.providers.map((x) => x.name));
    const sel = $("#pfPreset");
    sel.replaceChildren(...ST.data.presets.filter((x) => !existing.has(x.Name) || x.Name === "custom")
      .map((x) => el("option", { value: x.Name }, x.Label)));
    sel.onchange = () => applyPreset();
    applyPreset();
    $("#pfKey").placeholder = "Dán API key";
  }
  f.scrollIntoView({ block: "nearest", behavior: "smooth" });
}

function applyPreset() {
  const preset = ST.data.presets.find((x) => x.Name === $("#pfPreset").value) || {};
  const custom = preset.Name === "custom";
  $("#pfNameField").classList.toggle("hidden", !custom);
  $("#pfName").value = custom ? "" : preset.Name;
  fillProviderFields(preset.Name, "", preset.BaseURL || "", []);
  $("#pfTypeField").classList.toggle("hidden", !preset.NeedType);
}

function fillProviderFields(name, type, baseURL, models) {
  $("#pfType").value = type || "";
  $("#pfTypeField").classList.toggle("hidden", !type && name !== "custom");
  $("#pfBaseURL").value = baseURL || "";
  $("#pfModels").value = (models || []).join("\n");
  $("#pfCatalog").value = "";
  loadCatalog(name);
}

async function loadCatalog(provider) {
  const dl = $("#pfCatalogList");
  dl.replaceChildren();
  $("#pfCatalogHint").textContent = "";
  ST.catalog = [];
  if (!provider || provider === "custom") return;
  try { ST.catalog = await api("api/settings/catalog?provider=" + encodeURIComponent(provider)); } catch { return; }
  const price = (m) => (m.input_per_m || m.output_per_m ? ` — $${m.input_per_m}/$${m.output_per_m} mỗi 1M token vào/ra` : "");
  for (const m of ST.catalog) dl.append(el("option", { value: m.id }, (m.name || m.id) + price(m)));
  $("#pfCatalogHint").textContent = ST.catalog.length ? `Danh mục có ${ST.catalog.length} model. Trên OpenRouter có thể gõ tay model miễn phí có đuôi “:free” (giới hạn số lượt).` : "";
}

$("#pfCatalogAdd").onclick = () => {
  const id = $("#pfCatalog").value.trim();
  if (!id) return;
  const lines = $("#pfModels").value.split("\n").map((x) => x.trim()).filter(Boolean);
  if (!lines.includes(id)) lines.push(id);
  $("#pfModels").value = lines.join("\n");
  $("#pfCatalog").value = "";
};

function providerDraft() {
  const p = ST.editing;
  const name = p ? p.name : ($("#pfPreset").value === "custom" ? $("#pfName").value.trim() : $("#pfPreset").value);
  const key = $("#pfKey").value.trim();
  return {
    provider: name,
    type: $("#pfType").value,
    base_url: $("#pfBaseURL").value.trim(),
    models: $("#pfModels").value.split("\n").map((x) => x.trim()).filter(Boolean),
    api_key_action: $("#pfKeyClear").checked ? "clear" : key ? "replace" : "keep",
    api_key: key,
  };
}

$("#pfTest").onclick = async () => {
  const out = $("#pfTestResult");
  out.className = "hint";
  out.textContent = "Đang gửi một request nhỏ để kiểm tra…";
  $("#pfTest").disabled = true;
  try {
    const r = await api("api/settings/test", { method: "POST", body: providerDraft() });
    out.className = "hint ok";
    out.textContent = `✓ Kết nối được ${r.model} (${r.ms} ms). Trả lời: “${r.reply}”`;
  } catch (e) {
    out.className = "hint err";
    out.textContent = "✗ " + e.message;
  } finally { $("#pfTest").disabled = false; }
};

$("#providerForm").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("api/settings/provider", { method: "PUT", body: providerDraft() });
    toast("Đã lưu nhà cung cấp");
    $("#providerForm").classList.add("hidden");
    await loadSettings();
  } catch (err) { toast(err.message, "error"); }
});
