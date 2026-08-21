<script setup>
import { computed, onMounted, ref } from 'vue'
import { api } from '../api/client.js'
import AdminShell from '../components/AdminShell.vue'
import StatePanel from '../components/StatePanel.vue'
import { formatDateTime } from '../lib/vitals.js'

const records = ref([])
const history = ref([])
const loading = ref(true)
const error = ref('')
const state = ref('')
const reason = ref('')

const REASON_LABELS = {
  account_expiring: '账号临期',
  account_expired: '订阅终止',
  banned: '账号封禁',
  account_retired: '账号下线',
  grace_expired: '宽限期结束',
  grace_superseded: '宽限期被接替',
}

const REASON_BADGES = {
  banned: 'status-revoked',
  account_expiring: 'status-full',
  account_expired: 'status-expired',
  account_retired: 'status-expired',
}

const allocations = computed(() => records.value.filter((item) => !state.value || item.allocation_state === state.value))
const replacements = computed(() => history.value.filter((item) => !reason.value || item.reason === reason.value))

function reasonLabel(value) {
  return REASON_LABELS[value] || value
}

function reasonBadge(value) {
  return REASON_BADGES[value] || 'status-redeemed'
}

function accountLabel(name, id, gone) {
  const shown = name || `#${id}`
  return gone ? `${shown}（已下线）` : shown
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const result = await api.allocations()
    records.value = result.allocations || []
    history.value = result.replacements || []
  } catch (reasonError) {
    error.value = reasonError.message || '分配记录暂时无法读取。'
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <AdminShell>
    <section class="page-intro">
      <div>
        <p class="eyebrow">
          ALLOCATIONS
        </p>
        <h1>分配记录</h1>
        <p>展示数据库中当前生效的卡密与账号分配关系，以及历次换号流水。</p>
      </div>
      <button class="refresh-button" type="button" :disabled="loading" @click="load">
        刷新
      </button>
    </section>
    <section class="panel table-panel" aria-labelledby="allocations-title">
      <div class="section-head">
        <div>
          <p class="section-index">
            01 / ACTIVE RECORDS
          </p>
          <h2 id="allocations-title">
            当前分配
          </h2>
        </div>
        <div class="controls">
          <label for="allocation-state">状态筛选</label>
          <select id="allocation-state" v-model="state">
            <option value="">
              全部
            </option><option value="primary">
              Primary
            </option><option value="grace">
              Grace
            </option>
          </select>
        </div>
      </div>
      <div v-if="loading" class="table-skeleton" aria-busy="true">
        正在读取分配记录…
      </div>
      <StatePanel v-else-if="error" type="error" title="分配读取失败" :message="error" action="重新连接" @action="load" />
      <StatePanel v-else-if="allocations.length === 0" title="暂无分配" message="用户兑换卡密后，这里会出现当前记录。" />
      <div v-else class="table-wrap">
        <table>
          <thead><tr><th>卡密</th><th>账号</th><th>状态</th><th>分配时间</th><th>有效至</th></tr></thead>
          <tbody>
            <tr v-for="allocation in allocations" :key="allocation.id" class="row-green">
              <td class="mono-cell">
                **** {{ allocation.code_suffix }}
              </td>
              <td class="mono-cell">
                {{ allocation.display_username }}
              </td>
              <td><span class="status-badge status-redeemed">{{ allocation.allocation_state }}</span></td>
              <td>{{ formatDateTime(allocation.allocated_at) }}</td>
              <td>{{ formatDateTime(allocation.valid_until) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
    <section class="panel table-panel" aria-labelledby="history-title">
      <div class="section-head">
        <div>
          <p class="section-index">
            02 / REPLACEMENT HISTORY
          </p>
          <h2 id="history-title">
            替换历史
          </h2>
        </div>
        <div class="controls">
          <label for="replacement-reason">原因筛选</label>
          <select id="replacement-reason" v-model="reason">
            <option value="">
              全部
            </option><option value="account_expiring">
              账号临期
            </option><option value="account_expired">
              订阅终止
            </option><option value="banned">
              账号封禁
            </option><option value="account_retired">
              账号下线
            </option>
          </select>
        </div>
      </div>
      <div v-if="loading" class="table-skeleton" aria-busy="true">
        正在读取替换历史…
      </div>
      <StatePanel v-else-if="error" type="error" title="替换历史读取失败" :message="error" action="重新连接" @action="load" />
      <StatePanel
        v-else-if="replacements.length === 0"
        title="暂无替换记录"
        message="账号临期或被封禁触发换号后，这里会记录每一次迁移。"
      />
      <div v-else class="table-wrap">
        <table>
          <thead><tr><th>卡密</th><th>原换账号</th><th>新账号</th><th>原因</th><th>换号时间</th><th>宽限期至</th><th>触发方</th></tr></thead>
          <tbody>
            <tr v-for="item in replacements" :key="item.id" class="row-green">
              <td class="mono-cell">
                **** {{ item.code_suffix }}
              </td>
              <td class="mono-cell">
                {{ accountLabel(item.old_account_name, item.old_account_id, item.old_account_gone) }}
              </td>
              <td class="mono-cell">
                {{ accountLabel(item.new_account_name, item.new_account_id, item.new_account_gone) }}
              </td>
              <td><span class="status-badge" :class="reasonBadge(item.reason)">{{ reasonLabel(item.reason) }}</span></td>
              <td>{{ formatDateTime(item.replaced_at) }}</td>
              <td>{{ item.grace_until ? formatDateTime(item.grace_until) : '即时切换' }}</td>
              <td>{{ item.operator === 'system' ? '系统自动' : '管理员' }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
  </AdminShell>
</template>
