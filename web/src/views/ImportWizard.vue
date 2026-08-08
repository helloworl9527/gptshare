<script setup>
import { computed, nextTick, onBeforeUnmount, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api/client.js'
import { normalizeJSONBatch, normalizeLineBatch } from '../import-normalize.js'
import { describeOAuthError } from '../oauth-error.js'
import AdminShell from '../components/AdminShell.vue'

const route = useRoute()
const router = useRouter()
const mode = ref(new Set(['oauth', 'token', 'device']).has(route.query.mode) ? route.query.mode : 'oauth')
const reauthorizeID = computed(() => route.query.reauthorize || '')
const label = ref('')
const credentialType = ref('access_token')
const credential = ref('')
const tokenImportMode = ref('single')
const batchFormat = ref('lines')
const batchRaw = ref('')
const batchResult = ref(null)
const retryItems = ref([])
const groupedBatchResults = computed(() => {
  const groups = [
    { status: 'success', label: '成功' },
    { status: 'duplicate', label: '重复' },
    { status: 'invalid', label: '无效' },
    { status: 'upstream_unavailable', label: '上游暂不可用' },
    { status: 'failed', label: '其他失败' },
  ]
  return groups.map((group) => ({
    ...group,
    items: batchResult.value?.results.filter((item) => item.status === group.status) || [],
  })).filter((group) => group.items.length)
})
const batchPlaceholder = computed(() => batchFormat.value === 'lines'
  ? 'token-1\ntoken-2'
  : '[{"label":"账号 1","access_token":"…"}]')
const oauth = ref(null)
const callbackURL = ref('')
const loading = ref(false)
const generalError = ref('')
const oauthError = ref(null)
const oauthErrorElement = ref()
const callbackInput = ref()
const device = ref(null)
const deviceState = ref('')
let timer

function setMode(nextMode) {
  mode.value = nextMode
  generalError.value = ''
  oauthError.value = null
  router.replace({ query: { ...route.query, mode: nextMode } })
}

async function submitToken() {
  generalError.value = ''
  if (!credential.value.trim()) {
    generalError.value = '请粘贴一种凭证后再提交。'
    return
  }
  loading.value = true
  try {
    const body = { label: label.value, [credentialType.value]: credential.value }
    const account = reauthorizeID.value
      ? await api.reauthorizeToken(reauthorizeID.value, body)
      : await api.importToken(body)
    credential.value = ''
    await router.replace({ name: 'account-detail', params: { id: account.id } })
  } catch (reason) {
    generalError.value = reason.message
  } finally {
    credential.value = ''
    loading.value = false
  }
}

function normalizeBatch() {
  return batchFormat.value === 'lines'
    ? normalizeLineBatch(batchRaw.value, credentialType.value)
    : normalizeJSONBatch(batchRaw.value)
}

async function submitBatch(itemsOverride) {
  generalError.value = ''
  let items
  try {
    items = itemsOverride || normalizeBatch()
  } catch (reason) {
    generalError.value = reason.message
    return
  }
  batchRaw.value = ''
  loading.value = true
  try {
    const result = await api.importTokenBatch(items)
    batchResult.value = result
    retryItems.value = result.results
      .filter((item) => item.status !== 'success' && item.status !== 'duplicate')
      .map((item) => items[item.index])
      .filter(Boolean)
  } catch (reason) {
    generalError.value = reason.message
    retryItems.value = items
  } finally {
    loading.value = false
  }
}

async function startOAuth() {
  generalError.value = ''
  oauthError.value = null
  loading.value = true
  try {
    oauth.value = reauthorizeID.value
      ? await api.startOAuthReauthorization(reauthorizeID.value)
      : await api.startOAuth(label.value)
    window.open(oauth.value.authorization_url, '_blank', 'noopener,noreferrer')
  } catch (reason) {
    generalError.value = reason.message
  } finally {
    loading.value = false
  }
}

async function completeOAuth() {
  generalError.value = ''
  oauthError.value = null
  if (!callbackURL.value.trim()) {
    oauthError.value = describeOAuthError({ code: 'oauth_callback_invalid' })
    await focusOAuthError()
    return
  }
  loading.value = true
  try {
    const account = await api.completeOAuth(oauth.value.session_id, callbackURL.value)
    callbackURL.value = ''
    await router.replace({ name: 'account-detail', params: { id: account.id } })
  } catch (reason) {
    callbackURL.value = ''
    oauthError.value = describeOAuthError(reason)
    await focusOAuthError()
  } finally {
    callbackURL.value = ''
    loading.value = false
  }
}

async function focusOAuthError() {
  await nextTick()
  oauthErrorElement.value?.focus()
}

function focusCallbackInput() {
  callbackInput.value?.focus()
}

function reopenOAuth() {
  if (oauth.value?.authorization_url) {
    window.open(oauth.value.authorization_url, '_blank', 'noopener,noreferrer')
  }
}

async function restartOAuth() {
  callbackURL.value = ''
  oauthError.value = null
  oauth.value = null
  await startOAuth()
}

async function startDevice() {
  generalError.value = ''
  loading.value = true
  clearTimeout(timer)
  try {
    device.value = reauthorizeID.value
      ? await api.startDeviceReauthorization(reauthorizeID.value)
      : await api.startDevice(label.value)
    deviceState.value = 'pending'
    schedule(device.value.interval_seconds)
  } catch (reason) {
    generalError.value = reason.message
  } finally {
    loading.value = false
  }
}

function schedule(seconds) {
  clearTimeout(timer)
  timer = setTimeout(pollDevice, Math.max(1, seconds) * 1000)
}

async function pollDevice() {
  try {
    const result = await api.pollDevice(device.value.session_id)
    deviceState.value = result.state
    if (result.state === 'authorized') {
      await router.replace({ name: 'account-detail', params: { id: result.account.id } })
      return
    }
    if (result.state === 'expired') return
    schedule(result.retry_after_seconds || device.value.interval_seconds)
  } catch (reason) {
    generalError.value = reason.message
  }
}

onBeforeUnmount(() => clearTimeout(timer))
</script>

<template>
  <AdminShell>
    <div class="form-page">
      <header class="page-intro">
        <div>
          <p class="eyebrow">
            ACCOUNT INTAKE · SECURE FLOW
          </p>
          <h1>{{ reauthorizeID ? '重新授权账号' : '导入账号凭证' }}</h1>
          <p>临时授权信息只发送到后端，不写入浏览器存储。</p>
        </div>
      </header>
      <div class="tab-list" role="tablist" aria-label="导入方式">
        <button id="oauth-tab" type="button" role="tab" :aria-selected="mode === 'oauth'" aria-controls="oauth-panel" @click="setMode('oauth')">
          OAuth 授权
        </button>
        <button id="token-tab" type="button" role="tab" :aria-selected="mode === 'token'" aria-controls="token-panel" @click="setMode('token')">
          令牌直导
        </button>
        <button id="device-tab" type="button" role="tab" :aria-selected="mode === 'device'" aria-controls="device-panel" @click="setMode('device')">
          设备码授权
        </button>
      </div>
      <p v-if="generalError" class="form-alert" role="alert">
        {{ generalError }}
      </p>

      <section v-if="mode === 'oauth'" id="oauth-panel" class="form-surface" role="tabpanel" aria-labelledby="oauth-tab">
        <label v-if="!reauthorizeID" for="oauth-label">账号标签</label>
        <input v-if="!reauthorizeID" id="oauth-label" v-model="label" autocomplete="off" placeholder="例如：North Star…">
        <template v-if="!oauth">
          <p>请使用下方按钮生成并打开带完整参数的 OpenAI 官方授权页，不要手动打开裸的授权网址。完成登录后，本机回调页面可能无法打开，这是正常现象。</p>
          <button class="primary-action" type="button" :disabled="loading" @click="startOAuth">
            <span>{{ loading ? '正在生成…' : '打开 OAuth 授权页' }}</span><span aria-hidden="true">↗</span>
          </button>
        </template>
        <form v-else @submit.prevent="completeOAuth">
          <article v-if="oauthError" id="oauth-error" ref="oauthErrorElement" class="oauth-error" role="alert" tabindex="-1">
            <h2>{{ oauthError.title }}</h2>
            <p>{{ oauthError.detail }}</p>
            <dl class="oauth-error-reference">
              <div>
                <dt>错误码</dt>
                <dd>{{ oauthError.code }}</dd>
              </div>
              <div v-if="oauthError.requestId">
                <dt>请求编号</dt>
                <dd>{{ oauthError.requestId }}</dd>
              </div>
            </dl>
            <div v-if="oauthError.recovery !== 'contact_support'" class="oauth-error-actions">
              <button v-if="oauthError.recovery === 'retry_callback'" type="button" @click="focusCallbackInput">
                重新粘贴回调 URL
              </button>
              <button v-if="oauthError.recovery === 'retry_callback' || oauthError.recovery === 'reopen_authorization'" type="button" @click="reopenOAuth">
                重新打开当前授权页
              </button>
              <button v-if="oauthError.recovery === 'restart_authorization'" type="button" :disabled="loading" @click="restartOAuth">
                生成新的授权链接
              </button>
              <button v-if="oauthError.recovery === 'restart_original_account'" type="button" :disabled="loading" @click="restartOAuth">
                使用原账号重新授权
              </button>
            </div>
          </article>
          <p>授权完成后，复制浏览器地址栏中以 <code>http://localhost:1455/auth/callback</code> 开头的完整 URL。</p>
          <a class="button-link" :href="oauth.authorization_url" target="_blank" rel="noopener noreferrer">重新打开授权页</a>
          <label for="oauth-callback">完整回调 URL</label>
          <textarea id="oauth-callback" ref="callbackInput" v-model="callbackURL" rows="5" autocomplete="off" spellcheck="false" placeholder="http://localhost:1455/auth/callback?code=…&state=…" :aria-invalid="oauthError ? 'true' : undefined" :aria-describedby="oauthError ? 'oauth-error' : undefined" />
          <p class="privacy-note">
            提交后回调 URL 会立即清空；授权会话 15 分钟后过期。
          </p>
          <button class="primary-action" type="submit" :disabled="loading">
            <span>{{ loading ? '正在兑换并验证…' : '完成 OAuth 授权' }}</span><span aria-hidden="true">→</span>
          </button>
        </form>
      </section>

      <section v-else-if="mode === 'token'" id="token-panel" class="form-surface" role="tabpanel" aria-labelledby="token-tab">
        <div v-if="!reauthorizeID" class="segmented-control" role="group" aria-label="令牌导入数量">
          <button type="button" :aria-pressed="tokenImportMode === 'single'" @click="tokenImportMode = 'single'">
            单个导入
          </button>
          <button type="button" :aria-pressed="tokenImportMode === 'batch'" @click="tokenImportMode = 'batch'">
            批量导入
          </button>
        </div>
        <form v-if="reauthorizeID || tokenImportMode === 'single'" @submit.prevent="submitToken">
          <label v-if="!reauthorizeID" for="token-label">账号标签</label>
          <input v-if="!reauthorizeID" id="token-label" v-model="label" autocomplete="off" placeholder="例如：North Star…">
          <label for="credential-type">凭证类型</label>
          <select id="credential-type" v-model="credentialType">
            <option value="access_token">
              Access Token
            </option>
            <option value="refresh_token">
              Refresh Token
            </option>
            <option value="session_token">
              Session Token
            </option>
          </select>
          <label for="credential">凭证内容</label>
          <textarea id="credential" v-model="credential" rows="7" autocomplete="off" spellcheck="false" placeholder="粘贴凭证…" />
          <p class="privacy-note">
            提交完成后输入会立即清空；页面不会回显或持久保存凭证。
          </p>
          <button class="primary-action" type="submit" :disabled="loading">
            <span>{{ loading ? '正在验证…' : reauthorizeID ? '验证并更新授权' : '验证并导入账号' }}</span><span aria-hidden="true">→</span>
          </button>
        </form>
        <div v-else>
          <div class="segmented-control compact" role="group" aria-label="批量文本格式">
            <button type="button" :aria-pressed="batchFormat === 'lines'" @click="batchFormat = 'lines'">
              逐行令牌
            </button>
            <button type="button" :aria-pressed="batchFormat === 'json'" @click="batchFormat = 'json'">
              JSON 数组
            </button>
          </div>
          <label v-if="batchFormat === 'lines'" for="batch-type">统一凭证类型</label>
          <select v-if="batchFormat === 'lines'" id="batch-type" v-model="credentialType">
            <option value="access_token">
              Access Token
            </option>
            <option value="refresh_token">
              Refresh Token
            </option>
            <option value="session_token">
              Session Token
            </option>
          </select>
          <label for="batch-credentials">{{ batchFormat === 'lines' ? '每行一个凭证' : '凭证对象数组' }}</label>
          <textarea id="batch-credentials" v-model="batchRaw" rows="12" autocomplete="off" spellcheck="false" :placeholder="batchPlaceholder" />
          <p class="privacy-note">
            最多 50 项。提交时原始文本立即清空，成功项不会因其他项失败而回滚。
          </p>
          <button class="primary-action" type="button" :disabled="loading" @click="submitBatch()">
            <span>{{ loading ? '正在逐项验证…' : '开始批量导入' }}</span><span aria-hidden="true">→</span>
          </button>
          <article v-if="batchResult" class="batch-results" aria-live="polite">
            <h2>批量结果</h2>
            <p>共 {{ batchResult.total }} 项：成功 {{ batchResult.succeeded }}，失败 {{ batchResult.failed }}。</p>
            <section v-for="group in groupedBatchResults" :key="group.status" class="batch-result-group">
              <h3>{{ group.label }} · {{ group.items.length }}</h3>
              <ul>
                <li v-for="item in group.items" :key="item.index" :data-status="item.status">
                  #{{ item.index + 1 }}<span v-if="item.account"> · {{ item.account.label }}</span><span v-else-if="item.code"> · {{ item.code }}</span>
                </li>
              </ul>
            </section>
            <button v-if="retryItems.length" type="button" :disabled="loading" @click="submitBatch(retryItems)">
              仅重新提交失败项
            </button>
          </article>
        </div>
      </section>

      <section v-else id="device-panel" class="form-surface" role="tabpanel" aria-labelledby="device-tab">
        <label v-if="!reauthorizeID" for="device-label">账号标签</label>
        <input v-if="!reauthorizeID" id="device-label" v-model="label" autocomplete="off" placeholder="例如：Amber Lab…">
        <template v-if="!device">
          <p>生成一次性用户码后，在官方验证页完成授权；页面会按后端给出的间隔轮询。</p>
          <button class="primary-action" type="button" :disabled="loading" @click="startDevice">
            <span>{{ loading ? '正在生成…' : '生成设备授权码' }}</span><span aria-hidden="true">→</span>
          </button>
        </template>
        <article v-else class="device-code-card" aria-live="polite">
          <span>用户码</span><strong translate="no">{{ device.user_code }}</strong>
          <a :href="device.verify_url" target="_blank" rel="noopener noreferrer">打开官方验证页</a>
          <p>有效至 {{ new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short', timeZone: 'UTC' }).format(new Date(device.expires_at)) }}（UTC）</p>
          <p v-if="deviceState === 'slow_down'">
            服务要求降低轮询频率，已自动延长等待。
          </p>
          <p v-else-if="deviceState === 'expired'" role="alert">
            授权码已过期，请重新生成。
          </p>
          <p v-else>
            等待你完成授权…
          </p>
          <button v-if="deviceState === 'expired'" type="button" @click="device = null">
            重新开始
          </button>
        </article>
      </section>
    </div>
  </AdminShell>
</template>
