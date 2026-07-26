<script setup>
import { onMounted, reactive, ref } from 'vue'
import AdminShell from '../components/AdminShell.vue'
import StatePanel from '../components/StatePanel.vue'
import { api } from '../api/client.js'

const loading = ref(true)
const saving = ref(false)
const error = ref('')
const notice = ref('')
const settings = ref(null)
const boundaries = ref([])
const form = reactive({ poll_interval: 3600, near_expiry_days: 3, secrets: { telegram: '', wecom: '', feishu: '' } })
const channels = [{ key: 'telegram', label: 'Telegram Bot' }, { key: 'wecom', label: '企业微信' }, { key: 'feishu', label: '飞书' }]
const groupLabels = {
  unified_admin_auth: ['统一管理员认证', '仅用于管理员密码、会话、CSRF 与 TOTP'],
  monitor_data_encryption: ['监控数据加密', '仅用于监控 token 与账号凭证'],
  allocation_data_encryption: ['分配数据加密', '仅用于分配凭证与卡密明文 reveal'],
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [settingsResult, boundaryResult] = await Promise.all([api.settings(), api.securityBoundaries()])
    settings.value = settingsResult
    boundaries.value = boundaryResult.groups || []
    form.poll_interval = settings.value.poll_interval
    form.near_expiry_days = settings.value.near_expiry_days
  } catch (reason) {
    error.value = reason.message || '配置暂时无法读取。'
  } finally { loading.value = false }
}

async function save() {
  saving.value = true
  error.value = ''
  notice.value = ''
  try {
    const channelBody = {}
    for (const channel of channels) if (form.secrets[channel.key]) channelBody[channel.key] = { enabled: false, secret: form.secrets[channel.key] }
    settings.value = await api.updateSettings({ poll_interval: Number(form.poll_interval), near_expiry_days: Number(form.near_expiry_days), ...(Object.keys(channelBody).length ? { channels: channelBody } : {}) })
    for (const channel of channels) form.secrets[channel.key] = ''
    notice.value = '监控配置已保存；认证与数据加密边界未改变。'
  } catch (reason) { error.value = reason.message } finally { saving.value = false }
}

async function clearSecret(channel) {
  if (!window.confirm('清除该渠道的密钥配置？此操作需要重新填写才能恢复。')) return
  try { await api.clearChannelSecret(channel); await load(); notice.value = '渠道密钥已清除。' } catch (reason) { error.value = reason.message }
}

onMounted(load)
</script>

<template>
  <AdminShell>
    <section class="page-intro">
      <div>
        <p class="eyebrow">
          SECURITY BOUNDARIES · SETTINGS
        </p><h1>配置</h1><p>认证状态与两模块数据加密保持明确分组，页面永不读取密钥材料。</p>
      </div>
    </section>
    <div v-if="loading" class="state-panel" aria-busy="true">
      <h2>正在读取配置…</h2>
    </div>
    <StatePanel v-else-if="error && !settings" type="error" title="配置读取中断" :message="error" action="重新连接" @action="load" />
    <template v-else>
      <section class="security-boundaries" aria-labelledby="boundary-title">
        <div class="section-head compact">
          <div>
            <p class="section-index">
              01 / KEY BOUNDARIES
            </p><h2 id="boundary-title">
              密钥用途边界
            </h2>
          </div>
        </div>
        <div class="boundary-grid">
          <article v-for="group in boundaries" :key="group.id" class="boundary-item">
            <h3>{{ groupLabels[group.id]?.[0] || group.id }}</h3>
            <p>{{ groupLabels[group.id]?.[1] || group.purpose }}</p>
            <ul>
              <li v-for="name in group.configuration" :key="name">
                <code>{{ name }}</code>
              </li>
            </ul>
            <span class="boundary-lock">仅显示变量名 · 密钥材料不可见</span>
          </article>
        </div>
      </section>
      <form class="settings-form" @submit.prevent="save">
        <p v-if="error" class="form-alert" role="alert">
          {{ error }}
        </p><p v-if="notice" class="recovery-banner" role="status">
          {{ notice }}
        </p>
        <section class="form-surface">
          <p class="section-index">
            02 / MONITOR POLLING
          </p><h2>监控轮询参数</h2><div class="field-grid">
            <div><label for="poll-interval">轮询周期（秒）</label><input id="poll-interval" v-model.number="form.poll_interval" name="poll_interval" type="number" autocomplete="off" min="900" max="86400"><small>范围 900-86400 秒。</small></div><div><label for="near-days">临期阈值（天）</label><input id="near-days" v-model.number="form.near_expiry_days" name="near_expiry_days" type="number" autocomplete="off" min="1" max="30"><small>范围 1-30 天。</small></div>
          </div>
        </section>
        <section class="form-surface">
          <p class="section-index">
            03 / MONITOR CHANNELS
          </p><h2>通知渠道配置位</h2><p class="scope-note">
            渠道密钥只写不读，且不属于统一认证或分配数据密钥。
          </p><div class="channel-grid">
            <article v-for="channel in channels" :key="channel.key">
              <header><h3>{{ channel.label }}</h3><span>{{ settings.channels[channel.key].configured ? '已配置' : '未配置' }}</span></header><label :for="`${channel.key}-secret`">新密钥（只写）</label><input :id="`${channel.key}-secret`" v-model="form.secrets[channel.key]" :name="`${channel.key}_secret`" type="password" autocomplete="new-password" placeholder="留空不覆盖"><button v-if="settings.channels[channel.key].configured" class="text-button" type="button" @click="clearSecret(channel.key)">
                清除已配置密钥
              </button>
            </article>
          </div>
        </section>
        <button class="primary-action save-button" type="submit" :disabled="saving">
          {{ saving ? '正在保存…' : '保存监控配置' }}<span aria-hidden="true">→</span>
        </button>
      </form>
    </template>
  </AdminShell>
</template>
