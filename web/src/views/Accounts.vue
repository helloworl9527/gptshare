<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api/client.js'
import AdminShell from '../components/AdminShell.vue'
import FocusModal from '../components/FocusModal.vue'
import StatePanel from '../components/StatePanel.vue'
import { accountTone, formatDateTime } from '../lib/vitals.js'

const accounts = ref([])
const loading = ref(true)
const error = ref('')
const notice = ref('')
const warnings = ref([])
const search = ref('')
const modal = ref('')
const busy = ref(false)
const editingAccount = ref(null)
const settings = ref({ default_account_capacity: 3 })
const defaultCapacity = ref(3)
const editForm = reactive({
  display_username: '',
  display_password: '',
  display_2fa_secret: '',
  source_url: '',
  account_expiry: '',
  max_concurrent_users: 3,
  status: 'available',
  monitor_status: 'unknown_monitor',
  monitor_account_id: '',
})

const visible = computed(() => {
  const term = search.value.trim().toLocaleLowerCase()
  return accounts.value.filter((item) => !term
    || String(item.display_username).toLocaleLowerCase().includes(term)
    || String(item.monitor_account_id || '').toLocaleLowerCase().includes(term)
    || String(item.source_url || '').toLocaleLowerCase().includes(term))
})

async function load() {
  loading.value = true
  error.value = ''
  try {
		const [result, settingsResult] = await Promise.all([api.allocationAccounts(), api.accountSettings()])
    accounts.value = result.accounts || []
    warnings.value = result.warnings || []
    settings.value = settingsResult.settings || settings.value
    defaultCapacity.value = settings.value.default_account_capacity || 3
  } catch (reason) {
    error.value = reason.message || '账号池暂时无法读取。'
  } finally {
    loading.value = false
  }
}

async function saveDefaultCapacity() {
  busy.value = true
  notice.value = ''
  try {
    const result = await api.updateAccountSettings({ default_account_capacity: Number(defaultCapacity.value) })
    settings.value = result.settings || settings.value
    defaultCapacity.value = settings.value.default_account_capacity
    notice.value = '默认容量已保存。'
  } catch (reason) {
    notice.value = reason.message
  } finally {
    busy.value = false
  }
}

async function applyDefaultCapacity() {
  if (!window.confirm('将默认容量应用到所有现有账号？已有分配不会被删除。')) return
  busy.value = true
  notice.value = ''
  try {
    const result = await api.applyDefaultCapacity()
    notice.value = `已应用到 ${result.updated_accounts || 0} 个账号。`
    await load()
  } catch (reason) {
    notice.value = reason.message
  } finally {
    busy.value = false
  }
}

async function updateAccount() {
  if (!editingAccount.value) return
  busy.value = true
  notice.value = ''
  try {
		await api.updateAllocationAccount(editingAccount.value.id, {
      ...editForm,
      account_expiry: new Date(editForm.account_expiry).toISOString(),
    })
    modal.value = ''
    editingAccount.value = null
    notice.value = '账号已更新。'
    await load()
  } catch (reason) {
    notice.value = reason.message
  } finally {
    busy.value = false
  }
}

async function remove(account) {
  if (busy.value) return
  if (!window.confirm(`确认下线账号 ${account.display_username}？仍有效的卡密分配将自动迁移到备用账号，旧账号凭证将被清除。`)) return
  busy.value = true
  notice.value = ''
  try {
    const result = await api.deleteAllocationAccount(account.id)
    notice.value = `账号已下线：迁移 ${result.replaced_allocations || 0} 个有效分配，结束 ${result.closed_allocations || 0} 个无效或宽限分配。`
    await load()
  } catch (reason) {
    notice.value = reason.message
  } finally {
    busy.value = false
  }
}

async function sync(account) {
  busy.value = true
  notice.value = ''
  try {
		await api.syncAllocationAccount(account.id)
    notice.value = '账号状态已同步。'
    await load()
  } catch (reason) {
    notice.value = reason.message
  } finally {
    busy.value = false
  }
}

async function syncAll() {
  busy.value = true
  notice.value = ''
  try {
		const result = await api.syncAllocationAccounts()
    accounts.value = result.accounts || accounts.value
    warnings.value = result.warnings || []
    notice.value = result.failed > 0 ? '一期状态同步部分降级，已保留本地业务状态。' : '一期状态已批量同步。'
    await load()
  } catch (reason) {
    notice.value = reason.message
  } finally {
    busy.value = false
  }
}

async function pullMonitorAccounts() {
  busy.value = true
  notice.value = ''
  try {
    const result = await api.pullMonitorAccounts()
    accounts.value = result.accounts || accounts.value
    notice.value = pullSyncNotice(result)
    await load()
  } catch (reason) {
    notice.value = reason.message
  } finally {
    busy.value = false
  }
}

function pullSyncNotice(result) {
  const summary = `一期同步完成：新建 ${result.created || 0}，更新 ${result.updated || 0}，跳过 ${result.skipped || 0}，失败 ${result.failed || 0}。`
  const details = (result.errors || []).map((item, index) => {
    const account = item.monitor_account_id || `第 ${index + 1} 项`
    return `${account}：${pullSyncErrorLabel(item.code)}`
  })
  return details.length ? `${summary} ${details.join('；')}` : summary
}

function pullSyncErrorLabel(code) {
  return ({
    alive_expiry_conflict: '状态为 alive，但到期时间已过',
    past_expiry_for_non_terminal_account: '非终态账号的到期时间已过',
    missing_monitor_account_id: '缺少一期账号 ID',
    missing_account_expiry: '缺少或无法识别到期时间',
    unsupported_monitor_status: '一期账号状态无法识别',
    duplicate_monitor_account: '一期列表包含重复账号',
    account_sync_failed: '账号写入失败',
  })[code] || `同步失败（${code || 'unknown_error'}）`
}

function openEdit(account) {
  editingAccount.value = account
  Object.assign(editForm, {
    display_username: account.display_username || '',
    display_password: '',
    display_2fa_secret: '',
    source_url: account.source_url || '',
    account_expiry: toLocalDatetime(account.account_expiry),
    max_concurrent_users: account.max_concurrent_users || 1,
    status: account.status || 'available',
    monitor_status: account.monitor_status || 'unknown_monitor',
    monitor_account_id: account.monitor_account_id || '',
  })
  modal.value = 'edit'
}

function toLocalDatetime(value) {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60000)
  return local.toISOString().slice(0, 16)
}

onMounted(load)
</script>

<template>
  <AdminShell>
    <section class="page-intro">
      <div>
        <p class="eyebrow">
          ACCOUNT POOL
        </p>
        <h1>账号池</h1>
        <p>管理二期独立账号库存、容量与一期同步状态。</p>
      </div>
      <button class="primary-action compact-action" type="button" :disabled="busy" @click="pullMonitorAccounts">
        从一期同步账号
      </button>
    </section>
    <p v-if="warnings.includes('phase_one_monitor_unavailable')" class="recovery-banner warning-banner" role="alert">
      一期监控暂时不可用，后台已保留本地业务状态并标记 monitor_unknown。
    </p>
    <p v-if="notice" class="recovery-banner" role="status">
      {{ notice }}
    </p>
    <section class="panel capacity-panel" aria-labelledby="capacity-settings-title">
      <div class="section-head compact">
        <div>
          <p class="section-index">
            00 / DEFAULT CAPACITY
          </p>
          <h2 id="capacity-settings-title">
            默认并发容量
          </h2>
        </div>
      </div>
      <form class="capacity-form" @submit.prevent="saveDefaultCapacity">
        <label for="default-capacity">默认容量</label>
        <input id="default-capacity" v-model.number="defaultCapacity" type="number" min="1" max="1000" required>
        <button class="refresh-button" type="submit" :disabled="busy">
          保存默认
        </button>
        <button class="refresh-button" type="button" :disabled="busy" @click="applyDefaultCapacity">
          应用到全部账号
        </button>
      </form>
    </section>
    <section class="panel table-panel" aria-labelledby="accounts-title">
      <div class="section-head">
        <div>
          <p class="section-index">
            01 / ACCOUNTS
          </p>
          <h2 id="accounts-title">
            账号列表
          </h2>
        </div>
        <div class="controls">
          <label for="account-search">搜索账号</label>
          <input id="account-search" v-model="search" type="search" autocomplete="off" placeholder="搜索账号、来源或监控 ID">
          <button class="refresh-button" type="button" :disabled="loading" @click="load">
            刷新
          </button>
          <button class="refresh-button" type="button" :disabled="busy" @click="syncAll">
            同步全部
          </button>
        </div>
      </div>
      <div v-if="loading" class="table-skeleton" aria-busy="true">
        正在读取账号池…
      </div>
      <StatePanel v-else-if="error" type="error" title="账号池读取失败" :message="error" action="重新连接" @action="load" />
      <StatePanel v-else-if="visible.length === 0" title="暂无账号" message="从一期同步账号后，补齐密码和 2FA 即可分配卡密。" action="从一期同步账号" @action="pullMonitorAccounts" />
      <div v-else class="table-wrap">
        <table>
          <thead><tr><th>账号</th><th>来源</th><th>状态</th><th>容量</th><th>到期</th><th>一期状态</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="account in visible" :key="account.id" :class="`row-${accountTone(account)}`">
              <td class="mono-cell">
                {{ account.display_username }}
              </td>
              <td>
                <a v-if="account.source_url" class="source-link" :href="account.source_url" target="_blank" rel="noopener noreferrer">打开来源</a>
                <span v-else class="muted-value">未填写</span>
              </td>
              <td><span class="status-badge" :class="`status-${account.status}`">{{ account.status }}</span></td>
              <td><meter min="0" :max="account.max_concurrent_users" :value="account.current_allocations" /> {{ account.current_allocations }}/{{ account.max_concurrent_users }}</td>
              <td>{{ formatDateTime(account.account_expiry) }}</td>
              <td class="mono-cell">
                {{ account.monitor_status }}
              </td>
              <td class="row-actions">
                <button type="button" @click="openEdit(account)">
                  编辑
                </button>
                <button type="button" @click="sync(account)">
                  同步
                </button>
                <button class="danger-button" type="button" :disabled="busy" @click="remove(account)">
                  下线
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <FocusModal v-if="modal === 'edit'" title="编辑账号" @close="modal = ''; editingAccount = null">
      <form class="modal-form" @submit.prevent="updateAccount">
        <label for="edit-display-username">邮箱</label>
        <input id="edit-display-username" v-model="editForm.display_username" required readonly autocomplete="off">
        <label for="edit-display-password">新密码</label>
        <input id="edit-display-password" v-model="editForm.display_password" type="password" autocomplete="new-password" placeholder="留空不修改">
        <label for="edit-display-totp">新 2FA Secret</label>
        <input id="edit-display-totp" v-model="editForm.display_2fa_secret" autocomplete="off" placeholder="留空不修改">
        <label for="edit-source-url">账号来源链接</label>
        <input id="edit-source-url" v-model="editForm.source_url" type="url" maxlength="2048" autocomplete="off" placeholder="https://…（留空清除）">
        <label for="edit-account-expiry">账号到期</label>
        <input id="edit-account-expiry" v-model="editForm.account_expiry" required type="datetime-local">
        <label for="edit-max-users">最大并发</label>
        <input id="edit-max-users" v-model.number="editForm.max_concurrent_users" required type="number" min="1" max="1000">
        <label for="edit-status">业务状态</label>
        <select id="edit-status" v-model="editForm.status">
          <option value="available">
            available
          </option>
          <option value="pending_credentials">
            pending_credentials
          </option>
          <option value="full">
            full
          </option>
          <option value="unknown_monitor">
            unknown_monitor
          </option>
          <option value="expired">
            expired
          </option>
          <option value="banned">
            banned
          </option>
          <option value="disabled">
            disabled
          </option>
        </select>
        <label for="edit-monitor-status">一期状态</label>
        <select id="edit-monitor-status" v-model="editForm.monitor_status">
          <option value="alive">
            alive
          </option>
          <option value="unknown">
            unknown
          </option>
          <option value="unknown_monitor">
            unknown_monitor
          </option>
          <option value="dead_normal">
            dead_normal
          </option>
          <option value="dead_banned">
            dead_banned
          </option>
          <option value="not_found">
            not_found
          </option>
        </select>
        <label for="edit-monitor-account-id">一期账号 ID</label>
        <input id="edit-monitor-account-id" v-model="editForm.monitor_account_id" autocomplete="off">
        <button class="primary-action" type="submit" :disabled="busy">
          保存修改
        </button>
      </form>
    </FocusModal>
  </AdminShell>
</template>
