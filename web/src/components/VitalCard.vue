<script setup>
import { computed } from 'vue'
import PulseLine from './PulseLine.vue'
import { monitorCheckIssue } from '../lib/vitals.js'

const props = defineProps({ account: { type: Object, required: true } })
const emit = defineEmits(['select'])

const visualState = computed(() => {
  if (props.account.status === 'dead_banned') return 'banned'
  if (props.account.status === 'dead_normal') return 'retired'
  return props.account.near_expiry ? 'near' : 'alive'
})
const stateLabel = computed(() => ({ alive: '存活', near: '临期', banned: '封号', retired: '正常退役' })[visualState.value])
const stateCode = computed(() => ({ alive: 'ALIVE', near: 'EXPIRING', banned: 'BANNED', retired: 'RETIRED' })[visualState.value])
const checkAbnormal = computed(() => ['error', 'verification_required', 'contract_changed'].includes(props.account.last_check_state))
const checkIssue = computed(() => checkAbnormal.value ? monitorCheckIssue(props.account) : null)

function formatDate(value) {
  if (!value) return '—'
  return new Intl.DateTimeFormat('zh-CN', { timeZone: 'UTC', year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date(value))
}
</script>

<template>
  <article class="vital-card" :class="`state-${visualState}`">
    <button class="card-action" type="button" :aria-label="`${account.label || account.provider_account_id}，${stateLabel}，查看摘要`" @click="emit('select', account)" />
    <header>
      <div>
        <p class="account-id">
          {{ account.label || account.provider_account_id }}
        </p>
        <p v-if="account.label" class="provider-id" translate="no">
          {{ account.provider_account_id }}
        </p>
        <p class="account-email" translate="no" :title="account.email || '—'">
          {{ account.email || '—' }}
        </p>
      </div>
      <span class="state-badge"><i aria-hidden="true" />{{ stateLabel }}</span>
    </header>
    <PulseLine :state="visualState" />
    <div class="signal-meta">
      <span>SIGNAL</span><strong translate="no">{{ stateCode }}</strong>
      <span v-if="checkAbnormal" class="check-warning" :title="checkIssue?.title || '检查异常'">{{ checkIssue?.badge || '检查异常' }}</span>
    </div>
    <dl class="vital-stats">
      <div><dt>订阅类型</dt><dd>{{ account.plan?.toUpperCase() || 'UNKNOWN' }}</dd></div>
      <div><dt>订阅到期</dt><dd>{{ formatDate(account.current_expiry || account.auth_expiry) }}</dd></div>
      <div>
        <dt>账号邮箱</dt><dd :title="account.email || '—'">
          {{ account.email || '—' }}
        </dd>
      </div>
    </dl>
    <footer>
      <span :title="checkIssue?.detail">{{ checkIssue?.summary || (checkAbnormal ? '保留上次业务状态' : '状态已同步') }}</span>
      <span aria-hidden="true">↗</span>
    </footer>
  </article>
</template>
