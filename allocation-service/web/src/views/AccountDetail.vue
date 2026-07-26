<script setup>
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api/client.js'

const route = useRoute()
const router = useRouter()
const account = ref(null)
const loading = ref(true)
const error = ref('')
const recovered = ref(false)
const notice = ref('')
const working = ref(false)
const confirming = ref(false)
const cancelButton = ref(null)

const statusLabel = computed(() => ({ alive: account.value?.near_expiry ? '临期存活' : '存活', dead_banned: '异常封号', dead_normal: '正常退役' })[account.value?.status] || '未知')
const checkAbnormal = computed(() => ['error', 'verification_required', 'contract_changed'].includes(account.value?.last_check_state))

function dateTime(value) {
  if (!value) return '—'
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short', timeZone: 'UTC' }).format(new Date(value))
}

async function load() {
  loading.value = true
  const hadError = Boolean(error.value)
  error.value = ''
  try {
    account.value = await api.account(route.params.id)
    recovered.value = hadError
  } catch (reason) {
    error.value = reason.message || '账号详情暂时无法读取，请重试。'
  } finally { loading.value = false }
}

async function refreshNow() {
  working.value = true
  notice.value = ''
  try {
    const run = await api.refreshAccount(route.params.id)
    if (run.state === 'running') notice.value = `刷新已进入后台，运行编号 ${run.id}`
    else { notice.value = '刷新完成，当前快照已更新。'; await load() }
  } catch (reason) {
    notice.value = reason.status === 409 ? '该账号正在刷新，请稍后再查看。' : reason.message
  } finally { working.value = false }
}

async function remove() {
  working.value = true
  try { await api.removeAccount(route.params.id); await router.replace('/') }
  catch (reason) { notice.value = reason.message; confirming.value = false }
  finally { working.value = false }
}

onMounted(load)
watch(confirming, async (open) => { if (open) { await nextTick(); cancelButton.value?.focus() } })
</script>

<template>
  <div class="dashboard-shell">
    <a class="skip-link" href="#main-content">跳到主要内容</a>
    <header class="topbar">
      <RouterLink class="wordmark" to="/" aria-label="Vitals 总览">
        <span class="wordmark-pulse" aria-hidden="true" /><span>Vitals</span>
      </RouterLink>
      <nav aria-label="主导航">
        <RouterLink to="/">
          总览
        </RouterLink><RouterLink to="/import">
          导入账号
        </RouterLink><RouterLink to="/settings">
          配置
        </RouterLink>
      </nav>
      <div class="topbar-actions">
        <RouterLink class="nav-action" to="/">
          返回总览
        </RouterLink>
      </div>
    </header>
    <main id="main-content" class="dashboard-main detail-main">
      <div v-if="loading" class="state-panel" aria-busy="true">
        <h1>正在读取账号快照…</h1>
      </div>
      <section v-else-if="error" class="state-panel error-panel" role="alert">
        <span class="state-icon" aria-hidden="true">!</span><h1>详情读取中断</h1><p>{{ error }}</p><button type="button" @click="load">
          重新连接
        </button>
      </section>
      <template v-else-if="account">
        <header class="page-intro">
          <div>
            <p class="eyebrow">
              ACCOUNT VITALS · CURRENT SNAPSHOT
            </p><h1>{{ account.label || account.provider_account_id }}</h1><p class="mono-value" translate="no">
              {{ account.provider_account_id }}
            </p><p class="mono-value" translate="no">
              {{ account.email || '—' }}
            </p>
          </div>
          <span class="snapshot-badge" :class="`snapshot-${account.status}`">{{ statusLabel }}</span>
        </header>
        <div v-if="recovered" class="recovery-banner" role="status">
          连接已恢复，当前快照已重新载入。
        </div>
        <div v-if="notice" class="recovery-banner" role="status">
          {{ notice }}
        </div>
        <div v-if="checkAbnormal" class="warning-banner" role="status">
          上次检查异常；下方业务状态保留最近一次可信结果。
        </div>
        <section class="triad-grid" aria-label="订阅三态">
          <article><span>订阅类型</span><strong translate="no">{{ account.plan?.toUpperCase() || 'UNKNOWN' }}</strong></article>
          <article><span>订阅到期</span><strong>{{ dateTime(account.current_expiry || account.auth_expiry) }}</strong></article>
          <article><span>存活状态</span><strong>{{ statusLabel }}</strong></article>
        </section>
        <section class="detail-grid">
          <article class="detail-panel">
            <p class="section-index">
              01 / SURVIVAL
            </p><h2>本周期存活明细</h2><p class="scope-note">
              “本周期授权/导入时间”是当前授权周期起点；若账号曾重新授权，它不同于后端封前存活统计使用的初次导入基准。
            </p>
            <dl class="detail-list">
              <div>
                <dt>账号邮箱</dt><dd translate="no">
                  {{ account.email || '—' }}
                </dd>
              </div><div><dt>本周期授权/导入时间</dt><dd>{{ dateTime(account.last_authorized_at) }}</dd></div><div><dt>最后确认存活</dt><dd>{{ dateTime(account.last_alive_at) }}</dd></div><div><dt>失效时间</dt><dd>{{ dateTime(account.dead_at) }}</dd></div><div><dt>失效类型</dt><dd>{{ account.death_type === 'abnormal_ban' ? '异常封号' : account.death_type === 'normal_expiry' ? '正常到期' : '—' }}</dd></div><div><dt>封前存活天数</dt><dd>{{ account.death_type === 'abnormal_ban' && account.banned_survival_days != null ? `${Math.round(account.banned_survival_days)} 天` : '—' }}</dd></div>
            </dl>
          </article>
          <article class="detail-panel">
            <p class="section-index">
              02 / AUTHORIZATION
            </p><h2>令牌与授权</h2><dl class="detail-list">
              <div>
                <dt>凭证类型</dt><dd translate="no">
                  {{ account.credential?.type?.toUpperCase() || 'UNKNOWN' }}
                </dd>
              </div><div><dt>凭证状态</dt><dd>{{ account.credential?.configured ? '已加密配置' : '未配置' }}</dd></div><div><dt>授权边界</dt><dd>{{ dateTime(account.auth_expiry) }}</dd></div><div><dt>轮询状态</dt><dd>{{ account.polling_paused ? '已暂停' : '运行中' }}</dd></div>
            </dl><p class="privacy-note">
              这里只显示类型与配置状态，不显示令牌片段、密文或密钥标识。
            </p>
          </article>
        </section>
        <section class="action-panel" aria-labelledby="actions-title">
          <div>
            <p class="section-index">
              03 / ACTIONS
            </p><h2 id="actions-title">
              账号操作
            </h2>
          </div><div class="action-row">
            <button type="button" :disabled="working" @click="refreshNow">
              {{ working ? '处理中…' : '立即刷新' }}
            </button><RouterLink class="button-link" :to="{ name: 'import', query: { reauthorize: account.id, mode: 'token' } }">
              令牌重新授权
            </RouterLink><RouterLink class="button-link" :to="{ name: 'import', query: { reauthorize: account.id, mode: 'device' } }">
              设备码重新授权
            </RouterLink><button class="danger-button" type="button" @click="confirming = true">
              移除账号
            </button>
          </div>
        </section>
        <div v-if="confirming" class="modal-backdrop" role="presentation" @click.self="confirming = false" @keydown.esc="confirming = false">
          <section class="confirm-dialog" role="dialog" aria-modal="true" aria-labelledby="remove-title">
            <h2 id="remove-title">
              移除这个账号？
            </h2><p>系统会清除保存的凭证并停止监控。此操作不能撤销。</p><div class="action-row">
              <button ref="cancelButton" type="button" @click="confirming = false">
                保留账号
              </button><button class="danger-button" type="button" :disabled="working" @click="remove">
                确认移除
              </button>
            </div>
          </section>
        </div>
      </template>
    </main>
  </div>
</template>
