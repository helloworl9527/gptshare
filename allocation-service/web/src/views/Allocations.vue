<script setup>
import { computed, onMounted, ref } from 'vue'
import { api } from '../api/client.js'
import AdminShell from '../components/AdminShell.vue'
import StatePanel from '../components/StatePanel.vue'
import { derivedAllocations, formatDateTime } from '../lib/vitals.js'

const accounts = ref([])
const cards = ref([])
const loading = ref(true)
const error = ref('')
const state = ref('')

const allocations = computed(() => derivedAllocations(accounts.value, cards.value).filter((item) => !state.value || item.state === state.value))

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [accountResult, cardResult] = await Promise.all([api.accounts(), api.cards({ status: 'redeemed' })])
    accounts.value = accountResult.accounts || []
    cards.value = cardResult.cards || []
  } catch (reason) {
    error.value = reason.message || '分配记录暂时无法读取。'
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
        <p>基于已兑换卡密与账号容量生成当前分配运营视图。</p>
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
        正在读取分配记录...
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
                {{ allocation.account }}
              </td>
              <td><span class="status-badge status-redeemed">{{ allocation.state }}</span></td>
              <td>{{ formatDateTime(allocation.allocated_at) }}</td>
              <td>{{ formatDateTime(allocation.valid_until) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
    <section class="panel history-note" aria-labelledby="history-title">
      <p class="section-index">
        02 / REPLACEMENT HISTORY
      </p>
      <h2 id="history-title">
        替换历史
      </h2>
      <p>本步不新增后端历史接口；当前页面只展示可由已批准管理端 API 推导的 active 运营视图。完整长期替换历史按计划在后续自动替换步骤落地。</p>
    </section>
  </AdminShell>
</template>
