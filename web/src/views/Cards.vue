<script setup>
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api/client.js'
import AdminShell from '../components/AdminShell.vue'
import FocusModal from '../components/FocusModal.vue'
import StatePanel from '../components/StatePanel.vue'
import { cardTone, formatDateTime } from '../lib/vitals.js'

const cards = ref([])
const status = ref('')
const loading = ref(true)
const error = ref('')
const notice = ref('')
const modal = ref('')
const busy = ref(false)
const generated = ref([])
const revealed = reactive({})
const form = reactive({ quantity: 10, duration_days: 30, format: 'csv', extend_days: 7, selected: null })

const visible = computed(() => cards.value)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const result = await api.cards({ status: status.value })
    cards.value = result.cards || []
  } catch (reason) {
    error.value = reason.message || '卡密库存暂时无法读取。'
  } finally {
    loading.value = false
  }
}

async function generate() {
  busy.value = true
  notice.value = ''
  try {
    const result = await api.generateCards({ quantity: form.quantity, duration_days: form.duration_days })
    generated.value = result.cards || []
    notice.value = `已生成 ${generated.value.length} 张卡密，明文仅本次可见。`
    await load()
  } catch (reason) {
    notice.value = reason.message
  } finally {
    busy.value = false
  }
}

async function copyGenerated() {
  const plaintext = generated.value.map((card) => card.code).join('\n')
  if (!plaintext || !navigator.clipboard?.writeText) {
    notice.value = '浏览器不支持自动复制，请手动选择卡密。'
    return
  }
  try {
    await navigator.clipboard.writeText(plaintext)
    notice.value = `已复制 ${generated.value.length} 张卡密，每行一个。`
  } catch {
    notice.value = '浏览器拒绝了复制请求，请手动选择卡密。'
  }
}

async function exportBatch() {
  busy.value = true
  notice.value = ''
  try {
    await api.exportCards({ quantity: form.quantity, duration_days: form.duration_days, format: form.format })
    notice.value = '导出请求已完成并写入审计。'
  } catch (reason) {
    notice.value = reason.message
  } finally {
    busy.value = false
  }
}

async function revoke(card) {
  busy.value = true
  notice.value = ''
  try {
    await api.revokeCard(card.id)
    notice.value = '卡密已作废，用户侧不可再查询对应账号。'
    await load()
  } catch (reason) {
    notice.value = reason.message
  } finally {
    busy.value = false
  }
}

async function reveal(card) {
  busy.value = true
  notice.value = ''
  try {
    const result = await api.revealCard(card.id)
    revealed[card.id] = result.code || result.message || '明文不可用(旧批次)'
    notice.value = result.code ? '卡密明文已显示。' : '明文不可用(旧批次)。'
  } catch (reason) {
    notice.value = reason.message
  } finally {
    busy.value = false
  }
}

function hideReveal(card) {
  delete revealed[card.id]
}

async function extend() {
  busy.value = true
  notice.value = ''
  try {
    await api.extendCard(form.selected.id, form.extend_days)
    modal.value = ''
    notice.value = '卡密有效期已延期。'
    await load()
  } catch (reason) {
    notice.value = reason.message
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>

<template>
  <AdminShell>
    <section class="page-intro">
      <div>
        <p class="eyebrow">
          CARD INVENTORY
        </p>
        <h1>卡密管理</h1>
        <p>生成、筛选、作废、延期和导出卡密批次。</p>
      </div>
      <div class="action-row">
        <button class="primary-action compact-action" type="button" @click="modal = 'generate'">
          批量生成
        </button>
        <button class="nav-action" type="button" @click="modal = 'export'">
          导出
        </button>
      </div>
    </section>
    <p class="selection-note">
      查看明文会写入安全审计；页面不会把卡密写入浏览器存储或日志。
    </p>
    <p v-if="notice" class="recovery-banner" role="status">
      {{ notice }}
    </p>
    <section v-if="generated.length" class="panel generated-panel" aria-label="本次生成明文卡密">
      <div class="generated-heading">
        <p class="section-index">
          ONE TIME PLAINTEXT
        </p>
        <button type="button" @click="copyGenerated">
          一键复制全部
        </button>
      </div>
      <div class="generated-list">
        <code v-for="card in generated" :key="card.id">{{ card.code }}</code>
      </div>
    </section>
    <section class="panel table-panel" aria-labelledby="cards-title">
      <div class="section-head">
        <div>
          <p class="section-index">
            01 / CARDS
          </p>
          <h2 id="cards-title">
            卡密列表
          </h2>
        </div>
        <div class="controls">
          <label for="card-status">状态筛选</label>
          <select id="card-status" v-model="status" @change="load">
            <option value="">
              全部
            </option>
            <option value="unused">
              未使用
            </option>
            <option value="redeemed">
              已兑换
            </option>
            <option value="revoked">
              已作废
            </option>
            <option value="expired">
              已过期
            </option>
          </select>
          <button class="refresh-button" type="button" :disabled="loading" @click="load">
            刷新
          </button>
        </div>
      </div>
      <div v-if="loading" class="table-skeleton" aria-busy="true">
        正在读取卡密库存…
      </div>
      <StatePanel v-else-if="error" type="error" title="卡密读取失败" :message="error" action="重新连接" @action="load" />
      <StatePanel v-else-if="visible.length === 0" title="暂无卡密" message="批量生成后即可开始兑换。" action="批量生成" @action="modal = 'generate'" />
      <div v-else class="table-wrap">
        <table>
          <thead><tr><th>卡密</th><th>时长</th><th>状态</th><th>兑换时间</th><th>到期</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="card in visible" :key="card.id" :class="`row-${cardTone(card)}`">
              <td class="mono-cell">
                <span v-if="revealed[card.id]" class="reveal-code">{{ revealed[card.id] }}</span>
                <span v-else>**** {{ card.code_suffix }}</span>
              </td>
              <td>{{ card.duration_days }} 天</td>
              <td><span class="status-badge" :class="`status-${card.status}`">{{ card.status }}</span></td>
              <td>{{ formatDateTime(card.redeemed_at) }}</td>
              <td>{{ formatDateTime(card.expires_at) }}</td>
              <td class="row-actions">
                <button v-if="revealed[card.id]" type="button" @click="hideReveal(card)">
                  隐藏
                </button>
                <button v-else type="button" :aria-label="`查看尾号 ${card.code_suffix} 的卡密明文`" @click="reveal(card)">
                  查看
                </button>
                <button type="button" :disabled="card.status !== 'redeemed'" @click="form.selected = card; modal = 'extend'">
                  延期
                </button>
                <button class="danger-button" type="button" :disabled="card.status === 'revoked' || card.status === 'expired'" @click="revoke(card)">
                  作废
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <FocusModal v-if="modal === 'generate' || modal === 'export'" :title="modal === 'generate' ? '批量生成卡密' : '导出卡密'" @close="modal = ''">
      <form class="modal-form" @submit.prevent="modal === 'generate' ? generate() : exportBatch()">
        <label for="quantity">数量</label>
        <input id="quantity" v-model.number="form.quantity" type="number" min="1" max="1000" required>
        <label for="duration">时长档位</label>
        <select id="duration" v-model.number="form.duration_days">
          <option :value="7">
            7 天
          </option><option :value="14">
            14 天
          </option><option :value="30">
            30 天
          </option><option :value="90">
            90 天
          </option>
        </select>
        <template v-if="modal === 'export'">
          <label for="format">导出格式</label>
          <select id="format" v-model="form.format">
            <option value="csv">
              CSV
            </option><option value="txt">
              TXT
            </option>
          </select>
        </template>
        <button class="primary-action" type="submit" :disabled="busy">
          {{ modal === 'generate' ? '生成卡密' : '导出文件' }}
        </button>
      </form>
    </FocusModal>
    <FocusModal v-if="modal === 'extend'" title="延期卡密" @close="modal = ''">
      <form class="modal-form" @submit.prevent="extend">
        <label for="extend-days">延期天数</label>
        <input id="extend-days" v-model.number="form.extend_days" type="number" min="1" max="365" required>
        <button class="primary-action" type="submit" :disabled="busy">
          确认延期
        </button>
      </form>
    </FocusModal>
  </AdminShell>
</template>
