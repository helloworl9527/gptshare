<script setup>
import { computed, nextTick, onBeforeUnmount, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, fetchCSRF } from '../api/client.js'

const router = useRouter()
const route = useRoute()
const step = ref('password')
const username = ref('admin')
const password = ref('')
const code = ref('')
const challenge = ref('')
const busy = ref(false)
const error = ref(route.query.reason === 'expired' ? '登录状态已失效，请重新验证。' : '')
const errorElement = ref()
const secondsLeft = ref(0)
const lockedFor = ref(0)
let timer

const actionLabel = computed(() => busy.value ? '正在验证…' : step.value === 'password' ? '验证密码' : '完成登录')
const challengeHint = computed(() => secondsLeft.value > 0 ? `验证码挑战将在 ${secondsLeft.value} 秒后过期` : '验证码挑战已过期')

async function submit() {
  if (busy.value || lockedFor.value > 0) return
  error.value = ''
  busy.value = true
  try {
    if (step.value === 'password') {
      await fetchCSRF()
      const result = await api.password(username.value.trim(), password.value)
      challenge.value = result.challenge
      password.value = ''
      step.value = 'totp'
      startTimer(result.expires_in || 120)
      requestAnimationFrame(() => document.querySelector('#totp-code')?.focus())
    } else {
      if (secondsLeft.value <= 0) {
        resetPasswordStep('验证码挑战已过期，请重新输入密码。')
        return
      }
      await api.totp(challenge.value, code.value)
      await fetchCSRF()
      const target = typeof route.query.redirect === 'string' ? route.query.redirect : '/'
      await router.replace(target)
    }
  } catch (reason) {
    if (reason.retryAfter > 0) startLock(reason.retryAfter)
    error.value = reason.message || '验证未完成，请稍后重试。'
    code.value = ''
    await nextTick()
    errorElement.value?.focus()
  } finally {
    busy.value = false
  }
}

function startTimer(seconds) {
  clearInterval(timer)
  secondsLeft.value = seconds
  timer = setInterval(() => {
    secondsLeft.value = Math.max(0, secondsLeft.value - 1)
    if (secondsLeft.value === 0) clearInterval(timer)
  }, 1000)
}

function startLock(seconds) {
  clearInterval(timer)
  lockedFor.value = seconds
  timer = setInterval(() => {
    lockedFor.value = Math.max(0, lockedFor.value - 1)
    if (lockedFor.value === 0) clearInterval(timer)
  }, 1000)
}

function resetPasswordStep(message = '') {
  clearInterval(timer)
  step.value = 'password'
  challenge.value = ''
  code.value = ''
  secondsLeft.value = 0
  error.value = message
}

onBeforeUnmount(() => clearInterval(timer))
</script>

<template>
  <main class="login-shell">
    <section class="login-brand" aria-labelledby="brand-title">
      <div class="brand-mark" aria-hidden="true">
        <span /><span /><span />
      </div>
      <p class="eyebrow">
        UNIFIED OPERATIONS
      </p>
      <h1 id="brand-title">
        Vitals
      </h1>
      <p class="brand-copy">
        统一查看账号监控、业务库存、卡密交付与安全配置边界。
      </p>
      <div class="signal-preview" aria-label="存活状态脉搏线示意">
        <span>LIVE SIGNAL</span>
        <svg viewBox="0 0 360 80" role="img" aria-label="绿色搏动线表示账号存活">
          <g class="login-signal-motion">
            <polyline points="0,45 85,45 102,45 114,14 130,68 146,34 158,45 250,45 265,45 277,25 291,55 305,45 360,45" />
          </g>
        </svg>
      </div>
    </section>

    <section class="login-panel" aria-labelledby="login-title">
      <div class="login-card">
        <div class="step-index" aria-label="登录进度">
          <span :class="{ active: step === 'password', done: step === 'totp' }">01</span>
          <i />
          <span :class="{ active: step === 'totp' }">02</span>
        </div>
        <p class="eyebrow">
          SECURE ADMIN ACCESS
        </p>
        <h2 id="login-title">
          {{ step === 'password' ? '管理员登录' : '二次验证' }}
        </h2>
        <p class="form-intro">
          {{ step === 'password' ? '输入管理员凭据，继续进行动态验证码验证。' : '输入验证器中当前显示的 6 位动态验证码。' }}
        </p>

        <p v-if="error" ref="errorElement" class="form-alert" role="alert" tabindex="-1">
          {{ error }}
        </p>
        <p v-if="lockedFor > 0" class="lock-hint" role="status">
          安全锁定中，请在 {{ lockedFor }} 秒后重试。
        </p>

        <form @submit.prevent="submit">
          <template v-if="step === 'password'">
            <label for="username">管理员账号</label>
            <input id="username" v-model="username" name="username" autocomplete="username" spellcheck="false" required maxlength="256">
            <label for="password">密码</label>
            <input id="password" v-model="password" name="password" type="password" autocomplete="current-password" required maxlength="1024">
          </template>
          <template v-else>
            <label for="totp-code">动态验证码</label>
            <input id="totp-code" v-model="code" class="totp-input" name="one-time-code" inputmode="numeric" autocomplete="one-time-code" spellcheck="false" pattern="[0-9]{6}" maxlength="6" required aria-describedby="challenge-hint">
            <p id="challenge-hint" class="challenge-hint" :class="{ expired: secondsLeft === 0 }">
              {{ challengeHint }}
            </p>
            <button class="text-button" type="button" @click="resetPasswordStep()">
              返回密码验证
            </button>
          </template>
          <button class="primary-action" type="submit" :disabled="busy || lockedFor > 0 || (step === 'totp' && (code.length !== 6 || secondsLeft === 0))">
            {{ actionLabel }} <span aria-hidden="true">→</span>
          </button>
        </form>
        <p class="security-note">
          <span aria-hidden="true">●</span> 会话仅由 Secure HttpOnly Cookie 保存，页面不会读取或存储令牌。
        </p>
      </div>
    </section>
  </main>
</template>
