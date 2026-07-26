<script setup>
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api/client.js'
const loading = ref(true); const saving = ref(false); const error = ref(''); const notice = ref(''); const settings = ref(null)
const form = reactive({ poll_interval: 3600, near_expiry_days: 3, secrets: { telegram: '', wecom: '', feishu: '' } })
const channels = [{ key: 'telegram', label: 'Telegram Bot' }, { key: 'wecom', label: '企业微信' }, { key: 'feishu', label: '飞书' }]
async function load() { loading.value = true; error.value = ''; try { settings.value = await api.settings(); form.poll_interval = settings.value.poll_interval; form.near_expiry_days = settings.value.near_expiry_days } catch (reason) { error.value = reason.message } finally { loading.value = false } }
async function save() { saving.value = true; error.value = ''; notice.value = ''; try { const channelBody = {}; for (const channel of channels) if (form.secrets[channel.key]) channelBody[channel.key] = { enabled: false, secret: form.secrets[channel.key] }; settings.value = await api.updateSettings({ poll_interval: Number(form.poll_interval), near_expiry_days: Number(form.near_expiry_days), ...(Object.keys(channelBody).length ? { channels: channelBody } : {}) }); for (const channel of channels) form.secrets[channel.key] = ''; notice.value = '配置已保存；渠道仍保持未接通。' } catch (reason) { error.value = reason.message } finally { saving.value = false } }
async function clearSecret(channel) { if (!window.confirm('清除该渠道的密钥配置？此操作需要重新填写才能恢复。')) return; try { await api.clearChannelSecret(channel); await load(); notice.value = '渠道密钥已清除。' } catch (reason) { error.value = reason.message } }
onMounted(load)
</script>
<template>
  <div class="dashboard-shell">
    <a class="skip-link" href="#main-content">跳到主要内容</a><header class="topbar">
      <RouterLink class="wordmark" to="/">
        <span class="wordmark-pulse" aria-hidden="true" /><span>Vitals</span>
      </RouterLink><nav aria-label="主导航">
        <RouterLink to="/">
          总览
        </RouterLink><RouterLink to="/import">
          导入账号
        </RouterLink><RouterLink to="/settings" aria-current="page">
          配置
        </RouterLink>
      </nav><div />
    </header><main id="main-content" class="dashboard-main form-page">
      <header class="page-intro">
        <div>
          <p class="eyebrow">
            MONITOR SETTINGS · PHASE 01
          </p><h1>监控与通知配置</h1><p>调整轮询节奏与临期阈值；一期通知渠道仅保留安全配置位。</p>
        </div>
      </header><div v-if="loading" class="state-panel" aria-busy="true">
        <h2>正在读取配置…</h2>
      </div><section v-else-if="error && !settings" class="state-panel error-panel" role="alert">
        <h2>配置读取中断</h2><p>{{ error }}</p><button type="button" @click="load">
          重新连接
        </button>
      </section><form v-else class="settings-form" @submit.prevent="save">
        <p v-if="error" class="form-alert" role="alert">
          {{ error }}
        </p><p v-if="notice" class="recovery-banner" role="status">
          {{ notice }}
        </p><section class="form-surface">
          <p class="section-index">
            01 / POLLING
          </p><h2>轮询参数</h2><div class="field-grid">
            <div><label for="poll-interval">轮询周期（秒）</label><input id="poll-interval" v-model.number="form.poll_interval" name="poll_interval" type="number" inputmode="numeric" min="900" max="86400"><small>范围 900–86400 秒，默认 3600 秒。</small></div><div><label for="near-days">临期阈值（天）</label><input id="near-days" v-model.number="form.near_expiry_days" name="near_expiry_days" type="number" inputmode="numeric" min="1" max="30"><small>范围 1–30 天，默认 3 天。</small></div>
          </div>
        </section><section class="form-surface">
          <p class="section-index">
            02 / CHANNEL SLOTS
          </p><h2>通知渠道配置位</h2><p class="scope-note">
            一期尚未接通任何真实渠道，所有 enabled 状态固定为 false，不会外发消息。
          </p><div class="channel-grid">
            <article v-for="channel in channels" :key="channel.key">
              <header><h3>{{ channel.label }}</h3><span>{{ settings.channels[channel.key].configured ? '已配置密钥' : '未配置' }}</span></header><label :for="`${channel.key}-secret`">新密钥（只写）</label><input :id="`${channel.key}-secret`" v-model="form.secrets[channel.key]" :name="`${channel.key}_secret`" type="password" autocomplete="new-password" placeholder="留空则不覆盖…"><label class="disabled-toggle"><input type="checkbox" disabled :checked="false"> 启用渠道（尚未接通）</label><button v-if="settings.channels[channel.key].configured" class="text-button" type="button" @click="clearSecret(channel.key)">
                清除已配置密钥
              </button>
            </article>
          </div>
        </section><button class="primary-action save-button" type="submit" :disabled="saving">
          <span>{{ saving ? '正在保存…' : '保存监控配置' }}</span><span aria-hidden="true">→</span>
        </button>
      </form>
    </main>
  </div>
</template>
