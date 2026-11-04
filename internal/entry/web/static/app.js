"use strict";

// ================= Trạng thái =================
const S = {
  view: "library",
  books: [],          // /api/books
  bookId: "",
  es: null,           // EventSource của truyện đang mở
  state: null,        // /api/books/{id}/state
  hasBook: false,
  outline: null,      // /api/books/{id}/outline
  bookSig: "",
  chapter: 0,
  chapterData: null,
  chapterNums: [],
  dirty: false,
  saveTimer: null,
  selection: "",
  sessions: [],
  sessionId: 0,       // 0 = phiên mới, tạo khi gửi tin đầu tiên
  messages: [],
  chatBusy: false,
  unreadAct: 0,
  openedAt: 0,
  newTarget: null,    // null = tạo truyện mới; id = bắt đầu truyện trống đó
};

const $ = (sel) => document.querySelector(sel);
const el = (tag, attrs = {}, ...kids) => {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") n.className = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) n.setAttribute(k, v === true ? "" : v);
  }
  for (const k of kids.flat()) if (k !== null && k !== undefined && k !== false) n.append(k.nodeType ? k : String(k));
  return n;
};

function loadLocal(key, fallback) {
  try { const v = localStorage.getItem(key); return v ? JSON.parse(v) : fallback; } catch { return fallback; }
}
function saveLocal(key, value) {
  try { localStorage.setItem(key, JSON.stringify(value)); } catch { /* bỏ qua */ }
}

async function api(path, opts = {}) {
  const init = { method: opts.method || "GET", headers: {} };
  if (opts.body !== undefined) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(opts.body);
  }
  if (opts.keepalive) init.keepalive = true;
  const res = await fetch(path, init);
  let data = null;
  try { data = await res.json(); } catch { /* không phải JSON */ }
  if (!res.ok) {
    const err = new Error((data && data.error) || `Lỗi ${res.status}`);
    err.status = res.status;
    throw err;
  }
  return data;
}
const bookApi = (path, opts) => api(`api/books/${S.bookId}/${path}`, opts);

function toast(msg, kind = "") {
  const t = el("div", { class: "toast " + kind }, msg);
  $("#toasts").append(t);
  setTimeout(() => t.remove(), kind === "error" ? 6000 : 3000);
}

function debounce(fn, ms) {
  let t;
  return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
}

const countWords = (s) => (s.trim() ? s.trim().split(/\s+/).length : 0);
const fmtNum = (n) => (n || 0).toLocaleString("vi-VN");
function timeAgo(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return "";
  const s = (Date.now() - d.getTime()) / 1000;
  if (s < 60) return "vừa xong";
  if (s < 3600) return Math.floor(s / 60) + " phút trước";
  if (s < 86400) return Math.floor(s / 3600) + " giờ trước";
  if (s < 86400 * 7) return Math.floor(s / 86400) + " ngày trước";
  return d.toLocaleDateString("vi-VN");
}

// ================= Markdown tối giản (đã escape) =================
function escapeHtml(s) {
  return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function inlineMd(s) {
  return s
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>")
    .replace(/(^|[^*])\*([^*\s][^*]*)\*/g, "$1<i>$2</i>");
}
function renderMd(src) {
  const lines = escapeHtml(src).split("\n");
  let html = "", list = null, para = [];
  const flushPara = () => { if (para.length) { html += "<p>" + inlineMd(para.join("<br>")) + "</p>"; para = []; } };
  const flushList = () => { if (list) { html += `</${list}>`; list = null; } };
  for (const raw of lines) {
    const line = raw.trimEnd();
    let m;
    if (!line.trim()) { flushPara(); flushList(); continue; }
    if ((m = line.match(/^(#{1,4})\s+(.*)$/))) { flushPara(); flushList(); html += `<h3>${inlineMd(m[2])}</h3>`; continue; }
    if ((m = line.match(/^\s*[-*•]\s+(.*)$/))) { flushPara(); if (list !== "ul") { flushList(); html += "<ul>"; list = "ul"; } html += `<li>${inlineMd(m[1])}</li>`; continue; }
    if ((m = line.match(/^\s*\d+[.)]\s+(.*)$/))) { flushPara(); if (list !== "ol") { flushList(); html += "<ol>"; list = "ol"; } html += `<li>${inlineMd(m[1])}</li>`; continue; }
    if ((m = line.match(/^&gt;\s?(.*)$/))) { flushPara(); flushList(); html += `<blockquote>${inlineMd(m[1])}</blockquote>`; continue; }
    flushList();
    para.push(line);
  }
  flushPara(); flushList();
  return html;
}
function proseNodes(text) {
  const frag = document.createDocumentFragment();
  for (const p of text.split(/\n\s*\n|\n/)) {
    if (p.trim()) frag.append(el("p", {}, p.trim()));
  }
  return frag;
}

// ================= Điều hướng =================
function route() {
  const m = location.hash.match(/^#\/b\/([\w-]+)/);
  if (m) openBook(m[1]);
  else showLibrary();
}
window.addEventListener("hashchange", route);

const STATE_TEXT = { idle: "Đang nghỉ", running: "AI đang làm việc", pausing: "Đang dừng…", paused: "Tạm dừng", completed: "Đã hoàn tất" };
const PHASE_TEXT = { init: "khởi tạo", premise: "dựng tiền đề", outline: "dựng dàn ý", writing: "viết chương", complete: "hoàn tất" };

// ================= Thư viện =================
async function loadBooks() {
  try { S.books = await api("api/books"); } catch (e) { toast(e.message, "error"); return; }
  if (S.view === "library") renderLibrary();
  renderBookMenu();
}

function renderLibrary() {
  const grid = $("#bookGrid");
  grid.replaceChildren();
  const running = S.books.filter((b) => b.running).length;
  $("#libSummary").textContent = `${S.books.length} truyện` + (running ? ` · ${running} truyện AI đang chạy` : "");

  for (const b of S.books) {
    const meta = el("div", { class: "meta" });
    if (b.running) meta.append(el("span", { class: "pill ok" }, el("span", { class: "dot running" }), "AI đang chạy"));
    else if (b.phase) meta.append(el("span", { class: "pill" }, PHASE_TEXT[b.phase] ? "Giai đoạn " + PHASE_TEXT[b.phase] : b.phase));
    else meta.append(el("span", { class: "pill" }, "Chưa bắt đầu"));
    if (b.total) meta.append(el("span", { class: "pill ai" }, `AI ${b.completed}/${b.total} chương`));
    if (b.mine_chapters) meta.append(el("span", { class: "pill me" }, `Bạn ${b.mine_chapters} chương · ${fmtNum(b.mine_words)} chữ`));
    if (b.mode) meta.append(el("span", { class: "pill" }, b.mode === "review" ? "Chỉ dàn ý" : "AI viết hết"));

    const card = el("div", { class: "book-card" + (b.running ? " running" : ""), tabindex: "0",
      onclick: () => (location.hash = `#/b/${b.id}`),
      onkeydown: (e) => { if (e.key === "Enter") location.hash = `#/b/${b.id}`; } },
      el("h3", {}, b.title || b.name),
      el("div", { class: "syn" }, b.synopsis || (b.title !== b.name ? b.name : "Chưa có tóm tắt.")),
      meta,
      el("div", { class: "foot" },
        el("span", {}, "Sửa lần cuối " + timeAgo(b.updated_at)),
        el("span", { class: "tools" },
          el("button", { class: "btn tiny ghost", title: "Đổi tên", onclick: (e) => { e.stopPropagation(); renameBook(b); } }, "✎"),
          el("button", { class: "btn tiny ghost", title: "Ẩn khỏi thư viện (giữ nguyên file)", onclick: (e) => { e.stopPropagation(); archiveBook(b); } }, "🗄"))));
    grid.append(card);
  }
  grid.append(el("div", { class: "book-card new-card", tabindex: "0", onclick: () => openNewDialog(null),
    onkeydown: (e) => { if (e.key === "Enter") openNewDialog(null); } },
    el("div", { class: "plus" }, "＋"), el("div", {}, "Truyện mới")));
}

async function renameBook(b) {
  const name = prompt("Tên truyện trong thư viện:", b.name);
  if (!name || !name.trim()) return;
  try { await api(`api/books/${b.id}`, { method: "PATCH", body: { name } }); loadBooks(); }
  catch (e) { toast(e.message, "error"); }
}
async function archiveBook(b) {
  if (!confirm(`Ẩn “${b.title || b.name}” khỏi thư viện?\n\nFile truyện và lịch sử trò chuyện vẫn được giữ nguyên trên đĩa.`)) return;
  try { await api(`api/books/${b.id}`, { method: "DELETE" }); toast("Đã ẩn khỏi thư viện"); loadBooks(); }
  catch (e) { toast(e.message, "error"); }
}

function showLibrary() {
  leaveBook();
  S.view = "library";
  $("#libraryView").classList.remove("hidden");
  $("#deskView").classList.add("hidden");
  document.title = "Thư viện – ainovel";
  loadBooks();
}

// ================= Mở / rời một truyện =================
async function leaveBook() {
  if (S.dirty) await saveMine({ keepalive: true });
  if (S.es) { S.es.close(); S.es = null; }
  S.bookId = "";
}

async function openBook(id) {
  if (S.bookId === id && S.view === "desk") return;
  await leaveBook();
  Object.assign(S, {
    view: "desk", bookId: id, state: null, hasBook: false, outline: null, bookSig: "",
    chapter: 0, chapterData: null, chapterNums: [], dirty: false, selection: "",
    sessions: [], sessionId: 0, messages: [], unreadAct: 0, openedAt: Date.now(),
  });
  $("#libraryView").classList.add("hidden");
  $("#deskView").classList.remove("hidden");
  $("#bookTitle").textContent = (S.books.find((b) => b.id === id) || {}).title || "…";
  $("#btnExport").href = `api/books/${id}/export`;
  $("#activity").replaceChildren();
  $("#actBadge").classList.add("hidden");
  $("#chapterList").replaceChildren(el("div", { class: "empty" }, "Đang mở truyện…"));
  $("#bible").replaceChildren();
  $("#aiOutline").replaceChildren();
  $("#aiText").replaceChildren();
  $("#aiChapterLabel").textContent = "—";
  $("#meChapterLabel").textContent = "Chọn một chương bên trái";
  const ed = $("#editor"); ed.value = ""; ed.disabled = true;
  updateWordCount(); setSaveState("—"); setSelection("");
  resetLive("");
  switchTab("ai", "outline");

  if (!S.books.length) loadBooks(); else renderBookMenu();
  await loadState();
  if (S.bookId !== id) return;
  connectEvents();
  loadSessions();
}

// ---- Menu chuyển truyện ----
function renderBookMenu() {
  const menu = $("#bookMenu");
  menu.replaceChildren();
  for (const b of S.books) {
    menu.append(el("button", { class: b.id === S.bookId ? "current" : "", onclick: () => { menu.classList.add("hidden"); location.hash = `#/b/${b.id}`; } },
      el("span", { class: "dot " + (b.running ? "running" : "") }), b.title || b.name));
  }
  menu.append(el("hr"),
    el("button", { onclick: () => { menu.classList.add("hidden"); openNewDialog(null); } }, "＋ Truyện mới"),
    el("button", { onclick: () => { menu.classList.add("hidden"); location.hash = "#/"; } }, "← Về thư viện"));
}
$("#bookSwitch").onclick = (e) => { e.stopPropagation(); loadBooks(); $("#bookMenu").classList.toggle("hidden"); };
document.addEventListener("click", (e) => { if (!e.target.closest(".book-switch")) $("#bookMenu").classList.add("hidden"); });
$("#btnBack").onclick = () => (location.hash = "#/");

// ================= Tabs =================
document.querySelectorAll("[data-tabs]").forEach((nav) => {
  nav.addEventListener("click", (e) => {
    const b = e.target.closest("button[data-tab]");
    if (b) switchTab(nav, b.dataset.tab);
  });
});
function switchTab(nav, name) {
  if (typeof nav === "string") nav = document.querySelector(`[data-tabs="${nav}"]`);
  nav.querySelectorAll("button").forEach((x) => x.classList.toggle("active", x.dataset.tab === name));
  const scope = nav.closest(".sidebar, .pane");
  scope.querySelectorAll(":scope > [data-panel]").forEach((p) => p.classList.toggle("active", p.dataset.panel === name));
  if (name === "activity") { S.unreadAct = 0; $("#actBadge").classList.add("hidden"); }
  if (name === "live") { const box = $("#aiLive"); box.scrollTop = box.scrollHeight; }
}

// ================= Trạng thái engine =================
async function loadState() {
  const id = S.bookId;
  if (!id) return;
  let data;
  try { data = await bookApi("state"); }
  catch (e) {
    if (e.status === 404) { toast("Không tìm thấy truyện", "error"); location.hash = "#/"; return; }
    setStatus("error", e.message);
    return;
  }
  if (S.bookId !== id) return;
  S.state = data.snapshot;
  S.hasBook = data.has_book;
  const s = S.state;
  const running = s.IsRunning || s.RuntimeState === "running";
  const title = s.BookTitle || data.name || "Truyện chưa đặt tên";

  $("#bookTitle").textContent = title;
  document.title = title + " – ainovel";
  let txt = STATE_TEXT[s.RuntimeState] || s.RuntimeState || "—";
  if (s.Phase && PHASE_TEXT[s.Phase]) txt += " · giai đoạn " + PHASE_TEXT[s.Phase];
  if (s.TotalChapters) txt += ` · AI đã viết ${s.CompletedCount}/${s.TotalChapters} chương`;
  if (s.TotalCostUSD > 0) txt += ` · $${s.TotalCostUSD.toFixed(3)}`;
  if (s.AdvanceMode === "review" && !running && s.Phase === "writing") txt += " · đang chờ bạn";
  setStatus(running ? "running" : s.RuntimeState === "paused" ? "paused" : "", txt);

  document.querySelectorAll(".seg").forEach((b) => b.classList.toggle("active", b.dataset.mode === (s.AdvanceMode || "auto")));
  $("#btnRun").disabled = running;
  $("#btnRun").textContent = S.hasBook ? "▶ Chạy AI" : "▶ Bắt đầu";
  $("#btnPause").disabled = !running;
  $("#btnNext").classList.toggle("hidden", s.AdvanceMode !== "review");
  $("#btnNext").disabled = running || s.Phase !== "writing";
  $("#liveDot").classList.toggle("hidden", !running);

  const sig = [s.Phase, s.CompletedCount, s.TotalChapters, (s.Outline || []).length, s.BookTitle, s.InProgressChapter].join("|");
  if (sig !== S.bookSig) {
    const first = S.bookSig === "";
    S.bookSig = sig;
    await loadOutline();
    if (!first && S.chapter) refreshChapterAI();
  }
}
const loadStateSoon = debounce(loadState, 700);

function setStatus(kind, text) {
  $("#statusDot").className = "dot " + kind;
  $("#statusText").textContent = text;
}

// ================= Dàn ý, thiết lập =================
async function loadOutline() {
  const id = S.bookId;
  let data;
  try { data = await bookApi("outline"); } catch { return; }
  if (S.bookId !== id) return;
  S.outline = data;
  renderChapterList();
  renderBible();
  if (!S.chapter && S.chapterNums.length) {
    const last = loadLocal("ainovel.lastChapter." + id, 0);
    selectChapter(S.chapterNums.includes(last) ? last : S.chapterNums[0]);
  }
}

function renderChapterList() {
  const box = $("#chapterList");
  const groups = (S.outline && S.outline.groups) || [];
  S.chapterNums = [];
  box.replaceChildren();
  if (!groups.length) {
    box.append(el("div", { class: "empty" },
      S.hasBook ? "AI đang dựng dàn ý… Bấm ▶ Chạy AI nếu nó đang dừng." : "Truyện này chưa có dàn ý. Bấm ▶ Bắt đầu để nhập ý tưởng."));
    return;
  }
  let lastVol = null;
  for (const g of groups) {
    const head = el("div", { class: "group-head", title: g.goal || "" });
    if (g.volume && g.volume !== lastVol) { head.append(el("span", { class: "vol" }, g.volume)); lastVol = g.volume; }
    if (g.arc) head.append(g.arc);
    const wrap = el("div", { class: "group" }, head);
    for (const c of g.chapters || []) {
      S.chapterNums.push(c.chapter);
      const marks = el("span", { class: "marks" });
      if (c.ai_done) marks.append(el("span", { class: "mark ai", title: "AI đã viết bản tham khảo" }, "AI"));
      if (c.mine_words) marks.append(el("span", { class: "mark me", title: "Số chữ bạn đã viết" }, fmtNum(c.mine_words)));
      wrap.append(el("button", {
        class: "ch" + (c.chapter === S.chapter ? " active" : ""), "data-ch": c.chapter, title: c.core_event || "",
        onclick: () => selectChapter(c.chapter),
      }, el("span", { class: "num" }, c.chapter), el("span", { class: "t" }, c.title || "(chưa đặt tên)"), marks));
    }
    if (!(g.chapters || []).length) {
      wrap.append(el("div", { class: "group-pending" }, g.pending ? `~${g.pending} chương, AI sẽ chi tiết hoá khi tới` : "Chưa chi tiết hoá"));
    }
    box.append(wrap);
  }
}

function renderBible() {
  const box = $("#bible");
  const b = S.outline || {};
  box.replaceChildren();
  if (b.synopsis) box.append(el("h3", {}, "Tóm tắt"), el("div", { class: "prose-sm" }, b.synopsis));
  if (b.premise) box.append(el("h3", {}, "Tiền đề"), el("div", { class: "prose-sm" }, b.premise));
  if ((b.characters || []).length) {
    box.append(el("h3", {}, `Nhân vật (${b.characters.length})`));
    for (const c of b.characters) {
      box.append(el("div", { class: "char" }, el("b", {}, c.name), el("span", { class: "role" }, c.role || ""),
        el("p", {}, c.description || ""), c.arc ? el("p", {}, "Hành trình: " + c.arc) : null));
    }
  }
  if (b.world) box.append(el("h3", {}, "Thế giới & luật lệ"), el("div", { class: "prose-sm" }, b.world));
  if (!box.children.length) box.append(el("div", { class: "empty" }, "Chưa có thiết lập."));
}

// ================= Chương =================
async function selectChapter(n) {
  if (S.dirty) await saveMine();
  const id = S.bookId;
  S.chapter = n;
  saveLocal("ainovel.lastChapter." + id, n);
  document.querySelectorAll(".ch").forEach((b) => b.classList.toggle("active", +b.dataset.ch === n));
  const active = document.querySelector(`.ch[data-ch="${n}"]`);
  if (active) active.scrollIntoView({ block: "nearest" });

  let d;
  try { d = await bookApi(`chapter/${n}`); } catch (e) { toast(e.message, "error"); return; }
  if (S.chapter !== n || S.bookId !== id) return;
  S.chapterData = d;
  const title = d.outline && d.outline.title ? `Chương ${n}: ${d.outline.title}` : `Chương ${n}`;
  $("#aiChapterLabel").textContent = title;
  $("#meChapterLabel").textContent = title;
  renderOutline(d);
  renderAIText(d);

  const ed = $("#editor");
  ed.disabled = false;
  ed.value = d.mine || "";
  ed.scrollTop = 0;
  S.dirty = false;
  updateWordCount();
  setSaveState(d.mine ? "Đã lưu" : "Chưa viết");
  setSelection("");
}

async function refreshChapterAI() {
  const n = S.chapter, id = S.bookId;
  try {
    const d = await bookApi(`chapter/${n}`);
    if (S.chapter !== n || S.bookId !== id) return;
    S.chapterData = { ...d, mine: S.chapterData ? S.chapterData.mine : d.mine };
    renderOutline(d);
    renderAIText(d);
  } catch { /* bỏ qua */ }
}

function list(items) {
  return el("ul", {}, (items || []).filter(Boolean).map((x) => el("li", {}, x)));
}

function renderOutline(d) {
  const box = $("#aiOutline");
  box.replaceChildren();
  const o = d.outline, p = d.plan;
  if (!o && !p) { box.append(el("div", { class: "empty" }, "Chương này chưa có dàn ý.")); return; }
  if (o) {
    box.append(el("h2", {}, o.title || `Chương ${d.chapter}`));
    if (o.core_event) box.append(el("h4", {}, "Sự kiện chính"), el("div", { class: "core" }, proseNodes(o.core_event)));
    if ((o.scenes || []).length) box.append(el("h4", {}, "Các cảnh"), list(o.scenes));
    if (o.hook) box.append(el("h4", {}, "Móc cuối chương"), el("p", {}, o.hook));
  }
  if (p) {
    box.append(el("h4", {}, "Kế hoạch chi tiết của AI"));
    const rows = [["Mục tiêu", p.goal], ["Xung đột", p.conflict], ["Cảm xúc", p.emotion_arc], ["Móc", p.hook], ["Ghi chú", p.notes]];
    box.append(list(rows.filter((r) => r[1]).map(([k, v]) => el("span", {}, el("b", {}, k + ": "), v))));
    const c = p.contract || {};
    if ((c.required_beats || []).length) box.append(el("h4", {}, "Nhịp bắt buộc"), list(c.required_beats));
    if ((c.forbidden_moves || []).length) box.append(el("h4", {}, "Không được xảy ra"), list(c.forbidden_moves));
    if ((c.continuity_checks || []).length) box.append(el("h4", {}, "Cần khớp với trước"), list(c.continuity_checks));
  }
}

function renderAIText(d) {
  const box = $("#aiText");
  box.replaceChildren();
  if (!d.ai_text) {
    const running = S.state && S.state.IsRunning;
    box.append(el("div", { class: "empty" },
      "AI chưa viết chương này.",
      el("br"),
      S.state && S.state.AdvanceMode === "review"
        ? "Bạn có thể tự viết dựa vào dàn ý, hoặc bấm “AI viết chương kế” để AI viết chương tiếp theo làm bản tham khảo."
        : running ? "AI sẽ viết tới chương này theo thứ tự." : ""));
    return;
  }
  box.append(el("div", { class: "ai-meta" },
    el("span", {}, d.ai_state === "final" ? "Bản đã duyệt" : "Bản nháp đang viết"),
    el("span", {}, "·"), el("span", {}, fmtNum(countWords(d.ai_text)) + " chữ"),
    el("button", { class: "btn tiny ghost", onclick: () => copyText(d.ai_text) }, "Sao chép")));
  box.append(proseNodes(d.ai_text));
}

async function copyText(t) {
  try { await navigator.clipboard.writeText(t); toast("Đã sao chép"); } catch { toast("Không sao chép được", "error"); }
}

// ================= Bản thảo của tác giả =================
const editor = $("#editor");
editor.addEventListener("input", () => {
  S.dirty = true;
  setSaveState("Đang gõ…");
  updateWordCount();
  clearTimeout(S.saveTimer);
  S.saveTimer = setTimeout(saveMine, 1200);
});
["select", "keyup", "mouseup"].forEach((ev) => editor.addEventListener(ev, () => {
  const sel = editor.value.slice(editor.selectionStart, editor.selectionEnd);
  setSelection(sel.trim().length >= 4 ? sel : "");
}));

function updateWordCount() { $("#wordCount").textContent = `${fmtNum(countWords(editor.value))} chữ`; }
function setSaveState(t) { $("#saveState").textContent = t; }

async function saveMine(opts = {}) {
  clearTimeout(S.saveTimer);
  if (!S.bookId || !S.chapter || !S.dirty) return;
  const id = S.bookId, n = S.chapter, text = editor.value;
  S.dirty = false;
  setSaveState("Đang lưu…");
  try {
    const r = await api(`api/books/${id}/chapter/${n}/mine`, { method: "PUT", body: { text }, keepalive: opts.keepalive });
    if (S.bookId !== id) return;
    if (S.chapterData && S.chapter === n) S.chapterData.mine = text;
    if (S.chapter === n) setSaveState("Đã lưu lúc " + new Date().toLocaleTimeString("vi-VN"));
    const mark = document.querySelector(`.ch[data-ch="${n}"] .marks`);
    if (mark) {
      let m = mark.querySelector(".mark.me");
      if (!r.words) { if (m) m.remove(); }
      else { if (!m) { m = el("span", { class: "mark me" }); mark.append(m); } m.textContent = fmtNum(r.words); }
    }
  } catch (e) {
    if (S.bookId === id) { S.dirty = true; setSaveState("Lưu thất bại!"); }
    toast("Lưu thất bại: " + e.message, "error");
  }
}

window.addEventListener("beforeunload", () => { if (S.dirty) saveMine({ keepalive: true }); });
document.addEventListener("keydown", (e) => {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "s" && S.view === "desk") { e.preventDefault(); S.dirty = true; saveMine(); }
});

function stepChapter(delta) {
  const i = S.chapterNums.indexOf(S.chapter);
  const next = S.chapterNums[i + delta];
  if (next) selectChapter(next);
}
$("#btnPrevCh").onclick = () => stepChapter(-1);
$("#btnNextCh").onclick = () => stepChapter(1);

function insertIntoEditor(text) {
  if (!S.chapter) { toast("Hãy chọn một chương trước", "error"); return; }
  const start = editor.selectionStart, end = editor.selectionEnd;
  const before = editor.value.slice(0, start), after = editor.value.slice(end);
  const pad = before && !before.endsWith("\n") ? "\n\n" : "";
  editor.value = before + pad + text.trim() + after;
  const pos = (before + pad + text.trim()).length;
  editor.setSelectionRange(pos, pos);
  editor.focus();
  editor.dispatchEvent(new Event("input"));
}

// ================= Luồng sự kiện =================
function connectEvents() {
  const id = S.bookId;
  const es = new EventSource(`api/books/${id}/events`);
  S.es = es;
  es.onmessage = (m) => {
    if (S.bookId !== id) return;
    let msg;
    try { msg = JSON.parse(m.data); } catch { return; }
    switch (msg.type) {
      case "event": addActivity(msg.data); loadStateSoon(); break;
      case "delta": appendLive(msg.data); break;
      case "clear": resetLive(""); break;
      case "live": resetLive(msg.data || ""); break;
      case "done": loadState(); break;
    }
  };
  es.onerror = () => { if (S.bookId === id) setStatus("error", "Mất kết nối, đang thử lại…"); };
  es.onopen = () => { if (S.bookId === id) loadState(); };
}

let liveNode = null;
function resetLive(text) {
  const box = $("#aiLive");
  box.replaceChildren();
  liveNode = document.createTextNode(text);
  box.append(liveNode);
  if (!text) box.append(el("div", { class: "empty" }, "Khi AI chạy, nội dung nó đang viết sẽ hiện ở đây theo thời gian thực."));
}
function appendLive(delta) {
  const box = $("#aiLive");
  if (!liveNode || !box.contains(liveNode)) resetLive("");
  const empty = box.querySelector(".empty");
  if (empty) empty.remove();
  const stick = box.scrollHeight - box.scrollTop - box.clientHeight < 60;
  liveNode.appendData(delta);
  if (stick) box.scrollTop = box.scrollHeight;
}

function addActivity(ev) {
  const box = $("#activity");
  const stick = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  const body = ev.detail && ev.detail !== ev.summary
    ? el("details", {}, el("summary", {}, el("span", { class: "cat" }, ev.category), ev.summary), el("pre", {}, ev.detail))
    : el("div", {}, el("span", { class: "cat" }, ev.category), ev.summary);
  box.append(el("div", { class: "act " + (ev.level || "") }, el("time", {}, ev.time), body));
  while (box.children.length > 400) box.firstChild.remove();
  if (stick) box.scrollTop = box.scrollHeight;
  if (!document.querySelector('[data-tab="activity"]').classList.contains("active")) {
    S.unreadAct++;
    const b = $("#actBadge");
    b.textContent = S.unreadAct > 99 ? "99+" : S.unreadAct;
    b.classList.remove("hidden");
  }
  // Sự kiện cũ phát lại khi vừa mở truyện thì không bật thông báo.
  if (ev.level === "error" && Date.now() - S.openedAt > 1500) toast(ev.summary, "error");
}

// ================= Điều khiển engine =================
document.querySelectorAll(".seg").forEach((b) => b.addEventListener("click", async () => {
  try {
    await bookApi("mode", { method: "POST", body: { mode: b.dataset.mode } });
    toast(b.dataset.mode === "review" ? "AI sẽ dừng trước mỗi chương mới, bạn tự viết" : "AI sẽ tự viết liên tục");
    loadState();
  } catch (e) { toast(e.message, "error"); }
}));

function currentMode() {
  const a = document.querySelector(".seg.active");
  return a ? a.dataset.mode : "review";
}

$("#btnRun").onclick = async () => {
  if (!S.hasBook) { openNewDialog(S.bookId); return; }
  if (currentMode() === "auto" && !confirm("Chế độ “AI viết hết”: AI sẽ tự viết liên tục nhiều chương và tốn token cho tới khi bạn bấm Dừng. Chạy tiếp?")) return;
  try {
    await bookApi("resume", { method: "POST", body: { mode: currentMode() } });
    toast("Đang khởi động AI…");
    switchTab("ai", "live");
    setTimeout(loadState, 800);
  } catch (e) { toast(e.message, "error"); }
};
$("#btnPause").onclick = async () => {
  try { await bookApi("pause", { method: "POST" }); toast("Đã gửi lệnh dừng, AI sẽ dừng sau bước hiện tại"); loadStateSoon(); }
  catch (e) { toast(e.message, "error"); }
};
$("#btnNext").onclick = async () => {
  try { await bookApi("next", { method: "POST" }); toast("AI đang viết chương tiếp theo"); switchTab("ai", "live"); loadStateSoon(); }
  catch (e) { toast(e.message, "error"); }
};
$("#btnChatToggle").onclick = () => $("#layout").classList.toggle("no-chat");

// ---- Truyện mới / bắt đầu truyện trống ----
const dlg = $("#newDialog");
function openNewDialog(targetId) {
  S.newTarget = targetId;
  $("#newTitle").textContent = targetId ? "Bắt đầu truyện này" : "Truyện mới";
  $("#newNameField").classList.toggle("hidden", !!targetId);
  $("#newName").value = "";
  const mode = targetId ? currentMode() : "review";
  dlg.querySelectorAll('input[name="newMode"]').forEach((r) => (r.checked = r.value === mode));
  dlg.showModal();
  (targetId ? $("#newPrompt") : $("#newName")).focus();
}
$("#btnLibNew").onclick = () => openNewDialog(null);
$("#newForm").addEventListener("submit", async (e) => {
  if (e.submitter && e.submitter.value === "cancel") return;
  e.preventDefault();
  const prompt = $("#newPrompt").value.trim();
  if (!prompt) { toast("Hãy nhập ý tưởng truyện", "error"); return; }
  const mode = dlg.querySelector('input[name="newMode"]:checked').value;
  $("#newSubmit").disabled = true;
  try {
    if (S.newTarget) {
      await api(`api/books/${S.newTarget}/start`, { method: "POST", body: { prompt, mode } });
      dlg.close();
      switchTab("ai", "live");
      setTimeout(loadState, 1000);
    } else {
      const row = await api("api/books", { method: "POST", body: { name: $("#newName").value.trim(), prompt, mode } });
      dlg.close();
      await loadBooks();
      location.hash = `#/b/${row.id}`;
      setTimeout(() => switchTab("ai", "live"), 300);
    }
    $("#newPrompt").value = "";
    toast(mode === "review" ? "AI đang dựng dàn ý…" : "AI bắt đầu viết…");
  } catch (err) { toast(err.message, "error"); }
  finally { $("#newSubmit").disabled = false; }
});

// ---- Kéo đổi độ rộng ----
(() => {
  const sp = $("#splitter"), pane = document.querySelector(".ai-pane"), desk = document.querySelector(".desk");
  const saved = loadLocal("ainovel.split", 0);
  if (saved) pane.style.flexBasis = saved + "%";
  sp.addEventListener("pointerdown", (e) => {
    sp.setPointerCapture(e.pointerId);
    const rect = desk.getBoundingClientRect();
    const move = (ev) => {
      const pct = Math.min(70, Math.max(20, ((ev.clientX - rect.left) / rect.width) * 100));
      pane.style.flexBasis = pct + "%";
      saveLocal("ainovel.split", Math.round(pct));
    };
    const up = () => { sp.removeEventListener("pointermove", move); sp.removeEventListener("pointerup", up); };
    sp.addEventListener("pointermove", move);
    sp.addEventListener("pointerup", up);
  });
})();

// ================= Phiên trò chuyện =================
async function loadSessions(selectId) {
  const id = S.bookId;
  try { S.sessions = await bookApi("sessions"); } catch (e) { toast(e.message, "error"); return; }
  if (S.bookId !== id) return;
  const remembered = loadLocal("ainovel.session." + id, 0);
  let want = selectId !== undefined ? selectId : remembered;
  if (want && !S.sessions.some((s) => s.id === want)) want = 0;
  if (!want && selectId === undefined && S.sessions.length) want = S.sessions[0].id;
  renderSessionSelect(want);
  await selectSession(want);
}

function renderSessionSelect(current) {
  const sel = $("#sessionSelect");
  sel.replaceChildren();
  if (!current) sel.append(el("option", { value: "0", selected: true }, "Phiên mới"));
  for (const s of S.sessions) {
    sel.append(el("option", { value: s.id, selected: s.id === current }, `${s.title} · ${timeAgo(s.updated_at)}`));
  }
}

async function selectSession(sid) {
  const id = S.bookId;
  S.sessionId = sid;
  saveLocal("ainovel.session." + id, sid);
  if (!sid) { S.messages = []; renderChat(); return; }
  try { S.messages = await api(`api/sessions/${sid}/messages`); } catch (e) { toast(e.message, "error"); S.messages = []; }
  if (S.bookId === id && S.sessionId === sid) renderChat();
}

$("#sessionSelect").onchange = (e) => selectSession(+e.target.value);
$("#btnSessNew").onclick = () => { renderSessionSelect(0); selectSession(0); $("#chatText").focus(); };
$("#btnSessRename").onclick = async () => {
  const s = S.sessions.find((x) => x.id === S.sessionId);
  if (!s) { toast("Phiên này chưa có tin nhắn nào", "error"); return; }
  const title = prompt("Tên phiên:", s.title);
  if (!title || !title.trim()) return;
  try { await api(`api/sessions/${s.id}`, { method: "PATCH", body: { title } }); loadSessions(s.id); }
  catch (e) { toast(e.message, "error"); }
};
$("#btnSessDelete").onclick = async () => {
  const s = S.sessions.find((x) => x.id === S.sessionId);
  if (!s) return;
  if (!confirm(`Xoá phiên “${s.title}” và toàn bộ tin nhắn trong đó?`)) return;
  try { await api(`api/sessions/${s.id}`, { method: "DELETE" }); loadSessions(); }
  catch (e) { toast(e.message, "error"); }
};

// ================= Trợ lý =================
function setSelection(text) {
  S.selection = text;
  const info = $("#selInfo");
  if (!text) { info.classList.add("hidden"); return; }
  info.replaceChildren(el("span", {}, `Đang chọn ${fmtNum(countWords(text))} chữ trong bản thảo — trợ lý sẽ xem đoạn này`),
    el("button", { title: "Bỏ chọn", onclick: () => setSelection("") }, "✕"));
  info.classList.remove("hidden");
}

function renderChat() {
  const log = $("#chatLog");
  log.replaceChildren();
  if (!S.messages.length) {
    log.append(el("div", { class: "chat-hint" },
      "Hỏi gì cũng được về chương đang viết. Trợ lý đọc được dàn ý, nhân vật, bản AI và bản thảo của bạn. Mỗi phiên trò chuyện được lưu riêng cho truyện này."));
  }
  for (const m of S.messages) log.append(msgNode(m));
  log.scrollTop = log.scrollHeight;
}

function msgNode(m) {
  const n = el("div", { class: "msg " + m.role, title: m.chapter ? `Lúc đang ở chương ${m.chapter}` : "" });
  if (m.role === "assistant") {
    n.innerHTML = renderMd(m.content);
    n.append(el("div", { class: "msg-tools" },
      el("button", { class: "btn tiny ghost", title: "Chèn vào vị trí con trỏ trong bản thảo", onclick: () => insertIntoEditor(stripMd(m.content)) }, "Chèn vào bản thảo"),
      el("button", { class: "btn tiny ghost", onclick: () => copyText(m.content) }, "Sao chép")));
  } else {
    n.textContent = (m.role === "steer" ? "➜ Engine: " : "") + m.content;
  }
  return n;
}
const stripMd = (s) => s.replace(/\*\*([^*]+)\*\*/g, "$1").replace(/^#+\s+/gm, "").replace(/^>\s?/gm, "");

function adoptSession(sess) {
  if (!sess || S.sessionId === sess.id) return;
  S.sessionId = sess.id;
  saveLocal("ainovel.session." + S.bookId, sess.id);
  if (!S.sessions.some((s) => s.id === sess.id)) S.sessions.unshift(sess);
  renderSessionSelect(sess.id);
}

async function sendChat(text) {
  text = text.trim();
  if (!text || S.chatBusy || !S.bookId) return;
  const id = S.bookId;
  const target = $("#chatTarget").value;
  $("#chatText").value = "";

  if (target === "steer") {
    S.messages.push({ role: "steer", content: text, chapter: S.chapter });
    renderChat();
    try {
      const r = await bookApi("steer", { method: "POST", body: { text, session_id: S.sessionId, chapter: S.chapter } });
      adoptSession(r.session);
      toast("Đã gửi chỉ đạo cho engine, xem tab Hoạt động");
    } catch (e) { toast(e.message, "error"); }
    return;
  }

  S.messages.push({ role: "user", content: text, chapter: S.chapter });
  renderChat();
  S.chatBusy = true;
  $("#btnSend").disabled = true;

  const log = $("#chatLog");
  const node = el("div", { class: "msg assistant typing" });
  log.append(node);
  log.scrollTop = log.scrollHeight;

  let answer = "", failed = "";
  try {
    const res = await fetch(`api/books/${id}/chat`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ session_id: S.sessionId, text, chapter: S.chapter, mine: editor.value, selection: S.selection }),
    });
    if (!res.ok) {
      let msg = `Lỗi ${res.status}`;
      try { msg = (await res.json()).error || msg; } catch { /* */ }
      throw new Error(msg);
    }
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = "", pending = false;
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n\n")) >= 0) {
        const chunk = buf.slice(0, idx); buf = buf.slice(idx + 2);
        if (!chunk.startsWith("data: ")) continue;
        const ev = JSON.parse(chunk.slice(6));
        if (ev.type === "session") { if (S.bookId === id) adoptSession(ev.session); }
        else if (ev.type === "delta") answer += ev.text;
        else if (ev.type === "error") failed = ev.text;
      }
      if (!pending && S.bookId === id) {
        pending = true;
        requestAnimationFrame(() => {
          pending = false;
          const stick = log.scrollHeight - log.scrollTop - log.clientHeight < 60;
          node.innerHTML = renderMd(answer);
          if (stick) log.scrollTop = log.scrollHeight;
        });
      }
    }
  } catch (e) { failed = e.message; }

  S.chatBusy = false;
  $("#btnSend").disabled = false;
  if (S.bookId !== id) return;
  node.remove();
  if (answer.trim()) S.messages.push({ role: "assistant", content: answer, chapter: S.chapter });
  renderChat();
  if (failed) {
    log.append(el("div", { class: "msg error" }, "Lỗi: " + failed));
    log.scrollTop = log.scrollHeight;
  }
}

$("#btnSend").onclick = () => sendChat($("#chatText").value);
$("#chatText").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) { e.preventDefault(); sendChat(e.target.value); }
});
$("#chips").addEventListener("click", (e) => {
  const b = e.target.closest("button[data-q]");
  if (!b) return;
  if (b.dataset.needsSel && !S.selection) { toast("Hãy bôi đen một đoạn trong bản thảo trước", "error"); return; }
  $("#chatTarget").value = "assist";
  sendChat(b.dataset.q);
});

// ================= Khởi động =================
route();
setInterval(() => {
  if (document.hidden) return;
  if (S.view === "library") loadBooks();
  else if (S.state && (S.state.IsRunning || S.state.RuntimeState === "pausing")) loadState();
}, 5000);
