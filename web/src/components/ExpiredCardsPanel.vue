<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '../api/client.js'
import { formatDateTime } from '../lib/vitals.js'
import StatePanel from './StatePanel.vue'

const cards = ref([])
const total = ref(0)
const page = ref(1)
const pageSize = 20
const search = ref('')
const appliedSearch = ref('')
const loading = ref(true)
const error = ref('')
const pageCount = computed(() => Math.max(1, Math.ceil(total.value / pageSize)))
let generation = 0

const statusLabels = {
  available: '可分配', full: '已满', pending_credentials: '凭据待补全',
  expired: '账号已过期', banned: '已封禁', disabled: '已下线', unknown_monitor: '监控未知',
}

async function load(targetPage = page.value) {
  const requestGeneration = ++generation
  loading.value = true
  error.value = ''
  try {
    const result = await api.expiredCards({ page: targetPage, page_size: pageSize, search: appliedSearch.value })
    if (requestGeneration !== generation) return
    cards.value = result.cards || []
    total.value = Number(result.total || 0)
    page.value = Number(result.page || targetPage)
    if (page.value > pageCount.value) {
      await load(pageCount.value)
    }
  } catch (reason) {
    if (requestGeneration === generation) error.value = reason.message || '已过期卡密暂时无法读取。'
  } finally {
    if (requestGeneration === generation) loading.value = false
  }
}

function applySearch() {
  appliedSearch.value = search.value.trim()
  load(1)
}

onMounted(() => load())
onBeforeUnmount(() => { generation++ })
</script>

<template>
  <section class="panel table-panel expired-cards-panel" aria-labelledby="expired-cards-title">
    <div class="section-head">
      <div>
        <p class="section-index">
          DEVICE CLEANUP
        </p>
        <h2 id="expired-cards-title">
          近 3 天已过期卡密及对应账号 <small>共 {{ total }} 张</small>
        </h2>
      </div>
      <form class="controls" @submit.prevent="applySearch">
        <label for="expired-card-search">查找</label>
        <input id="expired-card-search" v-model="search" maxlength="256" placeholder="账号邮箱、卡密尾号或编号">
        <button class="nav-action" type="submit" :disabled="loading">
          搜索
        </button>
        <button class="refresh-button" type="button" :disabled="loading" @click="load()">
          刷新清单
        </button>
      </form>
    </div>
    <p class="expired-cleanup-note">
      仅显示最近 72 小时内已过期的卡密，列出其曾分配过的全部账号，包括换号前的旧账号。卡密到期不会自动退出已登录设备；请进入对应账号管理设备。仍有有效卡密的账号，清理设备时请注意其他用户。
    </p>
    <div v-if="loading" class="table-skeleton" aria-busy="true">
      正在读取已过期卡密…
    </div>
    <StatePanel v-else-if="error" type="error" title="过期清单读取失败" :message="error" action="重试" @action="load()" />
    <StatePanel v-else-if="cards.length === 0" title="近 3 天暂无已过期卡密" :message="appliedSearch ? '没有匹配的过期卡密，请调整查询条件。' : '这里只显示最近 72 小时内已过期的卡密。'" />
    <div v-else class="table-wrap">
      <table>
        <thead>
          <tr><th>卡密</th><th>到期时间</th><th>曾分配账号 / 当前有效卡密</th></tr>
        </thead>
        <tbody>
          <tr v-for="card in cards" :key="card.id" class="row-retired">
            <td class="mono-cell">
              **** {{ card.code_suffix }}<small class="expired-card-id">卡密 #{{ card.id }}</small>
            </td>
            <td>{{ formatDateTime(card.expires_at) }}</td>
            <td>
              <ul v-if="card.accounts.length" class="expired-account-list">
                <li v-for="account in card.accounts" :key="account.id">
                  <span class="mono-cell">{{ account.display_username || `账号 #${account.id}` }}</span>
                  <span class="status-badge" :class="`status-${account.status}`">{{ account.archived ? '已下线' : (statusLabels[account.status] || account.status) }}</span>
                  <small :class="{ 'cleanup-active-warning': account.active_card_count > 0 }">
                    {{ account.active_card_count > 0 ? `仍有 ${account.active_card_count} 张有效卡密` : '无有效卡密' }}
                  </small>
                  <small>最近分配：{{ formatDateTime(account.last_allocated_at) }}</small>
                </li>
              </ul>
              <span v-else class="muted-value">无分配记录</span>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <div v-if="!error && total > 0" class="expired-pagination" aria-label="过期卡密分页">
      <span>第 {{ page }} / {{ pageCount }} 页 · 共 {{ total }} 张</span>
      <button class="nav-action" type="button" :disabled="loading || page <= 1" @click="load(page - 1)">
        上一页
      </button>
      <button class="nav-action" type="button" :disabled="loading || page >= pageCount" @click="load(page + 1)">
        下一页
      </button>
    </div>
  </section>
</template>
