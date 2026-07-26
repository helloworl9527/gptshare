<script setup>
import { computed, onMounted, ref } from 'vue'
import { api } from '../api/client.js'
import AdminShell from '../components/AdminShell.vue'
import StatePanel from '../components/StatePanel.vue'
import VitalsPanel from '../components/VitalsPanel.vue'
import { cardTone, daysUntil, formatDate, summarize } from '../lib/vitals.js'

const accounts = ref([])
const cards = ref([])
const dashboard = ref(null)
const loading = ref(true)
const error = ref('')
const recovered = ref(false)

const summary = computed(() => summarize(accounts.value, cards.value))
const metrics = computed(() => dashboard.value || {
  warning_level: 'safe',
  warning_label: '安全',
  available_capacity: summary.value.availableCapacity,
  capacity: summary.value.capacity,
  used: summary.value.used,
  daily_redemption_rate: 0,
  redeemed_last_7_days: 0,
  recommended_account_add: 0,
  days_to_exhaust_label: '∞',
})
const kpis = computed(() => [
  { key: 'health', label: '库存状态', value: metrics.value.warning_label, hint: `${metrics.value.available_capacity}/${metrics.value.capacity} 可用容量 · ${metrics.value.days_to_exhaust_label} 天`, tone: warningTone(metrics.value.warning_level) },
  { key: 'capacity', label: '可用容量', value: metrics.value.available_capacity, hint: `${metrics.value.used}/${metrics.value.capacity} 已分配`, tone: metrics.value.available_capacity === 0 ? 'red' : 'green' },
  { key: 'rate', label: '7日速率', value: Number(metrics.value.daily_redemption_rate || 0).toFixed(1), hint: `${metrics.value.redeemed_last_7_days || 0} 次 / 7 天`, tone: 'cyan' },
  { key: 'recommend', label: '建议补充', value: metrics.value.recommended_account_add || 0, hint: '补足安全库存', tone: metrics.value.recommended_account_add ? 'amber' : 'retired' },
])

const accountBuckets = computed(() => [
  { label: '0-7 天', count: accounts.value.filter((item) => between(daysUntil(item.account_expiry), 0, 7)).length },
  { label: '8-14 天', count: accounts.value.filter((item) => between(daysUntil(item.account_expiry), 8, 14)).length },
  { label: '15-30 天', count: accounts.value.filter((item) => between(daysUntil(item.account_expiry), 15, 30)).length },
])

const trend = computed(() => cards.value.filter((item) => item.redeemed_at).slice(0, 8).map((item) => ({
  id: item.id,
  suffix: item.code_suffix,
  date: formatDate(item.redeemed_at),
  tone: cardTone(item),
})))

async function load() {
  const hadError = Boolean(error.value)
  loading.value = true
  error.value = ''
  try {
    const [dashboardResult, accountResult, cardResult] = await Promise.all([api.dashboard(), api.accounts(), api.cards()])
    dashboard.value = dashboardResult.dashboard || null
    accounts.value = accountResult.accounts || []
    cards.value = cardResult.cards || []
    recovered.value = hadError
    if (hadError) setTimeout(() => { recovered.value = false }, 2800)
  } catch (reason) {
    error.value = reason.message || '后台数据暂时无法读取。'
  } finally {
    loading.value = false
  }
}

function between(value, min, max) {
  return value !== null && value >= min && value <= max
}

function warningTone(level) {
  if (level === 'exhausted' || level === 'urgent') return 'red'
  if (level === 'notice') return 'amber'
  return 'green'
}

onMounted(load)
</script>

<template>
  <AdminShell>
    <section class="dashboard-intro">
      <div>
        <p class="eyebrow">
          ALLOCATION VITALS · LIVE OVERVIEW
        </p>
        <h1>管理员后台</h1>
        <p>库存健康度、账号容量与卡密兑换趋势的统一视图。</p>
      </div>
      <button class="refresh-button" type="button" :disabled="loading" @click="load">
        刷新数据
      </button>
    </section>

    <div v-if="recovered" class="recovery-banner" role="status">
      连接已恢复，后台数据已重新同步。
    </div>
    <div v-if="loading" class="vitals-panel loading-grid" aria-busy="true" aria-label="正在加载 KPI">
      <article v-for="index in 4" :key="index" class="kpi-card skeleton-card">
        <span /><span /><span />
      </article>
    </div>
    <StatePanel v-else-if="error" type="error" title="后台读取中断" :message="error" action="重新连接" @action="load" />
    <template v-else>
      <VitalsPanel :items="kpis" />
      <section class="dashboard-grid" aria-label="库存分析">
        <article class="panel health-panel">
          <div class="section-head compact">
            <div>
              <p class="section-index">
                01 / ACCOUNT POOL
              </p>
              <h2>账号池时间分布</h2>
            </div>
          </div>
          <div class="bucket-list">
            <div v-for="bucket in accountBuckets" :key="bucket.label" class="bucket-row">
              <span>{{ bucket.label }}</span>
              <meter min="0" :max="Math.max(1, accounts.length)" :value="bucket.count" />
              <strong>{{ bucket.count }}</strong>
            </div>
          </div>
        </article>
        <article class="panel trend-panel">
          <div class="section-head compact">
            <div>
              <p class="section-index">
                02 / REDEMPTION TREND
              </p>
              <h2>兑换趋势</h2>
            </div>
          </div>
          <StatePanel v-if="trend.length === 0" title="暂无兑换记录" message="生成并兑换卡密后，这里会显示最近趋势。" />
          <ol v-else class="timeline">
            <li v-for="item in trend" :key="item.id" :class="`tone-${item.tone}`">
              <span>{{ item.date }}</span><strong>**** {{ item.suffix }}</strong>
            </li>
          </ol>
        </article>
      </section>
    </template>
  </AdminShell>
</template>
