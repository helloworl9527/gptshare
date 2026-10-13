<script setup>
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '../api/client.js'
import { formatDateTime } from '../lib/vitals.js'
import FocusModal from './FocusModal.vue'
import StatePanel from './StatePanel.vue'

const accounts = ref([])
const loading = ref(true)
const error = ref('')
const notice = ref('')
const selected = ref(null)
const clearing = ref(false)
const actionError = ref('')
let generation = 0

async function load() {
  if (clearing.value) return
  const requestGeneration = ++generation
  loading.value = true
  error.value = ''
  notice.value = ''
  selected.value = null
  try {
    const result = await api.passwordChangeDue()
    if (requestGeneration === generation) accounts.value = result.accounts || []
  } catch (reason) {
    if (requestGeneration === generation) error.value = reason.message || '待更改密码清单暂时无法读取。'
  } finally {
    if (requestGeneration === generation) loading.value = false
  }
}

function confirm(account) {
  if (loading.value || clearing.value) return
  selected.value = account
  actionError.value = ''
  notice.value = ''
}

function close() {
  if (!clearing.value) selected.value = null
}

async function markCleared() {
  if (!selected.value || clearing.value) return
  const account = selected.value
  const requestGeneration = generation
  clearing.value = true
  actionError.value = ''
  try {
    await api.clearPasswordChange(account.id)
    if (requestGeneration !== generation) return
    accounts.value = accounts.value.filter((item) => item.id !== account.id)
    selected.value = null
    notice.value = `已记录 ${account.display_username || `账号 #${account.id}`} 的密码处理状态。`
  } catch (reason) {
    if (requestGeneration === generation) actionError.value = reason.message || '标记失败，请重试。'
  } finally {
    clearing.value = false
  }
}

defineExpose({ reload: load })
onMounted(load)
onBeforeUnmount(() => { generation++ })
</script>

<template>
  <section class="panel password-panel" aria-labelledby="password-due-title">
    <div class="section-head compact">
      <div>
        <p class="section-index">
          PASSWORD CHANGE
        </p>
        <h2 id="password-due-title">
          待更改密码 <small v-if="!loading && !error">· {{ accounts.length }} 个账号</small>
        </h2>
      </div>
      <button class="nav-action" type="button" :disabled="loading || clearing" @click="load">
        刷新密码清单
      </button>
    </div>
    <p class="password-note">
      显示已有卡密到期、且账号订阅仍剩余超过 3 天的未处理账号。请手动修改密码并检查已登录设备；如有其他有效卡密，请注意其他用户，并同步更新账号池凭据。下方标记仅记录处理状态，不会修改密码或自动登出设备。
    </p>
    <p v-if="notice" class="recovery-banner" role="status">
      {{ notice }}
    </p>
    <div v-if="loading" class="table-skeleton" aria-busy="true">
      正在读取待更改密码清单…
    </div>
    <StatePanel v-else-if="error" type="error" title="密码清单读取失败" :message="error" action="重试" @action="load" />
    <StatePanel v-else-if="accounts.length === 0" title="暂无待更改密码账号" message="当前没有符合条件的未处理账号；处理后若有新的卡密到期，会再次提醒。" />
    <ul v-else class="password-list">
      <li v-for="item in accounts" :key="item.id" class="password-row">
        <div class="password-meta">
          <strong>{{ item.display_username || `账号 #${item.id}` }}</strong>
          <span>账号剩余 {{ item.days_account_remains }} 天 · 已过期卡密 {{ item.expired_card_count }} 张 · 最近卡密已过期 {{ item.days_expired_card_expired_ago }} 天</span>
          <span>账号到期：{{ formatDateTime(item.account_expiry) }} · 最近卡密到期：{{ formatDateTime(item.last_expired_card_expires_at) }}</span>
        </div>
        <button class="nav-action" type="button" :disabled="clearing" @click="confirm(item)">
          标记已更改密码
        </button>
      </li>
    </ul>
    <FocusModal v-if="selected" title="确认已更改密码" @close="close">
      <p>请确认已手动处理 {{ selected.display_username || `账号 #${selected.id}` }} 的密码及设备安全，并同步更新账号池凭据。</p>
      <p>此操作仅记录处理时间并移出提醒清单，不会实际修改密码或登出设备。后续卡密再到期时会重新提醒。</p>
      <p v-if="actionError" class="warning-banner" role="alert">
        {{ actionError }}
      </p>
      <div class="action-row">
        <button type="button" :disabled="clearing" @click="close">
          取消
        </button>
        <button type="button" :disabled="clearing" @click="markCleared">
          {{ clearing ? '正在记录…' : '确认已处理' }}
        </button>
      </div>
    </FocusModal>
  </section>
</template>

<style scoped>
.password-panel { margin-top: 28px; border-left: 4px solid var(--amber); }
.password-note { color: var(--muted); font-size: 13px; line-height: 1.7; }
.password-list { display: grid; gap: 10px; margin: 18px 0 0; padding: 0; list-style: none; }
.password-row { display: flex; flex-wrap: wrap; gap: 12px; align-items: center; justify-content: space-between; background: #f7faf9; padding: 12px 14px; }
.password-meta { display: grid; gap: 6px; min-width: 0; }
.password-meta strong { font: 700 13px var(--mono); overflow-wrap: anywhere; }
.password-meta span { color: var(--muted); font-size: 12px; line-height: 1.6; overflow-wrap: anywhere; }
.password-row .nav-action { flex: none; }
</style>
