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
let captchaId = 0;
let currentSecret = "";
let validUntil = null;

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
  const payload = { code: codeInput.value.trim() };
  if (captchaId) {
    payload.captcha_id = captchaId;
    payload.captcha_answer = answerInput.value.trim();
  }
  try {
    const redeem = await redeemCard(payload.code);
    let body = await queryCard(payload);
    let apiError = errorBody(body);
    if (!redeem.ok && !body.ok) {
      apiError = errorBody(body);
    }
    if (body.ok) {
      captchaId = 0;
      captchaBox.classList.remove("visible");
      answerInput.value = "";
      renderResult(body.result);
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
  document.querySelector("#totp-code").textContent = await generateTOTP(currentSecret);
  updateCountdowns();
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
    document.querySelector("#totp-code").textContent = await generateTOTP(currentSecret);
  } else {
    refreshButton.disabled = true;
    document.querySelector("#totp-code").textContent = "未提供";
    document.querySelector("#totp-timer").textContent = "请使用取件地址";
  }
  setCopyStatus("");
  updateCountdowns();
  resultPanel.classList.add("visible");
}

function setCopyStatus(message) {
  copyStatus.textContent = message;
}

function updateCountdowns() {
  const now = Date.now();
  document.querySelector("#totp-timer").textContent = currentSecret ? `${30 - Math.floor(now / 1000) % 30}s 后进入下一周期` : "请使用取件地址";
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

async function generateTOTP(secret) {
  const key = base32Decode(secret);
  const counter = Math.floor(Date.now() / 1000 / 30);
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
