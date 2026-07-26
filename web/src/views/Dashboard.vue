<script setup>
import { computed, onMounted, ref } from 'vue'
import AdminShell from '../components/AdminShell.vue'
import StatePanel from '../components/StatePanel.vue'
import { api } from '../api/client.js'
import { summarizeAllocation, summarizeMonitor } from '../lib/vitals.js'

const monitorAccounts = ref([])
const allocationAccounts = ref([])
const cards = ref([])
const inventory = ref({})
const loading = ref(true)
const error = ref('')
const recovered = ref(false)

const monitor = computed(() => summarizeMonitor(monitorAccounts.value))
const allocation = computed(() => summarizeAllocation(allocationAccounts.value, cards.value))
const pendingCredentials = computed(() => allocationAccounts.value.filter((item) => item.status === 'pending_credentials').length)
const capacity = computed(() => Number(inventory.value.capacity ?? allocation.value.capacity))
const available = computed(() => Number(inventory.value.available_capacity ?? allocation.value.availableCapacity))
const dailyRate = computed(() => Number.isFinite(Number(inventory.value.daily_redemption_rate)) ? Number(inventory.value.daily_redemption_rate).toFixed(2) : '—')

async function load() {
  const hadError = Boolean(error.value)
  loading.value = true
  error.value = ''
  try {
    const [monitorResult, dashboardResult, accountsResult, cardsResult] = await Promise.all([
      api.monitorAccounts(),
      api.allocationDashboard(),
      api.allocationAccounts(),
      api.cards(),
    ])
    monitorAccounts.value = monitorResult.accounts || []
    inventory.value = dashboardResult.dashboard || {}
    allocationAccounts.value = accountsResult.accounts || []
    cards.value = cardsResult.cards || []
    recovered.value = hadError
  } catch (reason) {
    error.value = reason.message || '统一运营体征暂时无法读取。'
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <AdminShell>
    <section class="dashboard-intro">
      <div>
        <p class="eyebrow">
          OPERATIONS VITALS · LIVE
        </p>
        <h1>运营总览</h1>
        <p>账号健康与业务库存的统一体征视图。</p>
      </div>
      <button class="refresh-button overview-refresh" type="button" :disabled="loading" @click="load">
        刷新数据
      </button>
    </section>

    <p v-if="recovered" class="recovery-banner" role="status">
      连接已恢复，两域体征已重新汇总。
    </p>
    <div v-if="loading" class="state-panel overview-state" aria-busy="true">
      <h2>正在汇总两域体征…</h2>
    </div>
    <StatePanel v-else-if="error" type="error" title="运营体征读取中断" :message="error" action="重新连接" @action="load" />
    <div v-else class="domain-grid">
      <section class="domain-panel" aria-labelledby="monitor-domain-title">
        <header class="domain-head">
          <h2 id="monitor-domain-title">
            账号健康
          </h2><span class="eyebrow">MONITOR</span>
        </header>
        <div class="domain-kpis">
          <article class="domain-kpi">
            <span>存活账号</span><strong>{{ monitor.alive }}</strong><small>{{ monitor.total }} 个账号纳入监控</small>
          </article>
          <article class="domain-kpi tone-red">
            <span>异常封号</span><strong>{{ monitor.banned }}</strong><small>正常到期不计入</small>
          </article>
          <article class="domain-kpi tone-amber">
            <span>临期账号</span><strong>{{ monitor.near }}</strong><small>接近订阅或授权边界</small>
          </article>
          <article class="domain-kpi tone-cyan">
            <span>平均封前存活</span><strong>{{ monitor.averageSurvival }}<small v-if="monitor.averageSurvival !== '—'"> 天</small></strong><small>{{ monitor.abnormalChecks }} 个检查异常</small>
          </article>
        </div>
        <RouterLink class="panel-link" to="/monitor/accounts">
          查看账号体征
        </RouterLink>
      </section>

      <section class="domain-panel" aria-labelledby="allocation-domain-title">
        <header class="domain-head">
          <h2 id="allocation-domain-title">
            业务库存
          </h2><span class="eyebrow">ALLOCATION</span>
        </header>
        <div class="domain-kpis">
          <article class="domain-kpi tone-amber">
            <span>库存预警</span><strong class="status-value">{{ inventory.warning_level || 'normal' }}</strong><small>预计 {{ inventory.days_to_exhaust ?? '—' }} 天耗尽</small>
          </article>
          <article class="domain-kpi">
            <span>可用容量</span><strong>{{ available }}<small>/{{ capacity }}</small></strong><small>剩余可分配并发位</small>
          </article>
          <article class="domain-kpi tone-cyan">
            <span>近 7 天兑换</span><strong>{{ inventory.redeemed_last_7_days ?? allocation.redeemed }}</strong><small>日均 {{ dailyRate }}</small>
          </article>
          <article class="domain-kpi" :class="{ 'tone-amber': pendingCredentials > 0 }">
            <span>待补全账号</span><strong>{{ pendingCredentials }}</strong><small>缺密码或 2FA，不参与兑换</small>
          </article>
        </div>
        <RouterLink class="panel-link" to="/allocation/accounts">
          管理账号池
        </RouterLink>
      </section>
    </div>
  </AdminShell>
</template>
