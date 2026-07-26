<script setup>
import { computed, onBeforeUnmount, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api/client.js'
import AdminShell from '../components/AdminShell.vue'

const route = useRoute(); const router = useRouter()
const mode = ref(route.query.mode === 'device' ? 'device' : 'token')
const reauthorizeID = computed(() => route.query.reauthorize || '')
const label = ref(''); const credentialType = ref('access_token'); const credential = ref('')
const loading = ref(false); const error = ref(''); const device = ref(null); const deviceState = ref(''); let timer

function setMode(nextMode) {
  mode.value = nextMode
  router.replace({ query: { ...route.query, mode: nextMode } })
}

async function submitToken() {
  error.value = ''
  if (!credential.value.trim()) { error.value = '请粘贴至少一种凭证后再提交。'; return }
  loading.value = true
  try {
    const body = { label: label.value, [credentialType.value]: credential.value }
    const account = reauthorizeID.value ? await api.reauthorizeToken(reauthorizeID.value, body) : await api.importToken(body)
    credential.value = ''
    await router.replace({ name: 'account-detail', params: { id: account.id } })
  } catch (reason) { error.value = reason.message }
  finally { credential.value = ''; loading.value = false }
}

async function startDevice() {
  error.value = ''; loading.value = true; clearTimeout(timer)
  try {
    device.value = reauthorizeID.value ? await api.startDeviceReauthorization(reauthorizeID.value) : await api.startDevice(label.value)
    deviceState.value = 'pending'; schedule(device.value.interval_seconds)
  } catch (reason) { error.value = reason.message }
  finally { loading.value = false }
}

function schedule(seconds) { clearTimeout(timer); timer = setTimeout(pollDevice, Math.max(1, seconds) * 1000) }
async function pollDevice() {
  try {
    const result = await api.pollDevice(device.value.session_id); deviceState.value = result.state
    if (result.state === 'authorized') { await router.replace({ name: 'account-detail', params: { id: result.account.id } }); return }
    if (result.state === 'expired') return
    schedule(result.retry_after_seconds || device.value.interval_seconds)
  } catch (reason) { error.value = reason.message }
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
          </p><h1>{{ reauthorizeID ? '重新授权账号' : '导入账号凭证' }}</h1><p>凭证只发送到本机后端，不写入浏览器存储。</p>
        </div>
      </header>
      <div class="tab-list" role="tablist" aria-label="导入方式">
        <button id="token-tab" type="button" role="tab" :aria-selected="mode === 'token'" aria-controls="token-panel" @click="setMode('token')">
          令牌直导
        </button><button id="device-tab" type="button" role="tab" :aria-selected="mode === 'device'" aria-controls="device-panel" @click="setMode('device')">
          设备码授权
        </button>
      </div>
      <p v-if="error" class="form-alert" role="alert">
        {{ error }}
      </p>
      <section v-if="mode === 'token'" id="token-panel" class="form-surface" role="tabpanel" aria-labelledby="token-tab">
        <form @submit.prevent="submitToken">
          <label v-if="!reauthorizeID" for="token-label">账号标签</label><input v-if="!reauthorizeID" id="token-label" v-model="label" name="label" autocomplete="off" placeholder="例如：North Star…"><label for="credential-type">凭证类型</label><select id="credential-type" v-model="credentialType" name="credential_type">
            <option value="access_token">
              Access Token
            </option><option value="refresh_token">
              Refresh Token
            </option><option value="session_token">
              Session Token
            </option>
          </select><label for="credential">凭证内容</label><textarea id="credential" v-model="credential" name="credential" rows="7" autocomplete="off" spellcheck="false" placeholder="粘贴凭证…" /><p class="privacy-note">
            提交完成后输入会立即清空；页面不会回显或持久保存凭证。
          </p><button class="primary-action" type="submit" :disabled="loading">
            <span>{{ loading ? '正在验证…' : reauthorizeID ? '验证并更新授权' : '验证并导入账号' }}</span><span aria-hidden="true">→</span>
          </button>
        </form>
      </section>
      <section v-else id="device-panel" class="form-surface" role="tabpanel" aria-labelledby="device-tab">
        <label v-if="!reauthorizeID" for="device-label">账号标签</label><input v-if="!reauthorizeID" id="device-label" v-model="label" name="device_label" autocomplete="off" placeholder="例如：Amber Lab…"><template v-if="!device">
          <p>生成一次性用户码后，在官方验证页完成授权；页面会按后端给出的间隔轮询。</p><button class="primary-action" type="button" :disabled="loading" @click="startDevice">
            <span>{{ loading ? '正在生成…' : '生成设备授权码' }}</span><span aria-hidden="true">→</span>
          </button>
        </template><article v-else class="device-code-card" aria-live="polite">
          <span>用户码</span><strong translate="no">{{ device.user_code }}</strong><a :href="device.verify_url" target="_blank" rel="noopener noreferrer">打开官方验证页</a><p>有效至 {{ new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short', timeZone: 'UTC' }).format(new Date(device.expires_at)) }}（UTC）</p><p v-if="deviceState === 'slow_down'">
            服务要求降低轮询频率，已自动延长等待。
          </p><p v-else-if="deviceState === 'expired'" role="alert">
            授权码已过期，请重新生成。
          </p><p v-else>
            等待你完成授权…
          </p><button v-if="deviceState === 'expired'" type="button" @click="device = null">
            重新开始
          </button>
        </article>
      </section>
    </div>
  </AdminShell>
</template>
