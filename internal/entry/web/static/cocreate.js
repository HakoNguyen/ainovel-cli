"use strict";
// Cùng AI xây dựng ý tưởng (start) và bàn hướng đi giữa chừng (stage).
// Dùng chung tiện ích ($, el, api, toast, renderMd, S, loadState…) từ app.js.

const CO_TEXT = {
  start: {
    title: "Cùng AI xây dựng ý tưởng",
    hint: "Trả lời các câu hỏi của AI. Bản yêu cầu bên phải được cập nhật sau mỗi lượt. Khi thấy ổn, chọn chế độ rồi bấm “Bắt đầu sáng tác”.",
    apply: "Bắt đầu sáng tác",
    cancel: "Xoá cuộc trò chuyện",
    cancelConfirm: "Xoá toàn bộ cuộc trò chuyện xây dựng ý tưởng của truyện này?",
    draftTitle: "sáng tác",
    empty: "Kể cho AI nghe ý tưởng ban đầu của bạn, vài câu là đủ: thể loại, nhân vật chính, điều bạn muốn người đọc cảm thấy…",
  },
  stage: {
    title: "Bàn hướng đi tiếp theo",
    hint: "AI đã tạm dừng viết. Cùng AI tính đoạn tiếp theo (vài chương tới, cung/tập sau). Xong thì bấm “Áp dụng hướng đi”, AI sẽ viết tiếp theo hướng này.",
    apply: "Áp dụng hướng đi & viết tiếp",
    cancel: "Huỷ bàn bạc (giữ tạm dừng)",
    cancelConfirm: "Huỷ phiên bàn hướng đi? AI sẽ giữ trạng thái tạm dừng.",
    draftTitle: "hướng đi tiếp theo",
    empty: "",
  },
};
const STAGE_OPENER = "Tôi muốn tạm dừng một chút để cùng bạn quy hoạch hướng đi tiếp theo.";

const CO = { kind: null, sess: null, busy: false, draftTimer: null, bookId: "" };

function coShow(on) {
  document.querySelector(".desk").classList.toggle("hidden", on);
  $("#cocreate").classList.toggle("hidden", !on);
}

function closeCoCreate() {
  CO.kind = null;
  coShow(false);
}

async function openCoCreate(kind, firstText) {
  const id = S.bookId;
  if (!id) return;
  CO.kind = kind; CO.bookId = id; CO.sess = null;
  const t = CO_TEXT[kind];
  $("#coTitle").textContent = t.title;
  $("#coHint").textContent = t.hint;
  $("#coApply").textContent = t.apply;
  $("#coCancel").textContent = t.cancel;
  $("#coDraftTitle").textContent = t.draftTitle;
  const mode = (S.state && S.state.AdvanceMode) || "review";
  document.querySelectorAll('input[name="coMode"]').forEach((r) => (r.checked = r.value === (kind === "start" ? "review" : mode)));
  coShow(true);
  renderCo();

  try {
    const r = await bookApi(`cocreate?kind=${kind}`);
    if (S.bookId !== id || CO.kind !== kind) return;
    CO.sess = r.session;
  } catch (e) { toast(e.message, "error"); return; }
  renderCo();

  const opener = (firstText || "").trim() || (kind === "stage" ? STAGE_OPENER : "");
  if (!CO.sess && opener) coSend(opener);
  else $("#coText").focus();
}

function renderCo(streaming) {
  const log = $("#coLog");
  const sess = CO.sess || { display: [], draft: "", ready: false, suggestions: [] };
  log.replaceChildren();
  if (!sess.display.length && !streaming && CO.kind && CO_TEXT[CO.kind].empty) {
    log.append(el("div", { class: "chat-hint" }, CO_TEXT[CO.kind].empty));
  }
  for (const line of sess.display) {
    const n = el("div", { class: "msg " + line.role });
    if (line.role === "assistant") n.innerHTML = renderMd(line.text);
    else n.textContent = line.text;
    log.append(n);
  }
  if (streaming) log.append(streaming);
  log.scrollTop = log.scrollHeight;

  if (document.activeElement !== $("#coDraft")) $("#coDraft").value = sess.draft || "";
  const ready = $("#coReady");
  ready.textContent = sess.ready ? "✓ AI thấy đã đủ thông tin" : sess.display.length ? "AI còn muốn hỏi thêm" : "";
  ready.className = "pill" + (sess.ready ? " ok" : "");
  ready.classList.toggle("hidden", !ready.textContent);

  const chips = $("#coSuggest");
  chips.replaceChildren();
  for (const s of sess.suggestions || []) {
    chips.append(el("button", { title: "Bấm để điền vào ô trả lời", onclick: () => { const t = $("#coText"); t.value = s; t.focus(); } }, s));
  }
  $("#coSend").disabled = CO.busy;
  $("#coApply").disabled = CO.busy || !($("#coDraft").value.trim());
}

async function coSend(text) {
  text = (text || "").trim();
  if (!text || CO.busy || !CO.kind) return;
  const id = CO.bookId, kind = CO.kind;
  CO.busy = true;
  $("#coText").value = "";

  // Hiện ngay câu của người dùng + bong bóng AI đang trả lời.
  const sess = CO.sess || { kind, display: [], draft: "", ready: false, suggestions: [] };
  CO.sess = { ...sess, display: [...sess.display, { role: "user", text }], suggestions: [] };
  const bubble = el("div", { class: "msg assistant typing" }, el("span", { class: "muted" }, "AI đang suy nghĩ…"));
  renderCo(bubble);

  let result = null, failed = "";
  try {
    const res = await fetch(`api/books/${id}/cocreate/message`, {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ kind, text }),
    });
    if (!res.ok) {
      let msg = `Lỗi ${res.status}`;
      try { msg = (await res.json()).error || msg; } catch { /* */ }
      throw new Error(msg);
    }
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let idx;
      while ((idx = buf.indexOf("\n\n")) >= 0) {
        const chunk = buf.slice(0, idx); buf = buf.slice(idx + 2);
        if (!chunk.startsWith("data: ")) continue;
        const ev = JSON.parse(chunk.slice(6));
        if (ev.type === "reply" && ev.text) { bubble.innerHTML = renderMd(ev.text); }
        else if (ev.type === "thinking" && !bubble.dataset.replied) {
          bubble.replaceChildren(el("span", { class: "muted" }, `AI đang suy nghĩ… (${ev.text.length} ký tự)`));
        }
        else if (ev.type === "result") result = ev.session;
        else if (ev.type === "error") failed = ev.text;
        if (ev.type === "reply") bubble.dataset.replied = "1";
      }
      const log = $("#coLog");
      log.scrollTop = log.scrollHeight;
    }
  } catch (e) { failed = e.message; }

  CO.busy = false;
  if (S.bookId !== id || CO.kind !== kind) return;
  if (result) {
    CO.sess = result;
    renderCo();
  } else {
    CO.sess = sess; // không lưu được lượt này: trả lại câu cho người dùng gửi lại
    renderCo();
    $("#coText").value = text;
    toast("AI chưa trả lời được: " + (failed || "lỗi không rõ"), "error");
  }
  if (kind === "stage") loadStateSoon();
}

$("#coSend").onclick = () => coSend($("#coText").value);
$("#coText").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) { e.preventDefault(); coSend(e.target.value); }
});
$("#coClose").onclick = closeCoCreate;

$("#coDraft").addEventListener("input", () => {
  $("#coApply").disabled = CO.busy || !$("#coDraft").value.trim();
  clearTimeout(CO.draftTimer);
  const id = CO.bookId, kind = CO.kind, draft = $("#coDraft").value;
  CO.draftTimer = setTimeout(async () => {
    try {
      await api(`api/books/${id}/cocreate/draft`, { method: "PUT", body: { kind, draft } });
      if (CO.sess) { CO.sess.draft = draft; CO.sess.draft_edited = true; }
    } catch (e) { toast("Lưu bản yêu cầu thất bại: " + e.message, "error"); }
  }, 800);
});

$("#coApply").onclick = async () => {
  const draft = $("#coDraft").value.trim();
  if (!draft) return;
  const kind = CO.kind;
  const mode = document.querySelector('input[name="coMode"]:checked').value;
  if (mode === "auto" && !confirm("Chế độ “AI viết hết”: AI sẽ tự viết liên tục nhiều chương và tốn token cho tới khi bạn bấm Dừng. Tiếp tục?")) return;
  $("#coApply").disabled = true;
  try {
    clearTimeout(CO.draftTimer);
    await bookApi("cocreate/apply", { method: "POST", body: { kind, mode, draft } });
    closeCoCreate();
    toast(kind === "start" ? (mode === "review" ? "AI đang dựng dàn ý theo bản yêu cầu…" : "AI bắt đầu viết…") : "Đã gửi hướng đi, AI sẽ viết tiếp");
    switchTab("ai", "live");
    setTimeout(loadState, 1000);
  } catch (e) { toast(e.message, "error"); $("#coApply").disabled = false; }
};

$("#coCancel").onclick = async () => {
  const kind = CO.kind;
  if (!kind || !confirm(CO_TEXT[kind].cancelConfirm)) return;
  try {
    await bookApi("cocreate/cancel", { method: "POST", body: { kind } });
    CO.sess = null;
    if (kind === "stage") { closeCoCreate(); loadState(); }
    else renderCo();
  } catch (e) { toast(e.message, "error"); }
};

$("#btnStage").onclick = () => openCoCreate("stage");
