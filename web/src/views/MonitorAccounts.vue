<script setup>
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import AdminShell from '../components/AdminShell.vue'
import StatePanel from '../components/StatePanel.vue'
import VitalCard from '../components/VitalCard.vue'
import VitalsPanel from '../components/VitalsPanel.vue'
import { api } from '../api/client.js'
import { sortAccounts, summarizeMonitor } from '../lib/vitals.js'

const router = useRouter()
const accounts = ref([])
const loading = ref(true)
const error = ref('')
const recovered = ref(false)
const filter = ref('all')
const sortMode = ref('status')
const query = ref('')

const summary = computed(() => summarizeMonitor(accounts.value))
const suspectCount = computed(() => accounts.value.filter(
  (account) => account.suspected_banned_at && account.status !== 'dead_banned' && account.status !== 'dead_normal',
).length)
const kpis = computed(() => [
  { key: 'alive', label: '存活账号', value: summary.value.alive, hint: `${summary.value.total} 个账号纳入监控`, tone: 'green' },
  { key: 'near', label: '临期账号', value: summary.value.near, hint: '接近订阅或授权边界', tone: summary.value.near ? 'amber' : 'retired' },
  { key: 'suspect', label: '疑似封号', value: suspectCount.value, hint: '已退出分配池，待人工确认', tone: suspectCount.value ? 'amber' : 'green' },
  { key: 'banned', label: '异常封号', value: summary.value.banned, hint: `平均封前存活 ${summary.value.averageSurvival} 天`, tone: summary.value.banned ? 'red' : 'green' },
  { key: 'checks', label: '检查异常', value: summary.value.abnormalChecks, hint: '业务状态保留最近可信值', tone: summary.value.abnormalChecks ? 'amber' : 'cyan' },
])

const filteredAccounts = computed(() => {
  const term = query.value.trim().toLowerCase()
  return sortAccounts(accounts.value, sortMode.value).filter((account) => {
    const state = account.status === 'dead_banned'
      ? 'banned'
      : account.status === 'dead_normal'
        ? 'retired'
        : account.suspected_banned_at
          ? 'suspect'
          : account.near_expiry ? 'near' : 'alive'
    const matchesFilter = filter.value === 'all' || filter.value === state
    const haystack = `${account.label || ''} ${account.provider_account_id || ''} ${account.email || ''}`.toLowerCase()
    return matchesFilter && (!term || haystack.includes(term))
  })
})

async function load() {
  const hadError = Boolean(error.value)
  loading.value = true
  error.value = ''
  try {
    const result = await api.monitorAccounts()
    accounts.value = result.accounts || []
    recovered.value = hadError
    if (hadError) setTimeout(() => { recovered.value = false }, 2800)
  } catch (reason) {
    error.value = reason.message || '账号体征暂时无法读取。'
  } finally {
    loading.value = false
  }
}

function selectAccount(account) {
  router.push({ name: 'account-detail', params: { id: account.id } })
}

onMounted(load)
</script>

<template>
  <AdminShell>
    <section class="dashboard-intro">
      <div>
        <p class="eyebrow">
          ACCOUNT VITALS · LIVE MONITOR
        </p>
        <h1>账号体征</h1>
        <p>订阅状态、封禁信号与授权边界的实时体征视图。</p>
      </div>
      <div class="topbar-actions">
        <RouterLink class="nav-action" to="/monitor/import">
          导入账号
        </RouterLink>
        <button class="refresh-button" type="button" :disabled="loading" @click="load">
          <span>刷新</span>
        </button>
      </div>
    </section>

    <div v-if="recovered" class="recovery-banner" role="status">
      连接已恢复，账号体征已重新同步。
    </div>
    <div v-if="loading" class="vitals-panel loading-grid" aria-busy="true" aria-label="正在加载 KPI">
      <article v-for="index in 4" :key="index" class="kpi-card skeleton-card">
        <span /><span /><span />
      </article>
    </div>
    <StatePanel v-else-if="error" type="error" title="体征读取中断" :message="error" action="重新连接" @action="load" />
    <template v-else>
      <VitalsPanel :items="kpis" />
      <section class="accounts-section">
        <div class="section-head">
          <div>
            <p class="section-index">
              01 / ACCOUNT SIGNALS
            </p>
            <h2>账号体征卡片</h2>
          </div>
          <div class="controls">
            <label for="account-search">搜索账号</label>
            <input id="account-search" v-model="query" type="search" placeholder="邮箱、标签或账号 ID" aria-label="搜索账号">
            <label for="state-filter">状态筛选</label>
            <select id="state-filter" v-model="filter" aria-label="状态筛选">
              <option value="all">
                全部状态
              </option>
              <option value="alive">
                存活
              </option>
              <option value="near">
                临期
              </option>
              <option value="suspect">
                疑似封号
              </option>
              <option value="banned">
                封号
              </option>
              <option value="retired">
                正常退役
              </option>
            </select>
            <label for="sort-mode">排序</label>
            <select id="sort-mode" v-model="sortMode" aria-label="排序">
              <option value="status">
                按风险排序
              </option>
              <option value="expiry">
                按到期时间
              </option>
              <option value="email">
                按邮箱
              </option>
            </select>
          </div>
        </div>
        <StatePanel v-if="filteredAccounts.length === 0" title="暂无账号体征" message="导入账号或调整筛选条件后，这里会显示监控卡片。" />
        <div v-else class="card-grid">
          <VitalCard v-for="account in filteredAccounts" :key="account.id" :account="account" @select="selectAccount" />
        </div>
      </section>
    </template>
  </AdminShell>
</template>
