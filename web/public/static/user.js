const form = document.querySelector("#query-form");
const codeInput = document.querySelector("#card-code");
const answerInput = document.querySelector("#captcha-answer");
const captchaBox = document.querySelector("#captcha-box");
const captchaQuestion = document.querySelector("#captcha-question");
const errorBox = document.querySelector("#error");
const resultPanel = document.querySelector("#result");
const queryButton = document.querySelector("#query-button");
const refreshButton = document.querySelector("#refresh-totp");
const copyStatus = document.querySelector("#copy-status");
const savedCardsPanel = document.querySelector("#saved-cards");
const savedCardsList = document.querySelector("#saved-cards-list");
const clearSavedCardsButton = document.querySelector("#clear-saved-cards");
const savedCardsStorageKey = "vitals.redeemed-cards.v1";
const maximumSavedCards = 20;
let captchaId = 0;
let currentSecret = "";
let validUntil = null;
let generatedTOTPPeriod = null;
let actionMode = "query";
let actionCaptchaId = 0;
let actionPreviewSignature = "";
let actionBusy = false;
const actionForm = document.querySelector("#card-action-form");
const originalInput = document.querySelector("#original-card-code");
const newInput = document.querySelector("#new-card-code");
const actionPreview = document.querySelector("#action-preview");
const actionError = document.querySelector("#action-error");
const actionSuccess = document.querySelector("#action-success");
const actionConfirm = document.querySelector("#action-confirm-button");

document.querySelectorAll("button[data-mode]").forEach(button => {
  button.addEventListener("click", () => setActionMode(button.dataset.mode));
});
function setActionMode(mode, originalCode) {
  if (actionBusy || queryButton.disabled) return;
  actionMode = mode;
  document.querySelectorAll("button[data-mode]").forEach(button => button.setAttribute("aria-pressed", String(button.dataset.mode === mode)));
  document.querySelector("#query-panel").hidden = mode !== "query";
  document.querySelector("#action-panel").hidden = mode === "query";
  if (originalCode) originalInput.value = originalCode;
  document.querySelector("#action-title").textContent = mode === "different" ? "兑换另一个不同账号" : "续费原兑换码";
  document.querySelector("#action-help").textContent = mode === "different"
    ? "新码获得独立有效期，分配时避开关联卡密正在使用的账号。原码须已激活；已结束的历史账号不在排除范围内。"
    : "将新码全部时长转入原码，成功后新码不能再次兑换。未过期从原到期日累加，已过期从现在起算，未激活只合并天数。原账号不可用时可能换号。";
  resultPanel.classList.remove("visible");
  currentSecret = ""; validUntil = null;
  resetActionPreview();
  actionCaptchaId = 0;
  document.querySelector("#action-captcha").classList.remove("visible");
  actionSuccess.textContent = "";
  actionError.classList.remove("visible");
}
function resetActionPreview() {
  actionPreviewSignature = ""; actionPreview.hidden = true; actionConfirm.disabled = true;
}
function actionPayload() {
  const value = { action: actionMode, original_code: originalInput.value.trim(), new_code: newInput.value.trim() };
  if (actionCaptchaId) Object.assign(value, { captcha_id: actionCaptchaId, captcha_answer: document.querySelector("#action-captcha-answer").value.trim() });
  return value;
}
function actionSignature() { return JSON.stringify([actionMode, originalInput.value.trim(), newInput.value.trim()]); }
[originalInput, newInput].forEach(input => input.addEventListener("input", () => { input.value = normalizeCardCode(input.value); resetActionPreview(); actionSuccess.textContent = ""; }));
async function requestCardAction(path, payload) {
  const response = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload) });
  return { ...await response.json(), ok: response.ok };
}
function displayActionError(body) {
  const error = errorBody(body);
  if (error.code === "captcha_required" && error.captcha) {
    actionCaptchaId = error.captcha.id;
    document.querySelector("#action-captcha-question").textContent = error.captcha.question;
    document.querySelector("#action-captcha").classList.add("visible");
    document.querySelector("#action-captcha-answer").focus();
  }
  actionError.textContent = error.message || "操作未完成，请使用相同兑换码重试。";
  actionError.classList.add("visible");
}
function setActionBusy(value) {
  actionBusy = value;
  originalInput.disabled = value; newInput.disabled = value;
  document.querySelector("#action-preview-button").disabled = value;
  document.querySelectorAll("button[data-mode]").forEach(button => { button.disabled = value; });
  actionConfirm.disabled = value || !actionPreviewSignature;
}
function describeOperation(operation) {
  const date = operation.expires_at ? new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" }).format(new Date(operation.expires_at)) : "激活后开始计时";
  const name = operation.action === "different" ? "兑换不同账号" : "续费原码";
  const previous = operation.before_expires_at ? new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" }).format(new Date(operation.before_expires_at)) : "尚未激活";
  const status = ({ unused: "未激活", redeemed: "使用中", expired: "已过期" })[operation.original_status] || "已检查";
  return `原码状态：${status}；原有效至：${previous}。\n${name}：${operation.added_days} 天。\n操作后有效至：${date}。${operation.status === "unused" ? `合并后共 ${operation.duration_days} 天，仍未激活。` : ""}\n${operation.action === "renew" ? "确认成功后，新码将用于续费，之后继续使用原码。" : "确认成功后，使用新码查询新账号。"}${operation.account_changed ? " 原账号已不可用，本次会重新分配账号。" : ""}`;
}
actionForm.addEventListener("submit", async event => {
  event.preventDefault();
  if (actionBusy) return;
  resetActionPreview(); actionError.classList.remove("visible"); actionSuccess.textContent = "";
  if (originalInput.value.length !== 14 || newInput.value.length !== 14 || originalInput.value === newInput.value) {
    displayActionError({ message: "请输入两个不同的完整兑换码。" }); return;
  }
  setActionBusy(true);
  try {
    const response = await requestCardAction("/api/cards/action-preview", actionPayload());
    if (!response.ok) { displayActionError(response); return; }
    actionPreview.textContent = describeOperation(response.operation) + (response.operation.replayed ? "\n该操作已完成，可确认刷新查询结果，不会再次消耗新码。" : "");
    actionPreview.hidden = false; actionPreviewSignature = actionSignature();
  } catch { displayActionError({ message: "预览未完成，请稍后重试。" }); }
  finally { setActionBusy(false); }
});
actionConfirm.addEventListener("click", async () => {
  if (actionBusy || !actionPreviewSignature || actionPreviewSignature !== actionSignature()) return;
  actionError.classList.remove("visible"); setActionBusy(true);
  let completed = false;
  try {
    const response = await requestCardAction(actionMode === "renew" ? "/api/cards/renew" : "/api/redeem-different", actionPayload());
    if (!response.ok) { resetActionPreview(); displayActionError(response); return; }
    completed = true;
    const code = actionMode === "renew" ? originalInput.value.trim() : newInput.value.trim();
    resetActionPreview();
    actionSuccess.textContent = (actionMode === "renew" ? "续费成功，新码已用于续费。请继续使用原码。" : "兑换成功，请使用新码查询账号。") + (response.operation.account_changed ? " 本次已重新分配账号。" : "");
    if (response.operation.status === "unused") {
      actionSuccess.textContent += ` 合并后共 ${response.operation.duration_days} 天，尚未开始计时。`; return;
    }
    saveRedeemedCard(code, response.operation.expires_at);
    codeInput.value = code;
    const query = await queryCard({ code });
    if (query.ok) await renderResult(query.result);
    else actionSuccess.textContent += " 账号信息请切换到查询 / 兑换后查询。";
  } catch {
    if (completed) actionSuccess.textContent += " 账号信息暂未加载，请使用有效兑换码查询。";
    else displayActionError({ message: "未能确认结果，请保留这两个兑换码并重试；相同操作不会重复续费。" });
  } finally { setActionBusy(false); }
});

renderSavedCards();

setInterval(() => {
  document.querySelector("#clock").textContent = new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(new Date());
  updateCountdowns();
}, 1000);

codeInput.addEventListener("input", () => {
  const compact = codeInput.value.toUpperCase().replace(/[^2-9A-HJKMNP-Z]/g, "").slice(0, 12);
  codeInput.value = compact.replace(/(.{4})(?=.)/g, "$1-");
});

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  queryButton.disabled = true;
  errorBox.classList.remove("visible");
  resultPanel.classList.remove("visible");
  currentSecret = "";
  validUntil = null;
  const payload = { code: codeInput.value.trim() };
  if (captchaId) {
    payload.captcha_id = captchaId;
    payload.captcha_answer = answerInput.value.trim();
  }
  try {
    const redeem = await redeemCard(payload.code);
    if (!redeem.ok && errorBody(redeem).code === "no_account_capacity") {
      captchaId = 0;
      captchaBox.classList.remove("visible");
      answerInput.value = "";
      showError("暂无可分配账号，兑换未完成，卡密未消耗。请联系管理员补充有效账号后重试。");
      return;
    }
    let body = await queryCard(payload);
    let apiError = errorBody(body);
    if (redeem.ok || body.ok) {
      saveRedeemedCard(payload.code, body.ok ? body.result.card.valid_until : null);
    }
    if (body.ok) {
      captchaId = 0;
      captchaBox.classList.remove("visible");
      answerInput.value = "";
      renderResult(body.result);
      return;
    }
    if (savedCardHasExpired(payload.code)) {
      captchaId = 0;
      captchaBox.classList.remove("visible");
      answerInput.value = "";
      showError("卡密已过期。");
      return;
    }
    if (apiError.code === "captcha_required" && apiError.captcha) {
      captchaId = apiError.captcha.id;
      captchaQuestion.textContent = apiError.captcha.question;
      captchaBox.classList.add("visible");
      answerInput.focus();
      showError("需要完成验证码后再查询。");
      return;
    }
    showError(apiError.code === "captcha_invalid" ? "验证码无效或已过期，请重新查询。" : "卡密暂不可查询，请检查后重试。");
  } catch {
    showError("请求未完成，请稍后重试。");
  } finally {
    queryButton.disabled = false;
  }
});

savedCardsList.addEventListener("click", (event) => {
  const button = event.target.closest("button[data-saved-code]");
  if (!button) return;
  if (actionBusy || queryButton.disabled) return;
  setActionMode("query");
  codeInput.value = button.dataset.savedCode;
  form.requestSubmit();
});

savedCardsList.addEventListener("click", (event) => {
  const button = event.target.closest("button[data-remove-code]");
  if (!button) return;
  writeSavedCards(readSavedCards().filter((item) => item.code !== button.dataset.removeCode));
  renderSavedCards();
});

clearSavedCardsButton.addEventListener("click", () => {
  writeSavedCards([]);
  renderSavedCards();
});
savedCardsList.addEventListener("click", event => {
  const button = event.target.closest("button[data-card-action]");
  if (button) { setActionMode(button.dataset.cardAction, button.dataset.originalCode); document.querySelector("#action-panel").scrollIntoView({ block: "start" }); }
});

function errorBody(body) {
  return body.error || body || {};
}

async function queryCard(payload) {
  const response = await fetch("/api/cards/query", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  const body = await response.json();
  return { ...body, ok: response.ok, status: response.status };
}

async function redeemCard(code) {
  const response = await fetch("/api/redeem", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ code }),
  });
  const body = await response.json();
  return { ...body, ok: response.ok, status: response.status };
}

refreshButton.addEventListener("click", async () => {
  if (!currentSecret) return;
  await refreshDisplayedTOTP();
});

document.querySelectorAll("[data-copy-target]").forEach((button) => {
  button.addEventListener("click", async () => {
    const target = document.querySelector(`#${button.dataset.copyTarget}`);
    const value = target?.textContent?.trim() || "";
    if (!value || !navigator.clipboard?.writeText) {
      setCopyStatus("无法复制，请手动选择。");
      return;
    }
    try {
      await navigator.clipboard.writeText(value);
      setCopyStatus("已复制");
    } catch {
      setCopyStatus("无法复制，请手动选择。");
    }
  });
});

async function renderResult(result) {
  currentSecret = result.totp?.secret || "";
  generatedTOTPPeriod = null;
  validUntil = new Date(result.card.valid_until);
  document.querySelector("#account").textContent = result.account.display_username;
  document.querySelector("#password").textContent = result.account.password;
  const pickup = document.querySelector("#pickup-address");
  const pickupAddress = result.account.pickup_address || "";
  pickup.textContent = pickupAddress || "未提供";
  if (pickupAddress) pickup.href = pickupAddress;
  else pickup.removeAttribute("href");
  pickup.toggleAttribute("aria-disabled", !pickupAddress);
  document.querySelector("#pickup-instructions").hidden = !pickupAddress;
  document.querySelector("#totp-item").hidden = Boolean(pickupAddress);
  document.querySelector("#replacement-notice").textContent = result.replacement_notice.state === "grace" ? "当前处于换号宽限期，请留意后续替换通知。" : "当前账号为主账号。";
  if (!pickupAddress && currentSecret) {
    refreshButton.disabled = false;
    await refreshDisplayedTOTP();
  } else {
    refreshButton.disabled = true;
    document.querySelector("#totp-code").textContent = "未提供";
    document.querySelector("#totp-timer").textContent = "请使用取件地址";
  }
  setCopyStatus("");
  resultPanel.classList.add("visible");
}

async function refreshDisplayedTOTP() {
  if (!currentSecret) return;
  const period = Math.floor(Date.now() / 1000 / 30);
  document.querySelector("#totp-code").textContent = await generateTOTP(currentSecret, period);
  generatedTOTPPeriod = period;
  updateCountdowns();
}

function setCopyStatus(message) {
  copyStatus.textContent = message;
}

function updateCountdowns() {
  const now = Date.now();
  const nowSeconds = Math.floor(now / 1000);
  const currentPeriod = Math.floor(nowSeconds / 30);
  if (!currentSecret) {
    document.querySelector("#totp-timer").textContent = "请使用取件地址";
  } else if (generatedTOTPPeriod !== null && currentPeriod > generatedTOTPPeriod) {
    document.querySelector("#totp-timer").textContent = "当前验证码已过期，请手动刷新";
  } else {
    const period = generatedTOTPPeriod ?? currentPeriod;
    document.querySelector("#totp-timer").textContent = `${Math.max(0, (period + 1) * 30 - nowSeconds)}s 后进入下一周期`;
  }
  if (validUntil) {
    const seconds = Math.max(0, Math.floor((validUntil.getTime() - now) / 1000));
    const days = Math.floor(seconds / 86400);
    const hours = Math.floor((seconds % 86400) / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);
    document.querySelector("#expires-in").textContent = `${days} 天 ${hours} 小时 ${minutes} 分钟`;
  }
}

function showError(message) {
  errorBox.textContent = message;
  errorBox.classList.add("visible");
}

function normalizeCardCode(value) {
  const compact = String(value || "").toUpperCase().replace(/[^2-9A-HJKMNP-Z]/g, "").slice(0, 12);
  return compact.replace(/(.{4})(?=.)/g, "$1-");
}

function savedCardHasExpired(code) {
  const normalized = normalizeCardCode(code);
  const saved = readSavedCards().find((item) => item.code === normalized);
  if (!saved?.valid_until) return false;
  const expiresAt = new Date(saved.valid_until).getTime();
  return Number.isFinite(expiresAt) && expiresAt <= Date.now();
}

function readSavedCards() {
  try {
    const parsed = JSON.parse(localStorage.getItem(savedCardsStorageKey) || "[]");
    if (!Array.isArray(parsed)) return [];
    return parsed.flatMap((item) => {
      const code = normalizeCardCode(item?.code);
      if (code.length !== 14) return [];
      const validUntil = item.valid_until && !Number.isNaN(new Date(item.valid_until).getTime()) ? item.valid_until : null;
      return [{ code, saved_at: item.saved_at || null, valid_until: validUntil }];
    }).slice(0, maximumSavedCards);
  } catch {
    return [];
  }
}

function writeSavedCards(items) {
  try {
    if (items.length === 0) {
      localStorage.removeItem(savedCardsStorageKey);
      return;
    }
    localStorage.setItem(savedCardsStorageKey, JSON.stringify(items.slice(0, maximumSavedCards)));
  } catch {
    // Browsers may deny storage in private or hardened modes; querying still works.
  }
}

function saveRedeemedCard(code, validUntil) {
  const normalized = normalizeCardCode(code);
  if (normalized.length !== 14) return;
  const previous = readSavedCards().find((item) => item.code === normalized);
  const next = {
    code: normalized,
    saved_at: previous?.saved_at || new Date().toISOString(),
    valid_until: validUntil || previous?.valid_until || null,
  };
  writeSavedCards([next, ...readSavedCards().filter((item) => item.code !== normalized)]);
  renderSavedCards();
}

function renderSavedCards() {
  const items = readSavedCards();
  savedCardsPanel.hidden = items.length === 0;
  savedCardsList.replaceChildren();
  for (const item of items) {
    const row = document.createElement("li");
    row.className = "saved-card-item";

    const details = document.createElement("div");
    const code = document.createElement("code");
    code.textContent = item.code;
    details.append(code);
    if (item.valid_until) {
      const expiry = document.createElement("small");
      expiry.textContent = `有效至 ${new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" }).format(new Date(item.valid_until))}`;
      details.append(expiry);
    }

    const actions = document.createElement("div");
    actions.className = "saved-card-actions";
    const query = document.createElement("button");
    query.type = "button";
    query.dataset.savedCode = item.code;
    query.textContent = "查询";
    const remove = document.createElement("button");
    remove.type = "button";
    remove.dataset.removeCode = item.code;
    remove.textContent = "移除";
    const renew = document.createElement("button");
    renew.type = "button"; renew.textContent = "续费"; renew.dataset.cardAction = "renew"; renew.dataset.originalCode = item.code;
    const different = document.createElement("button");
    different.type = "button"; different.textContent = "兑换不同账号"; different.dataset.cardAction = "different"; different.dataset.originalCode = item.code;
    actions.append(query, renew, different, remove);
    row.append(details, actions);
    savedCardsList.append(row);
  }
}

async function generateTOTP(secret, counter = Math.floor(Date.now() / 1000 / 30)) {
  const key = base32Decode(secret);
  const buffer = new ArrayBuffer(8);
  const view = new DataView(buffer);
  view.setUint32(4, counter);
  const cryptoKey = await crypto.subtle.importKey("raw", key, { name: "HMAC", hash: "SHA-1" }, false, ["sign"]);
  const hmac = new Uint8Array(await crypto.subtle.sign("HMAC", cryptoKey, buffer));
  const offset = hmac[hmac.length - 1] & 0x0f;
  const binary = ((hmac[offset] & 0x7f) << 24) | ((hmac[offset + 1] & 0xff) << 16) | ((hmac[offset + 2] & 0xff) << 8) | (hmac[offset + 3] & 0xff);
  return String(binary % 1000000).padStart(6, "0");
}

function base32Decode(value) {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  const clean = value.toUpperCase().replace(/=+$/g, "").replace(/\s+/g, "");
  let bits = "";
  for (const char of clean) {
    const index = alphabet.indexOf(char);
    if (index < 0) throw new Error("invalid base32");
    bits += index.toString(2).padStart(5, "0");
  }
  const bytes = [];
  for (let i = 0; i + 8 <= bits.length; i += 8) {
    bytes.push(parseInt(bits.slice(i, i + 8), 2));
  }
  return new Uint8Array(bytes);
}
